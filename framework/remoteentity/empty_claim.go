package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// emptyClaimMetric 是空认领的累积计数（D17）：event="left" 是本进程的写准入新插入了第一代所有权记录、而这个实体最终没有
// 提交落进权威的次数（留下一条空认领）；event="deleted" 是运维“删空认领”删掉的条数。两者之差不是现存数：留下的空认领
// 之后可能被同 ID 的首写填上，进程崩溃在提交前留下的不计入 left。现存数用 remote_entity.empty_claims 命令列出。
const emptyClaimMetric = "remote_entity.empty_claim_total"

// ErrEmptyClaimUnsupported 表示所有权存储不支持删空认领（不是持久权威，或没有 EmptyClaimStore 能力，例如 Redis marker
// 装配）。
var ErrEmptyClaimUnsupported = errors.New("remote_entity: the ownership store cannot delete empty claims")

// EmptyClaimOutcome 是一次 DeleteEmptyClaim 的结论。除 EmptyClaimDeleted 外都是“什么也没删”。
type EmptyClaimOutcome uint8

const (
	// EmptyClaimDeleted：删掉了。
	EmptyClaimDeleted EmptyClaimOutcome = iota + 1
	// EmptyClaimNotEmpty：不是空认领——权威版本大于 0（有过提交，含删过档）、内存里的实例有版本，或冷加载读到了聚合 /
	// 墓碑。
	EmptyClaimNotEmpty
	// EmptyClaimNotOwned：不归本 sid 独占——没有所有权记录、owner 是别的 sid，或在共享模式。
	EmptyClaimNotOwned
	// EmptyClaimChanged：检查之后、删除之前权威记录变了（有提交落进来、所有权代际变了、记录被换掉），比较删除没有命中。
	EmptyClaimChanged
)

func (o EmptyClaimOutcome) String() string {
	switch o {
	case EmptyClaimDeleted:
		return "deleted"
	case EmptyClaimNotEmpty:
		return "not_empty"
	case EmptyClaimNotOwned:
		return "not_owned"
	case EmptyClaimChanged:
		return "changed"
	default:
		return "unknown"
	}
}

// emptyClaimCheckedHook 只给测试用：在 DeleteEmptyClaim 判定“是空认领”之后、发出比较删除之前调用（持写门与所有权写锁），
// 用来在这个窗口里改权威记录或发起并发写，证明代际守护。生产为 nil。
var emptyClaimCheckedHook func(id int64)

