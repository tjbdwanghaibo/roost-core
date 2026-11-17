package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 在途换服的索引、列举与按期限了结（D8 A，维护者 2026-11-14）。
//
// 原来期限（RoleTransfer.DeadlineUnix）只记录：源服受理之后重启、玩家又不再来登录源服的，换服一直停在在途，
// account 也说不出有哪些。现在三件事：
//
//   - 索引：角色记录上 Transfer 非空的玩家，按期限从早到晚。它与角色记录**同一次原子写**维护——Redis 上是
//     角色存储自带的有序集合（versionstore.RedisIndex，值与索引一个脚本），进程内是 MemoryRoles 的同一把锁。
//     所以“写下了在途却不在索引里”的窗口不存在，不需要另一次写去补。
//   - 列举：ListRoleTransfers 按发起服列出在途的换服。源服（rehome 模块）起服与之后定期用它把自己受理过、
//     还没做完的换服接着做（向前恢复，不再只靠玩家登录），运维用 accountctl transfer-role list 看。
//   - 按期限了结：期限一到，换服就不能再提交——迟到的 CommitRoleTransfer 被拒绝，并当场把这一次记为已中止；
//     没有人再来提交的，由 account 进程的清扫（SweepExpiredTransfers，server_run.go 定期调用）中止。中止对
//     源服是安全的：account 仍把角色绑在源服；源服若还在做，它的提交拿到 stale 与“仍在本服”的绑定，按中止
//     收尾（删目标库的副本、撤屏障）。
//
// 期限是系统时间（Config.SystemNow），与发起时写下 DeadlineUnix 的是同一个钟。“过期”是 now >= DeadlineUnix。

// TransferIndex 是在途换服的索引，由角色存储实现（与角色记录同一次原子写维护）。Config.Roles 实现了它，
// 列举、清扫才可用；没实现（测试里的普通 MemoryStore、包了一层的故障注入存储）时 ListRoleTransfers 与
// SweepExpiredTransfers 答 ErrNoTransferIndex，提交照样按期限拒绝（那一条只看角色记录）。
// wiring/account 的 Mod 用 NewRedisStores 的角色存储，它实现了这个接口。
type TransferIndex interface {
	// TransfersDue 返回期限不晚于 deadlineUnix 的在途换服的玩家号，期限早的在前；跳过前 offset 个，至多 limit 个。
	TransfersDue(ctx context.Context, deadlineUnix int64, offset, limit int) ([]int64, error)
	// DeferTransfer 把一个条目的分数挪到 untilUnix：记录读不出来、一时处理不了时用，不让它占住每一页的开头。
	// 条目不在索引里时什么都不做。
	DeferTransfer(ctx context.Context, playerID int64, untilUnix int64) error
}

// ErrNoTransferIndex：角色存储没有在途换服的索引（见 TransferIndex）。
var ErrNoTransferIndex = errors.New("account: the role store keeps no index of transfers in flight")

const (
	// MaxListedTransfers 是 ListRoleTransfers 一次至多返回的条数。在途的换服只是正在换服的那些角色，
	// 正常远小于它；到了上限说明有大量换服没有了结，先看清扫是不是在跑。
	MaxListedTransfers = 1000
	// MaxBindingBatch 是 RoleBindingsAway 一次至多问的玩家数。
	MaxBindingBatch = 500
	// transferListPage 是列举时一次读索引的条数。
	transferListPage = 200
	// expiredDeferSeconds 是清扫处理不了的条目往后挪多久。
	expiredDeferSeconds = 60
)

// transferIndex 返回角色存储上的索引；没有时返回 nil。
func (s *Service) transferIndex() TransferIndex {
	index, _ := s.cfg.Roles.(TransferIndex)
	return index
}

// transferExpired 是“这次换服已经过了期限”：nowUnix >= DeadlineUnix。
func transferExpired(transfer *RoleTransfer, nowUnix int64) bool {
	return transfer != nil && nowUnix >= transfer.DeadlineUnix
}