// ListEmptyClaims 按 ID 升序列出本 sid 独占、kind 在 kinds 里、权威记录看起来是空认领（这条所有权记录下没有提交落进
// 权威）的实体，一页至多 min(limit, OwnedListPageLimit) 个；next 为 0 表示到底。结果是查询时刻的候选，逐个仍由
// DeleteEmptyClaim 在写门下复核。要 I/O，只能在 I/O 段调用。
func (m *Manager) ListEmptyClaims(ctx context.Context, kinds []entity.EntityKind, after int64, limit int) ([]int64, int64, error) {
	if err := fctx.BlockingError("remoteentity.ListEmptyClaims"); err != nil {
		return nil, 0, err
	}
	store, err := m.emptyClaimStore()
	if err != nil {
		return nil, 0, err
	}
	if len(kinds) == 0 || limit <= 0 {
		return nil, 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := m.ownershipContext(ctx)
	defer cancel()
	return store.ListEmptyClaims(ctx, m.localSid, kinds, after, min(limit, OwnedListPageLimit))
}

// DeleteEmptyClaim 是“删空认领”的运维入口（维护者 2026-11-14 D17 A）：删除本 sid 独占的实体 id 的所有权记录，前提是它
// 是空认领——这条所有权记录下从没有提交落进权威，本进程也没有在途的提交。
//
// 契约：
//
//   - 只由 owner 进程删自己的：与认领、转移、进出共享一样经 beginOwnershipTransition，持实体写门与所有权写锁，与本进程
//     的写准入、所有权变更串行。写门覆盖一笔写从准入到拿到持久结论的全程（WAL 级由 finalizer 持有到投影确认，
//     Durability 0 结果未知时持有到写下结论），所以持有写门时本进程没有这个实体的在途提交。
//   - 判定（全部成立才删）：权威记录是本 sid、独占、_ver 为 0、不是墓碑；本进程内存里的实例（如果有）版本为 0——业务
//     GetOrCreate 新建、事务被拒绝后留在内存里的空实例照样算空认领；内存里没有实例时经 loader 冷加载确认“权威里没有
//     聚合”（entity.ErrAuthorityEntityNotFound，DataEngine 聚合仓库在读库之前会等这个实体的在途投影，覆盖同 sid 重启后
//     重放中的 WAL 记录）。
//   - 删除是比较删除（EmptyClaimStore.DeleteEmptyClaimExpected）：只有记录仍是判定时那一代（owner、非共享、
//     _owner_epoch、_owner_route）、仍是 _ver 0 且不是墓碑时才删。判定之后有提交落进来（例如重放的投影）、所有权被别的
//     进程改了（单实例前提被打破时的重叠）、或记录被删掉又由别的 sid 重新认领，都不会被删，答 EmptyClaimChanged。
//   - 删除之后：本进程的所有权缓存置为“没有记录”、清掉对齐记录、按值删路由登记，计
//     remote_entity.empty_claim_total{event="deleted"}。内存里的空实例留在原处（版本 0，与业务刚新建时相同）：之后同 ID
//     的写准入重新首写认领（第一代记录，代际从 1 重新开始），路由前置检查答“不存在”（没有记录、不认领）。
//   - 代际重新从 1 开始是安全的：被删的那一代下没有任何提交，也就没有带它的回执、快照、outbox 记录或副本；路由登记
//     已按值删掉。
//   - 比较删除的结果未知（Mongo 错误）时在独立的有界 ctx 上重读权威：记录已不在按已删收尾，仍在则缓存同步到权威、返回
//     错误，可以重试。
//
// 归属于别的服务的 kind 返回 ErrRemoteKindNotOwned；存储不支持返回 ErrEmptyClaimUnsupported。要 I/O，只能在 I/O 段
// 调用（运维命令 remote_entity.empty_claims，见 RegisterAdminCommands）。
func (m *Manager) DeleteEmptyClaim(ctx context.Context, id int64) (EmptyClaimOutcome, error) {
	if err := fctx.BlockingError("remoteentity.DeleteEmptyClaim"); err != nil {
		return 0, err
	}
	store, err := m.emptyClaimStore()
	if err != nil {
		return 0, err
	}
	authority, durable := m.ownershipStore.(WriteAuthority)
	if !durable {
		return 0, ErrEmptyClaimUnsupported
	}
	if m.backend == nil {
		return 0, entity.ErrRemoteWriteCapabilityDisabled
	}
	wrapper, ctx, release, err := m.beginOwnershipTransition(ctx, id, false)
	if err != nil {
		return 0, err
	}
	defer release()

	state, found, err := authority.ReadAuthorityState(ctx, wrapper.id)
	if err != nil {
		return 0, fmt.Errorf("remote_entity: read ownership %d: %w", wrapper.id, err)
	}
	wrapper.setOwnership(state.Ownership, found)
	lease := state.Ownership
	switch {
	case !found || lease.OwnerSid != m.localSid || lease.Shared:
		return EmptyClaimNotOwned, nil
	case state.Version != 0:
		return EmptyClaimNotEmpty, nil
	}
	if local := wrapper.lookupLocalEntity(); local != nil {
		if local.EntityVersion() > 0 {
			return EmptyClaimNotEmpty, nil
		}
	} else {
		loaded, loadErr := wrapper.loadEntity(ctx)
		switch {
		case errors.Is(loadErr, entity.ErrAuthorityEntityNotFound):
			// 权威里没有聚合：空认领。
		case errors.Is(loadErr, entity.ErrAuthorityEntityDeleted):
			return EmptyClaimNotEmpty, nil
		case loadErr != nil:
			return 0, fmt.Errorf("remote_entity: load entity %d to confirm the empty claim: %w", wrapper.id, loadErr)
		case loaded != nil:
			// 有聚合（_ver 为 0 却读到数据，不该出现）：不删，交给写准入按版本对齐处理。
			wrapper.attachEntity(loaded)
			return EmptyClaimNotEmpty, nil
		default:
			// loader 既没给实例也没答“不存在”（例如 kind 不符）：确认不了，不删。
			return 0, fmt.Errorf("remote_entity: entity %d: the loader answered neither the entity nor entity.ErrAuthorityEntityNotFound; the empty claim is not confirmed", wrapper.id)
		}
	}
	if emptyClaimCheckedHook != nil {
		emptyClaimCheckedHook(wrapper.id)
	}
	deleted, err := store.DeleteEmptyClaimExpected(ctx, wrapper.id, lease)
	if err != nil {
		return m.settleIndeterminateEmptyClaimDelete(wrapper, lease, err)
	}
	if !deleted {
		// 代际守护拒绝：按权威刷新缓存，下一次准入或查询按新的记录走。
		wrapper.invalidateMarker()
		_ = wrapper.refreshMarked(ctx)
		return EmptyClaimChanged, nil
	}
	m.forgetDeletedClaim(ctx, wrapper, lease)
	return EmptyClaimDeleted, nil
}

// forgetDeletedClaim 在空认领的记录删掉之后同步本进程的状态：所有权缓存置为“没有记录”、清掉对齐记录、按值删路由登记。
// 调用方持写门与所有权写锁。
func (m *Manager) forgetDeletedClaim(ctx context.Context, wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease) {
	wrapper.setOwnership(entity.RemoteEntityMarkerLease{}, false)
	wrapper.clearAligned()
	m.dropRoute(ctx, wrapper.id, RouteValue(m.localSid, lease.MarkerEpoch))
	metrics.IncCounter(emptyClaimMetric, metrics.Labels{"event": "deleted"}, 1)
	slog.Info("remote_entity: deleted an empty claim", "entity", wrapper.id, "owner", lease.OwnerSid,
		"marker_epoch", lease.MarkerEpoch, "route_epoch", lease.RouteEpoch)
}

// settleIndeterminateEmptyClaimDelete 判定一次出错的比较删除：在独立的有界 ctx 上重读权威，记录已不在就按已删收尾，
// 仍在就把缓存同步到权威并返回错误。
func (m *Manager) settleIndeterminateEmptyClaimDelete(wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease, cause error) (EmptyClaimOutcome, error) {
	opErr := fmt.Errorf("remote_entity: delete empty claim %d: %w", wrapper.id, cause)
	wrapper.invalidateMarker()
	recheckCtx, cancel := context.WithTimeout(context.Background(), m.cfg.OpTimeout)
	defer cancel()
	if err := wrapper.refreshMarked(recheckCtx); err != nil {
		wrapper.invalidateMarker()
		return 0, errors.Join(opErr, fmt.Errorf("remote_entity: ownership of %d is unknown until the authority answers: %w", wrapper.id, err))
	}
	if _, found := wrapper.cachedOwnership(); !found {
		m.forgetDeletedClaim(recheckCtx, wrapper, lease)
		return EmptyClaimDeleted, nil
	}
	return 0, opErr
}

// emptyClaimStore 返回支持删空认领的所有权存储。
func (m *Manager) emptyClaimStore() (EmptyClaimStore, error) {
	if m == nil || m.ownershipStore == nil || m.cfg == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	store, ok := m.ownershipStore.(EmptyClaimStore)
	if !ok {
		return nil, fmt.Errorf("%w: %T", ErrEmptyClaimUnsupported, m.ownershipStore)
	}
	return store, nil
}