// ListRoleTransfers 列出在途的换服（角色记录上 Transfer 非空），按期限从早到晚，至多 MaxListedTransfers 条。
// fromServerID 非 0 时只列从这个服发起的（RoleTransfer.From）。每条是那个角色当前的 RoleBinding（Transfer 非空）。
// 只读。角色存储没有索引时答 ErrNoTransferIndex。
//
// 列举按索引翻页、再逐条读角色记录核对：翻页期间了结的不列，期间新发起的可能漏掉——调用方要的是“此刻在途的大致全集”，
// 定期再列一次就会看到。
func (s *Service) ListRoleTransfers(ctx context.Context, fromServerID int32) ([]RoleBinding, error) {
	index := s.transferIndex()
	if index == nil {
		return nil, ErrNoTransferIndex
	}
	if fromServerID < 0 {
		return nil, fmt.Errorf("%w: server id %d is negative", ErrRangeInvalid, fromServerID)
	}
	var listed []RoleBinding
	seen := map[int64]bool{}
	for offset := 0; len(listed) < MaxListedTransfers; offset += transferListPage {
		ids, err := index.TransfersDue(ctx, math.MaxInt64, offset, transferListPage)
		if err != nil {
			return nil, err
		}
		for _, playerID := range ids {
			if seen[playerID] {
				continue
			}
			seen[playerID] = true
			role, found, err := s.cfg.Roles.Get(ctx, playerID)
			if err != nil {
				return nil, err
			}
			if !found || role.Value.Transfer == nil || (fromServerID != 0 && role.Value.Transfer.From != fromServerID) {
				continue
			}
			binding, err := s.bindingOf(ctx, role.Value.clone())
			if err != nil {
				return nil, err
			}
			listed = append(listed, binding)
			if len(listed) == MaxListedTransfers {
				break
			}
		}
		if len(ids) < transferListPage {
			break
		}
	}
	slices.SortStableFunc(listed, func(a, b RoleBinding) int {
		switch {
		case a.Transfer.DeadlineUnix < b.Transfer.DeadlineUnix:
			return -1
		case a.Transfer.DeadlineUnix > b.Transfer.DeadlineUnix:
			return 1
		}
		return 0
	})
	return listed, nil
}

// RoleBindingsAway 回答 playerIDs 里哪些角色此刻**不由** serverID 承载：返回这些角色的绑定（ServingServerID 不是
// serverID），其余的（由它承载、不存在的、建角还没发布的）不返回。至多 MaxBindingBatch 个玩家号。只读。
//
// 给 game 进程起服时核对本服库里的玩家（D7 A）：库里有他的档、account 却把他绑在别的服，那份档是换服提交之后、
// 删源库之前崩溃留下的旧档，不能再加载（rehome 模块据此跳过并带版本条件删掉）。在途的换服照常由源服承载，
// 不在返回之列。
func (s *Service) RoleBindingsAway(ctx context.Context, serverID int32, playerIDs []int64) ([]RoleBinding, error) {
	if serverID <= 0 {
		return nil, fmt.Errorf("%w: server id %d is not positive", ErrRangeInvalid, serverID)
	}
	if len(playerIDs) > MaxBindingBatch {
		return nil, fmt.Errorf("%w: %d player ids, at most %d per call", ErrRangeInvalid, len(playerIDs), MaxBindingBatch)
	}
	var away []RoleBinding
	for _, playerID := range playerIDs {
		role, found, err := s.cfg.Roles.Get(ctx, playerID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		binding, err := s.bindingOf(ctx, role.Value.clone())
		if err != nil {
			return nil, err
		}
		if binding.ServingServerID != serverID {
			away = append(away, binding)
		}
	}
	return away, nil
}

// SweepExpiredTransfers 中止至多 limit 个已经过了期限的在途换服，返回中止了几个。account 进程定期调用
// （server_run.go）；多个 account 副本同时清扫无害：中止按角色记录的版本比较落地，同一个只中止一次。
//
// 每一个都按角色记录再判一次：读到时已经了结的、期限还没到的（不会出现：索引的分数就是期限）跳过。
// 处理不了的（读不出来、存储一时失败）往后挪 expiredDeferSeconds，不让它挡住后面的；错误汇总返回。
func (s *Service) SweepExpiredTransfers(ctx context.Context, limit int) (int, error) {
	index := s.transferIndex()
	if index == nil {
		return 0, ErrNoTransferIndex
	}
	if limit <= 0 {
		return 0, nil
	}
	nowUnix := s.cfg.SystemNow().Unix()
	ids, err := index.TransfersDue(ctx, nowUnix, 0, limit)
	if err != nil {
		return 0, err
	}
	aborted := 0
	var failures error
	for _, playerID := range ids {
		done, err := s.abortExpired(ctx, playerID)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("role %d: %w", playerID, err))
			failures = errors.Join(failures, index.DeferTransfer(ctx, playerID, nowUnix+expiredDeferSeconds))
			continue
		}
		if done {
			aborted++
		}
	}
	return aborted, failures
}

// abortExpired 中止 playerID 在途、已经过期的那次换服；没有在途的、没过期的什么都不做（done=false）。
func (s *Service) abortExpired(ctx context.Context, playerID int64) (bool, error) {
	role, err := s.loadRole(ctx, playerID)
	if errors.Is(err, ErrRoleMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	flight := role.Transfer
	if flight == nil || !transferExpired(flight, s.cfg.SystemNow().Unix()) {
		return false, nil
	}
	_, applied, err := s.abortRoleTransfer(ctx, playerID, flight.ID, expiryNote(flight), true)
	if err != nil {
		return false, err
	}
	if applied {
		s.report.Dropped("role_transfer.expired", 1)
		slog.Warn("account: aborted a role transfer past its deadline", "player_id", playerID, "transfer_id", flight.ID,
			"from", flight.From, "to", flight.To, "deadline_unix", flight.DeadlineUnix)
	}
	return applied, nil
}

// expiryNote 是按期限中止时写进 LastTransfer.Note 的原因。
func expiryNote(flight *RoleTransfer) string {
	return "expired: the deadline " + strconv.FormatInt(flight.DeadlineUnix, 10) + " passed before the transfer was committed"
}

// MemoryRoles 是进程内的角色存储，带在途换服的索引（TransferIndex）：给单进程调试与测试用，与 versionstore.MemoryStore
// 一样不是生产的权威存储。索引与值在同一把锁下改，和 Redis 上的原子写等价。
type MemoryRoles struct {
	mu    sync.Mutex
	inner *versionstore.MemoryStore[int64, Role]
	// due 是在途换服的玩家 → 分数（期限，或被 DeferTransfer 挪到的时刻）。
	due map[int64]int64
}

// NewMemoryRoles 返回空的 MemoryRoles。
func NewMemoryRoles() *MemoryRoles {
	return &MemoryRoles{inner: versionstore.NewMemoryStore[int64, Role](), due: map[int64]int64{}}
}

func (m *MemoryRoles) Get(ctx context.Context, key int64) (versionstore.Versioned[Role], bool, error) {
	return m.inner.Get(ctx, key)
}

func (m *MemoryRoles) Update(ctx context.Context, key int64, mutate versionstore.Mutate[Role]) (versionstore.Versioned[Role], bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, applied, err := m.inner.Update(ctx, key, mutate)
	if err == nil && applied {
		m.reindex(key, stored.Value)
	}
	return stored, applied, err
}

func (m *MemoryRoles) Create(ctx context.Context, key int64, value Role) (versionstore.Versioned[Role], bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, created, err := m.inner.Create(ctx, key, value)
	if err == nil && created {
		m.reindex(key, stored.Value)
	}
	return stored, created, err
}

func (m *MemoryRoles) Delete(ctx context.Context, key int64, expect versionstore.Versioned[Role]) error {
	return m.DeleteIf(ctx, key, expect, nil)
}

func (m *MemoryRoles) DeleteIf(ctx context.Context, key int64, expect versionstore.Versioned[Role], match func(Role) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inner.DeleteIf(ctx, key, expect, match); err != nil {
		return err
	}
	delete(m.due, key)
	return nil
}

// reindex 与 Redis 上的 transferIndexEntry 同一条规则：Transfer 非空才在索引里，分数是期限。
func (m *MemoryRoles) reindex(key int64, role Role) {
	if score, include := transferIndexEntry(role); include {
		m.due[key] = int64(score)
		return
	}
	delete(m.due, key)
}

func (m *MemoryRoles) TransfersDue(_ context.Context, deadlineUnix int64, offset, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type entry struct{ playerID, score int64 }
	var due []entry
	for playerID, score := range m.due {
		if score <= deadlineUnix {
			due = append(due, entry{playerID, score})
		}
	}
	slices.SortFunc(due, func(a, b entry) int {
		if a.score != b.score {
			return int(min(max(a.score-b.score, -1), 1))
		}
		return int(min(max(a.playerID-b.playerID, -1), 1))
	})
	var out []int64
	for i, item := range due {
		if i < offset {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, item.playerID)
	}
	return out, nil
}

func (m *MemoryRoles) DeferTransfer(_ context.Context, playerID int64, untilUnix int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.due[playerID]; ok {
		m.due[playerID] = untilUnix
	}
	return nil
}

// transferIndexEntry 是一条角色记录在在途换服索引里的样子：Transfer 非空才收录，分数是期限。
func transferIndexEntry(role Role) (score float64, include bool) {
	if role.Transfer == nil {
		return 0, false
	}
	return float64(role.Transfer.DeadlineUnix), true
}

// redisRoles 是 Redis 上的角色存储，带在途换服的索引（索引键 <前缀>:transfers，与角色记录同一个脚本维护）。
type redisRoles struct {
	*versionstore.RedisStore[int64, Role]
}

func (r redisRoles) TransfersDue(ctx context.Context, deadlineUnix int64, offset, limit int) ([]int64, error) {
	members, err := r.IndexPage(ctx, float64(deadlineUnix), offset, limit)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(members))
	for _, member := range members {
		playerID, err := strconv.ParseInt(member, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("account: transfer index member %q is not a player id: %w", member, err)
		}
		out = append(out, playerID)
	}
	return out, nil
}

func (r redisRoles) DeferTransfer(ctx context.Context, playerID int64, untilUnix int64) error {
	_, err := r.IndexDefer(ctx, playerID, float64(untilUnix))
	return err
}

var (
	_ versionstore.Store[int64, Role]              = (*MemoryRoles)(nil)
	_ versionstore.ConditionalDeleter[int64, Role] = (*MemoryRoles)(nil)
	_ TransferIndex                                = (*MemoryRoles)(nil)
	_ TransferIndex                                = redisRoles{}
)
