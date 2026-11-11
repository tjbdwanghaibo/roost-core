package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 单个角色换服：把一个角色从它绑定的服改绑到另一个服（设计 docs/maintenance/FEATURE-2026-11-11-player-rehome.md
// §3.1，现行说明 docs/framework/PLAYER-MIGRATION.md“单个玩家换服（account 一侧）”）。
//
// account 只管绑定：Role.ServerID、slot、BindEpoch。停写、交出实体、到达这些游戏侧的步骤不在这里；
// 调用方（源服进程，或运维的 accountctl）保证 Commit 之前源服已经不再写这个玩家。
//
// 三个写接口都是“角色记录定结果、slot 跟着走”：
//
//   - 角色记录是唯一的权威。Role.Transfer 非空就是换服在途；提交还是中止由角色记录上的一次版本比较
//     （versionstore.Update）决定，先落的那个生效，另一个得到 ErrTransferStale。所以已提交的不能被中止，
//     已中止的不能被提交。
//   - slot 先加后删。目标服的占位（Slot.TransferID）在改绑之前就在；旧服的 slot、被中止的占位只在角色
//     记录定了结果之后才删。角色因此任何时刻都有一个可用的 slot（readySlot），不会出现“绑定在某个服、
//     那里却没有它的 slot”。
//   - 跨记录的写不是原子的，任何一步之后都可能停下。每一步都按内容认记录（换服 ID、PlayerID、绑定代数），
//     重发同一个请求补完剩下的步骤；结果已定而 slot 没收尾的状态记在 RoleTransferRecord.Settled 上，
//     重发的 Commit / Abort、下一次 SelectRole、下一次 Begin 读到就补完（settleTransfer）。
//   - 占位以角色记录为准。占位只在角色写下 Transfer 之后才建，但建的那一刻角色可能已经变了（这次换服
//     被中止、提交，或换成了另一次）：Begin 占完位再读一次角色，不认这次换服了就撤掉占位、带回当前绑定。
//     占位写下之后进程就退出的，留下的是一个没有角色记录认的占位（releaseStrayHold 的定义）；它由之后
//     碰到它的入口撤掉——重发的 Abort、SelectRole、下一次 Begin、账号在那个服的 CreateRole。
//
// 换服 ID 由调用方给出，一次换服一个：同一个 ID 重发幂等，了结之后不能再用来发起。
//
// 过期的请求靠 Role.TransferSeq 挡住，不靠记住用过的 ID：它每受理一次发起、每记下一次中止就加 1，发起
// 必须带上当前值（TransferRequest.ExpectSeq）。一次换服被中止之后，那个 Begin 的原样重放带的是旧序号，
// 无论后来又有多少次发起与中止把 LastTransfer 换掉，它都发起不了。
//
// 一期的边界：不涉及已并入另一服的服（角色的绑定服已并入别处、或目标服已并入别处都拒绝）；期限
// （RoleTransfer.DeadlineUnix）只记录，不由 account 执行；没有在途换服的索引与列举，MergeServer 不检查
// 是否有在途的换服指向被合并的服（提交时会被拒绝，由调用方中止）。

const (
	// DefaultTransferTTL 是 TransferRequest.TTLSeconds 为 0 时的期限。
	DefaultTransferTTL = 10 * time.Minute
	// MaxTransferTTL 是期限的上限：超过它的请求被拒绝，而不是悄悄截短。
	MaxTransferTTL = 24 * time.Hour
	// MaxTransferIDBytes 是换服 ID 的长度上限。
	MaxTransferIDBytes = 128
)

// RoleBinding 返回角色当前的绑定：绑定服、承载服、绑定代数、换服尝试的序号、在途与最近一次了结的换服。只读。
// 角色不存在是 ErrRoleMissing；建角还没发布的角色没有绑定可言，与 ValidateSession 一样答 ErrConflict。
func (s *Service) RoleBinding(ctx context.Context, playerID int64) (RoleBinding, error) {
	role, err := s.loadRole(ctx, playerID)
	if err != nil {
		return RoleBinding{}, err
	}
	if err := s.roleReady(ctx, role); err != nil {
		return RoleBinding{}, err
	}
	return s.bindingOf(ctx, role)
}

// BeginRoleTransfer 登记一次换服并占住目标服的名额。成功之后角色仍绑定源服、照常可登录，Role.Transfer
// 非空；之后由 CommitRoleTransfer 改绑，或 AbortRoleTransfer 撤销。
//
// 拒绝（什么也不写）：
//
//   - ErrRangeInvalid：ID 为空或超长、TTLSeconds 为负或超过 MaxTransferTTL、备注超长。
//   - ErrRoleMissing / ErrConflict：角色不存在 / 建角还没发布。
//   - ErrTransferInProgress：另一次换服在途（另一个 ID，或同一个 ID 但目标不同）。
//   - ErrTransferStale：这个 ID 已经了结（不能复用）；ExpectEpoch 不是当前 BindEpoch；ExpectSeq 不是当前
//     TransferSeq（读到绑定之后有别的发起或中止落了地）；FromSID 不是当前承载服。
//   - ErrServerInvalid：目标服不是正数、不存在、或就是当前绑定服。
//   - ErrServerClosed：目标服已并入另一服或不是 open；角色的绑定服已并入另一服（一期不支持）。
//   - ErrRoleLimit：账号在目标服已有角色、待定的建角计划或别的占位。
//
// ErrTransferInProgress 与 ErrTransferStale 同时带回当前的 RoleBinding。
//
// 写入顺序：① 角色写 Transfer、TransferSeq 加 1；② 在目标服建占位 slot；③ 再读一次角色，确认它仍然认
// 这次换服。停在 ① 之后的，重发同一个 Begin（或直接 Commit）补上 ②。② 发现名额被别人抢先占了（与目标服
// 上的 CreateRole 竞争）时撤掉 ①、答 ErrRoleLimit（TransferSeq 不退回：这个请求已经用掉了它带的序号）。
// 先写角色再占位，是因为反过来停下会留下一个没有任何记录指向它的占位。
//
// ③ 是因为 ① 与 ② 之间这次换服可能已经被了结：Abort 看到“还没有占位”就收了尾，随后 ② 才把占位写下去。
// 这时撤掉刚留下的占位，答 ErrTransferStale（已中止、已提交）或 ErrTransferInProgress（在途的已经是
// 另一次）并带回当前绑定——与一个在了结之后才到的 Begin 得到的回答相同。已经提交的那个 slot 不会被
// 撤：它是角色在新服的 slot（releaseStrayHold）。停在 ② 与 ③ 之间的，占位由之后的入口撤掉。
func (s *Service) BeginRoleTransfer(ctx context.Context, playerID int64, request TransferRequest) (RoleBinding, error) {
	id, err := validTransferID(request.ID)
	if err != nil {
		return RoleBinding{}, err
	}
	ttl := time.Duration(request.TTLSeconds) * time.Second
	if request.TTLSeconds < 0 || ttl > MaxTransferTTL {
		return RoleBinding{}, fmt.Errorf("%w: transfer ttl is %ds, want 0 (default %s) to %s", ErrRangeInvalid, request.TTLSeconds, DefaultTransferTTL, MaxTransferTTL)
	}
	if ttl == 0 {
		ttl = DefaultTransferTTL
	}
	note, err := boundedNote(request.Note)
	if err != nil {
		return RoleBinding{}, err
	}
	role, err := s.loadRole(ctx, playerID)
	if err != nil {
		return RoleBinding{}, err
	}
	resent, err := decideBegin(role, id, request)
	if err != nil {
		s.report.Refused("begin_role_transfer", refusalReason(err))
		return s.withBinding(ctx, role, err)
	}
	if !resent {
		// 上一次换服的 slot 收尾先补完：它留下的旧 slot 可能正占着这次的目标服，LastTransfer 也即将被覆盖。
		if role, err = s.settleTransfer(ctx, role); err != nil {
			return RoleBinding{}, err
		}
		if err := s.admitTransfer(ctx, role, request.FromSID, request.To); err != nil {
			s.report.Refused("begin_role_transfer", refusalReason(err))
			if errors.Is(err, ErrTransferInProgress) {
				// 检查期间另一次换服先发起了：带回的绑定要看得到它。
				if latest, loadErr := s.loadRole(ctx, playerID); loadErr == nil {
					role = latest
				}
				return s.withBinding(ctx, role, err)
			}
			if errors.Is(err, ErrTransferStale) {
				return s.withBinding(ctx, role, err)
			}
			return RoleBinding{}, err
		}
		started := s.cfg.SystemNow() // 期限是真实时间上的界，不随业务时间偏移
		transfer := RoleTransfer{
			ID: id, From: role.ServerID, To: request.To, Epoch: role.BindEpoch,
			StartedAtUnix: started.Unix(), DeadlineUnix: started.Add(ttl).Unix(), Note: note,
		}
		latest := role
		stored, _, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
			if !found {
				return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
			}
			latest = current
			resent, err := decideBegin(current, id, request)
			if err != nil || resent {
				return current, false, err
			}
			if current.ServerID != transfer.From {
				return current, false, fmt.Errorf("%w: role %d moved to server %d meanwhile", ErrTransferStale, playerID, current.ServerID)
			}
			next := current.clone()
			next.Transfer, next.TransferSeq = cloneOf(&transfer), current.TransferSeq+1
			return next, true, nil
		})
		if err != nil {
			if errors.Is(err, ErrTransferStale) || errors.Is(err, ErrTransferInProgress) {
				s.report.Refused("begin_role_transfer", refusalReason(err))
				return s.withBinding(ctx, latest, err)
			}
			return RoleBinding{}, err
		}
		role = stored.Value.clone()
	}
	held := *role.Transfer
	holdErr := s.holdTarget(ctx, role, held)
	if holdErr != nil && !errors.Is(holdErr, ErrRoleLimit) {
		// 存储失败：占位写没写成不知道。重发同一个 Begin 补上；这次换服若已经被了结，可能留下的占位由
		// 之后的入口撤掉（releaseStrayHold）。
		return RoleBinding{}, holdErr
	}
	// ③ 占位这一步用的是 ① 那一刻的角色记录。以角色现在的结论为准。
	current, err := s.loadRole(ctx, playerID)
	if err != nil {
		return RoleBinding{}, err
	}
	if standing, refusal := decideBegin(current, id, request); !standing {
		if refusal == nil {
			// 角色上已经没有这次换服、也没有留下它的了结记录：并发的同一个 Begin 占不到名额，把它撤了。
			refusal = fmt.Errorf("%w: transfer %s of role %d was withdrawn meanwhile", ErrTransferStale, id, playerID)
		}
		s.report.Refused("begin_role_transfer", refusalReason(refusal))
		release := s.compensated(s.releaseStrayHold(ctx, slotKeyFor(role.AccountID, held.To)))
		binding, _ := s.withBinding(ctx, current, nil)
		return binding, errors.Join(refusal, release)
	}
	if holdErr != nil {
		// 名额被别人占了，这次换服做不成：撤掉 ①，当作没有发起过。撤不掉时角色留着 Transfer，
		// 重发同一个 Begin 会再撤一次，AbortRoleTransfer 也能了结它。
		s.report.Refused("begin_role_transfer", "role_limit")
		return RoleBinding{}, errors.Join(holdErr, s.compensated(s.withdrawTransfer(ctx, playerID, id)))
	}
	if resent {
		s.report.Replayed("begin_role_transfer")
	} else {
		s.report.Accepted("begin_role_transfer")
		slog.Info("account: role transfer begun", "player_id", playerID, "transfer_id", id, "from", held.From, "to", held.To, "epoch", held.Epoch, "transfer_seq", role.TransferSeq)
	}
	return s.bindingOf(ctx, current)
}

// CommitRoleTransfer 把在途的换服提交：角色改绑到目标服，BindEpoch 加 1。调用方保证源服已经不再写这个
// 玩家（已卸载并保持屏障，或进程已退出且 WAL 已排空）；account 不知道也不检查这一点。
//
// 同一个 ID 重发幂等（已提交的返回当前绑定，并补完 slot 的收尾）。拒绝：
//
//   - ErrTransferStale：这个 ID 已被中止、不是在途的那一次、或没有发起过；带回当前绑定。
//   - ErrServerInvalid / ErrServerClosed：目标服在换服期间被删除 / 并入了另一服；带回当前绑定，
//     由调用方 AbortRoleTransfer。
//   - ErrRoleLimit：目标服的名额已经不是这次换服的占位（占位被中止撤掉之后别人建了角）；同上。
//
// 写入顺序：确认占位在（缺了补上）→ 角色改 ServerID、BindEpoch、清 Transfer、写 LastTransfer → 清新服
// slot 的占位标记 → 删旧服的 slot → 记 Settled。角色写之前停下，什么也没变；之后停下，角色已经在新服
// 可用，剩下的由重发补完。
func (s *Service) CommitRoleTransfer(ctx context.Context, playerID int64, transferID string) (RoleBinding, error) {
	return s.commitRoleTransfer(ctx, playerID, transferID, "")
}

// CommitRoleTransferWithNote 是运维的人工提交（accountctl transfer-role commit）：与 CommitRoleTransfer
// 相同，另要求一条备注并写进 LastTransfer.Note。人工提交是操作者在断言“源服已经不再写这个玩家”，这句
// 断言要留在记录上。
func (s *Service) CommitRoleTransferWithNote(ctx context.Context, playerID int64, transferID, note string) (RoleBinding, error) {
	note, err := validateAdminNote(note)
	if err != nil {
		return RoleBinding{}, err
	}
	return s.commitRoleTransfer(ctx, playerID, transferID, note)
}

func (s *Service) commitRoleTransfer(ctx context.Context, playerID int64, transferID, note string) (RoleBinding, error) {
	id, err := validTransferID(transferID)
	if err != nil {
		return RoleBinding{}, err
	}
	role, err := s.loadRole(ctx, playerID)
	if err != nil {
		return RoleBinding{}, err
	}
	switch standingOf(role, id) {
	case transferCommitted:
		s.report.Replayed("commit_role_transfer")
	case transferInFlight:
		transfer := *role.Transfer
		if err := s.transferTarget(ctx, role, transfer.To, false); err != nil {
			s.report.Refused("commit_role_transfer", refusalReason(err))
			return s.withBinding(ctx, role, err)
		}
		if err := s.holdTarget(ctx, role, transfer); err != nil {
			if errors.Is(err, ErrRoleLimit) {
				s.report.Refused("commit_role_transfer", "role_limit")
				return s.withBinding(ctx, role, err)
			}
			return RoleBinding{}, err
		}
		nowUnix := s.cfg.SystemNow().Unix()
		latest := role
		stored, applied, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
			if !found {
				return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
			}
			latest = current
			switch standingOf(current, id) {
			case transferInFlight:
				flight := *current.Transfer
				next := current.clone()
				next.ServerID, next.BindEpoch, next.Transfer = flight.To, current.BindEpoch+1, nil
				next.LastTransfer = &RoleTransferRecord{
					ID: id, From: flight.From, To: flight.To, Committed: true, Epoch: next.BindEpoch, AtUnix: nowUnix, Note: flight.Note,
				}
				if note != "" {
					next.LastTransfer.Note = note
				}
				return next, true, nil
			case transferCommitted:
				return current, false, nil
			default:
				return current, false, commitRefusal(current, id)
			}
		})
		if err != nil {
			if errors.Is(err, ErrTransferStale) {
				// 这次换服没有被提交（并发的中止先落了）：上面可能刚补建的占位没有人再认，撤掉。
				s.report.Refused("commit_role_transfer", "stale")
				binding, _ := s.withBinding(ctx, latest, nil)
				return binding, errors.Join(err, s.compensated(s.releaseStrayHold(ctx, slotKeyFor(role.AccountID, transfer.To))))
			}
			return RoleBinding{}, err
		}
		role = stored.Value.clone()
		if applied {
			s.report.Accepted("commit_role_transfer")
			slog.Info("account: role transfer committed", "player_id", playerID, "transfer_id", id, "from", transfer.From, "to", transfer.To, "bind_epoch", role.BindEpoch, "note", role.LastTransfer.Note)
		} else {
			s.report.Replayed("commit_role_transfer")
		}
	default:
		s.report.Refused("commit_role_transfer", "stale")
		return s.withBinding(ctx, role, commitRefusal(role, id))
	}
	if role, err = s.settleTransfer(ctx, role); err != nil {
		return RoleBinding{}, fmt.Errorf("account: role %d is bound to server %d, but finishing transfer %s failed (re-send the same commit): %w", playerID, role.ServerID, id, err)
	}
	return s.bindingOf(ctx, role)
}

// AbortRoleTransfer 中止一次换服：角色留在源服，目标服的占位撤掉，这个 ID 记为已中止——之后它既不能
// 提交，也不能用来重新发起。reason 写进 LastTransfer.Note（可空，长度上限同备注）。
//
// 每记下一次中止，TransferSeq 加 1：中止之前发出的 Begin 带的都是旧序号，从此发起不了，哪怕后来的了结
// 把 LastTransfer 换成了别的 ID。重发（这个 ID 仍是最近了结的那一次）不再加。
//
// 同一个 ID 重发幂等，并且每次都再看一眼目标服：中止当时还没写下的占位（Begin 停在占位之前）在这里撤掉。
// 对一个没有发起成功的 ID 也可以中止（当前没有换服在途时）：同样记为已中止、TransferSeq 加 1，于是
// 一个迟到的 Begin 不能再把它做起来——调用方在 Begin 结果未知时用它得到确定的结论。拒绝：
//
//   - ErrTransferStale：这个 ID 已经提交；带回已提交的绑定，调用方按已提交处理。
//   - ErrTransferInProgress：在途的是另一个 ID；带回当前绑定。
//
// 写入顺序：角色清 Transfer、LastTransfer 记未提交、TransferSeq 加 1 → 删目标服的占位 → 记 Settled。先写角色再删占位：
// 反过来的话，一个并发的 Commit 可能在占位被删之后把角色改绑过去，留下一个没有 slot 的角色。
func (s *Service) AbortRoleTransfer(ctx context.Context, playerID int64, transferID, reason string) (RoleBinding, error) {
	id, err := validTransferID(transferID)
	if err != nil {
		return RoleBinding{}, err
	}
	reason, err = boundedNote(reason)
	if err != nil {
		return RoleBinding{}, err
	}
	role, err := s.loadRole(ctx, playerID)
	if err != nil {
		return RoleBinding{}, err
	}
	standing := standingOf(role, id)
	if standing == transferUnknown {
		// 要把这个 ID 记进 LastTransfer：上一次换服的收尾先补完，否则它的记录被覆盖之后没有人再补。
		if role, err = s.settleTransfer(ctx, role); err != nil {
			return RoleBinding{}, err
		}
	}
	switch standing {
	case transferAborted:
		s.report.Replayed("abort_role_transfer")
	case transferInFlight, transferUnknown:
		nowUnix := s.cfg.SystemNow().Unix()
		latest := role
		stored, applied, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
			if !found {
				return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
			}
			latest = current
			record := RoleTransferRecord{ID: id, From: current.ServerID, Epoch: current.BindEpoch, AtUnix: nowUnix, Note: reason}
			switch standingOf(current, id) {
			case transferInFlight:
				record.From, record.To = current.Transfer.From, current.Transfer.To
			case transferUnknown:
				if last := current.LastTransfer; last != nil && !last.Settled {
					return current, false, fmt.Errorf("%w: transfer %s of role %d was settled meanwhile and is not finished; retry", ErrConflict, last.ID, playerID)
				}
				record.Settled = true // 没有发起过，没有占位可撤
			case transferAborted:
				return current, false, nil
			default:
				return current, false, abortRefusal(current, id)
			}
			next := current.clone()
			next.Transfer, next.LastTransfer, next.TransferSeq = nil, &record, current.TransferSeq+1
			return next, true, nil
		})
		if err != nil {
			if errors.Is(err, ErrTransferStale) || errors.Is(err, ErrTransferInProgress) {
				s.report.Refused("abort_role_transfer", refusalReason(err))
				return s.withBinding(ctx, latest, err)
			}
			return RoleBinding{}, err
		}
		role = stored.Value.clone()
		if applied {
			s.report.Accepted("abort_role_transfer")
			slog.Info("account: role transfer aborted", "player_id", playerID, "transfer_id", id, "from", role.LastTransfer.From, "to", role.LastTransfer.To, "reason", reason)
		} else {
			s.report.Replayed("abort_role_transfer")
		}
	default:
		err := abortRefusal(role, id)
		s.report.Refused("abort_role_transfer", refusalReason(err))
		return s.withBinding(ctx, role, err)
	}
	if role, err = s.settleTransfer(ctx, role); err != nil {
		return RoleBinding{}, fmt.Errorf("account: transfer %s of role %d is recorded as aborted, but releasing its hold failed (re-send the same abort): %w", id, playerID, err)
	}
	return s.bindingOf(ctx, role)
}

// transferStanding 是一个换服 ID 相对于一条角色记录的状态。三个写接口都先按它归类再决定，CAS 里对当前值
// 再归类一次。
type transferStanding uint8

const (
	// transferUnknown：没有换服在途，这个 ID 也不是最近了结的那一次。
	transferUnknown transferStanding = iota + 1
	// transferInFlight：在途的就是这个 ID。
	transferInFlight
	// transferOtherInFlight：在途的是另一个 ID。
	transferOtherInFlight
	// transferCommitted / transferAborted：最近了结的就是这个 ID。
	transferCommitted
	transferAborted
)

func standingOf(role Role, id string) transferStanding {
	switch last := role.LastTransfer; {
	case role.Transfer != nil && role.Transfer.ID == id:
		return transferInFlight
	case role.Transfer != nil:
		return transferOtherInFlight
	case last != nil && last.ID == id && last.Committed:
		return transferCommitted
	case last != nil && last.ID == id:
		return transferAborted
	}
	return transferUnknown
}

// decideBegin 是 Begin 对一条角色记录的判定：resent 表示同一次换服已经在途（幂等重发），err 是拒绝。
// 两个都是零值表示可以发起。在途的重发不看 ExpectEpoch / ExpectSeq：受理它的那一次已经把序号加过了。
func decideBegin(role Role, id string, request TransferRequest) (resent bool, err error) {
	to := request.To
	switch standingOf(role, id) {
	case transferInFlight:
		if role.Transfer.To == to {
			return true, nil
		}
		return false, fmt.Errorf("%w: transfer %s of role %d is already moving it to server %d, not %d", ErrTransferInProgress, id, role.PlayerID, role.Transfer.To, to)
	case transferOtherInFlight:
		return false, fmt.Errorf("%w: role %d is moving to server %d (transfer %s)", ErrTransferInProgress, role.PlayerID, role.Transfer.To, role.Transfer.ID)
	case transferCommitted, transferAborted:
		return false, fmt.Errorf("%w: transfer %s of role %d is already settled; begin again with a new id", ErrTransferStale, id, role.PlayerID)
	}
	if role.BindEpoch != request.ExpectEpoch {
		return false, fmt.Errorf("%w: role %d is at bind epoch %d, caller presented %d", ErrTransferStale, role.PlayerID, role.BindEpoch, request.ExpectEpoch)
	}
	if role.TransferSeq != request.ExpectSeq {
		// 读到绑定之后有别的发起或中止落了地。这个请求本身可能就是一次已经中止的换服的重放。
		return false, fmt.Errorf("%w: role %d is at transfer seq %d, caller presented %d; read the binding again", ErrTransferStale, role.PlayerID, role.TransferSeq, request.ExpectSeq)
	}
	return false, nil
}

func commitRefusal(role Role, id string) error {
	switch standingOf(role, id) {
	case transferAborted:
		return fmt.Errorf("%w: transfer %s of role %d was aborted", ErrTransferStale, id, role.PlayerID)
	case transferOtherInFlight:
		return fmt.Errorf("%w: role %d is in transfer %s, not %s", ErrTransferStale, role.PlayerID, role.Transfer.ID, id)
	}
	return fmt.Errorf("%w: role %d has no transfer %s to commit", ErrTransferStale, role.PlayerID, id)
}

func abortRefusal(role Role, id string) error {
	if standingOf(role, id) == transferCommitted {
		return fmt.Errorf("%w: transfer %s of role %d is already committed; the role is bound to server %d", ErrTransferStale, id, role.PlayerID, role.ServerID)
	}
	return fmt.Errorf("%w: role %d is in transfer %s, not %s", ErrTransferInProgress, role.PlayerID, role.Transfer.ID, id)
}

// refusalReason 是拒绝在指标里的标签。
func refusalReason(err error) string {
	switch {
	case errors.Is(err, ErrTransferStale):
		return "stale"
	case errors.Is(err, ErrTransferInProgress):
		return "in_progress"
	case errors.Is(err, ErrRoleLimit):
		return "role_limit"
	case errors.Is(err, ErrServerClosed):
		return "server_closed"
	case errors.Is(err, ErrServerInvalid):
		return "server_invalid"
	}
	return "refused"
}

// admitTransfer 是 Begin 在写之前的检查（不写）：角色可用、发起方是承载服、不涉及已并入的服、目标服
// 可去、目标服的名额空着。
func (s *Service) admitTransfer(ctx context.Context, role Role, fromSID, to int32) error {
	if err := s.roleReady(ctx, role); err != nil {
		return err
	}
	serving, err := s.servingServer(ctx, role.ServerID)
	if err != nil {
		return err
	}
	if fromSID != serving {
		return fmt.Errorf("%w: role %d is served by server %d, not %d", ErrTransferStale, role.PlayerID, serving, fromSID)
	}
	if serving != role.ServerID {
		return fmt.Errorf("%w: server %d of role %d is merged into %d; roles of a merged server cannot be transferred yet",
			ErrServerClosed, role.ServerID, role.PlayerID, serving)
	}
	if err := s.transferTarget(ctx, role, to, true); err != nil {
		return err
	}
	key := slotKeyFor(role.AccountID, to)
	slot, taken, err := s.cfg.Slots.Get(ctx, key)
	if err != nil {
		return err
	}
	if taken && slot.Value.TransferID != "" {
		// 目标名额上是一个换服占位：没有角色记录认它的（一次已经了结的换服迟到写下的）先撤掉。
		if err := s.releaseStrayHold(ctx, key); err != nil {
			return err
		}
		if slot, taken, err = s.cfg.Slots.Get(ctx, key); err != nil {
			return err
		}
	}
	if taken && slot.Value.TransferID != "" && slot.Value.PlayerID == role.PlayerID {
		// 占着目标名额的是这个角色自己的另一次换服：它在本次读角色记录之后才发起。
		return fmt.Errorf("%w: role %d is moving to server %d (transfer %s)", ErrTransferInProgress, role.PlayerID, to, slot.Value.TransferID)
	}
	if taken {
		return fmt.Errorf("%w: %s already holds a role, a pending creation or a transfer hold on server %d", ErrRoleLimit, role.AccountID, to)
	}
	return nil
}

// transferTarget 检查目标服：存在、不是当前绑定服、没有并入另一服；opening（发起时）还要求它是 open。
// 提交时不再看状态：发起之后运维把目标服改成维护或爆满，在途的那个角色仍然落地。
func (s *Service) transferTarget(ctx context.Context, role Role, to int32, opening bool) error {
	if to <= 0 {
		return fmt.Errorf("%w: target server id %d is not positive", ErrServerInvalid, to)
	}
	if to == role.ServerID {
		return fmt.Errorf("%w: role %d is already bound to server %d", ErrServerInvalid, role.PlayerID, to)
	}
	server, found, err := s.cfg.Servers.Get(ctx, to)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: server %d is unknown", ErrServerInvalid, to)
	}
	if into := server.Value.MergedInto; into != 0 {
		return fmt.Errorf("%w: server %d is merged into %d; transfers onto a merged server are not supported yet", ErrServerClosed, to, into)
	}
	if opening && !server.Value.Status.acceptsNewRoles() {
		return fmt.Errorf("%w: server %d is %s", ErrServerClosed, to, server.Value.Status)
	}
	return nil
}

// holdTarget 保证目标服的 slot 是这次换服的占位：已经是就什么也不做，没有就建一个，被别的占着答
// ErrRoleLimit。占位照抄源服 slot 的建角计划并带着角色的 PlayerID，提交之后清掉 TransferID 就是角色
// 在新服的正式 slot；它属于提交之后的那一代绑定（Epoch+1），所以提交之前不会被当作可用的 slot。
//
// 目标名额上是这个角色的另一个占位时先让 releaseStrayHold 看一次：没有角色记录认它（一次已经了结的
// 换服迟到写下的）就撤掉再占，有人认（另一次换服在途）才是名额被占。
func (s *Service) holdTarget(ctx context.Context, role Role, transfer RoleTransfer) error {
	key := slotKeyFor(role.AccountID, transfer.To)
	swept := false
	for range 3 {
		slot, found, err := s.cfg.Slots.Get(ctx, key)
		if err != nil {
			return err
		}
		if found {
			held := slot.Value
			if held.TransferID == transfer.ID && held.PlayerID == role.PlayerID && held.BindEpoch == transfer.Epoch+1 {
				return nil
			}
			if held.TransferID != "" && held.PlayerID == role.PlayerID && !swept {
				swept = true
				if err := s.releaseStrayHold(ctx, key); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: the slot of %s on server %d is not the hold of transfer %s", ErrRoleLimit, role.AccountID, transfer.To, transfer.ID)
		}
		source, err := s.readySlot(ctx, role)
		if err != nil {
			return err
		}
		hold := Slot{
			AccountID: role.AccountID, ServerID: transfer.To, PlayerID: role.PlayerID, Creation: source.Creation,
			TransferID: transfer.ID, BindEpoch: transfer.Epoch + 1,
		}
		if _, created, err := s.cfg.Slots.Create(ctx, key, hold); err != nil || created {
			return err
		}
		// 没建成：读到“没有”之后别人建了。再看一次是谁。
	}
	return fmt.Errorf("%w: the slot of %s on server %d keeps changing", ErrConflict, role.AccountID, transfer.To)
}

// withdrawTransfer 撤掉 Begin 刚写下、占不到名额的 Transfer。不留记录：这次换服没有发起成。TransferSeq
// 不退回：受理时加的那一次已经让这个请求过期，原样重放它不会再被受理。
func (s *Service) withdrawTransfer(ctx context.Context, playerID int64, id string) error {
	_, _, err := s.cfg.Roles.Update(ctx, playerID, func(current Role, found bool) (Role, bool, error) {
		if !found || current.Transfer == nil || current.Transfer.ID != id {
			return current, false, nil
		}
		next := current.clone()
		next.Transfer = nil
		return next, true, nil
	})
	return err
}

// settleTransfer 补完最近一次了结的换服在 slot 上的收尾，并在角色记录上记 Settled；返回最新的角色记录。
// 每一步都按内容认记录，重复执行、迟到执行都无害：
//
//   - 已提交：新服 slot 的占位标记只在它仍是这次换服的标记时清掉；旧服的 slot 只在它属于这个角色、
//     绑定代数比这次提交更旧时删掉——角色后来又换回旧服的话，那里的 slot 属于更新的一代，不会被删。
//     记了 Settled 之后不再做。
//   - 已中止：目标服上没有角色记录认的占位撤掉（releaseStrayHold）。记了 Settled 之后仍然每次都看：
//     停在占位写入之前的 Begin 可能在收尾之后才把占位写下去，Settled 只说明当时撤过。代价是这些入口
//     在最近一次换服是中止时多读一次目标服的 slot。中止的是没发起过的 ID 时没有目标服可看。
func (s *Service) settleTransfer(ctx context.Context, role Role) (Role, error) {
	last := role.LastTransfer
	if last == nil || (last.Committed && last.Settled) {
		return role, nil
	}
	if !last.Committed {
		if last.To > 0 {
			if err := s.releaseStrayHold(ctx, slotKeyFor(role.AccountID, last.To)); err != nil {
				return role, err
			}
		}
		if last.Settled {
			return role, nil
		}
	} else {
		_, _, err := s.cfg.Slots.Update(ctx, slotKeyFor(role.AccountID, last.To), func(current Slot, found bool) (Slot, bool, error) {
			if !found || current.TransferID != last.ID || current.PlayerID != role.PlayerID {
				return current, false, nil
			}
			current.TransferID = ""
			return current, true, nil
		})
		if err != nil {
			return role, err
		}
		err = s.deleteSlotIf(ctx, slotKeyFor(role.AccountID, last.From), func(slot Slot) bool {
			return slot.PlayerID == role.PlayerID && slot.Creation.ID == role.CreationID && slot.BindEpoch < last.Epoch
		})
		if err != nil {
			return role, err
		}
	}
	stored, _, err := s.cfg.Roles.Update(ctx, role.PlayerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, role.PlayerID)
		}
		if current.LastTransfer == nil || current.LastTransfer.ID != last.ID || current.LastTransfer.Settled {
			return current, false, nil
		}
		next := current.clone()
		next.LastTransfer.Settled = true
		return next, true, nil
	})
	if err != nil {
		return role, err
	}
	return stored.Value.clone(), nil
}

// settleTransferBestEffort 是读路径上顺带的收尾（SelectRole）：失败只记日志与指标，不影响调用方。
func (s *Service) settleTransferBestEffort(ctx context.Context, role Role) {
	if role.LastTransfer == nil {
		return
	}
	if _, err := s.settleTransfer(ctx, role); err != nil {
		s.report.Dropped("role_transfer.settle_failed", 1)
		slog.Warn("account: finishing a settled role transfer failed; the next SelectRole, Begin or re-sent Commit/Abort retries it",
			"player_id", role.PlayerID, "transfer_id", role.LastTransfer.ID, "err", err)
	}
}

// releaseStrayHold 撤掉 key 上没有角色记录认的换服占位；key 上不是占位、或占位有人认时什么也不做。
// 它是删占位的唯一入口：中止的收尾、迟到的 Begin / Commit 撤自己留下的占位、之后的入口撤别人留下的，
// 都按同一条规则判断，调用方手里的角色记录新旧无关。
//
// 判断用的是读到 slot 之后再读的角色记录（holdStands）。顺序不能反：占位只在角色写下 Transfer 之后才建，
// 所以一个仍然作数的占位，在它之后读到的角色记录一定认它；先读角色、后读 slot，会把读角色之后才发起的
// 那次换服的占位当成没人认的。删除带着读到的版本与占位的身份（DeleteIf）：读与删之间 slot 变了就重读
// 再判一次。
func (s *Service) releaseStrayHold(ctx context.Context, key string) error {
	deleter, ok := s.cfg.Slots.(versionstore.ConditionalDeleter[string, Slot])
	if !ok {
		return fmt.Errorf("account: slot store must support atomic identity-checked DeleteIf")
	}
	var err error
	for range 3 {
		slot, found, getErr := s.cfg.Slots.Get(ctx, key)
		if getErr != nil {
			return getErr
		}
		hold := slot.Value
		if !found || hold.TransferID == "" {
			return nil
		}
		role, roleErr := s.loadRole(ctx, hold.PlayerID)
		if errors.Is(roleErr, ErrRoleMissing) {
			return nil // 占位指向的角色不存在：不是换服能留下的状态，不在这里猜
		}
		if roleErr != nil {
			return roleErr
		}
		if holdStands(role, hold) {
			return nil
		}
		err = deleter.DeleteIf(ctx, key, slot, func(current Slot) bool {
			return current.TransferID == hold.TransferID && current.PlayerID == hold.PlayerID && current.BindEpoch == hold.BindEpoch
		})
		if !errors.Is(err, versionstore.ErrVersionMismatch) {
			if err == nil {
				s.report.Dropped("role_transfer.stray_hold_released", 1)
				slog.Info("account: released a transfer hold no role record stands behind", "player_id", hold.PlayerID, "transfer_id", hold.TransferID, "server_id", hold.ServerID)
			}
			return err
		}
	}
	return fmt.Errorf("%w: slot %s keeps changing: %v", ErrConflict, key, err)
}

// holdStands 判断角色记录是否认一个带 TransferID 的 slot：
//
//   - slot 在角色的绑定服上：它是角色正在用的 slot（换服已经提交、占位标记还没清），永远不在这里删。
//   - 角色在途的换服就是它：同一个 ID、同一个目标服、同一代绑定。
//
// 其余的都没有人认：换服已经中止或撤回，或者角色记录上早已是别的换服。已提交的换服不会落到这里——
// 它的 slot 在角色的绑定服上；角色再换走之前，Begin 先补完上一次的收尾，标记已经清掉。
func holdStands(role Role, hold Slot) bool {
	if hold.ServerID == role.ServerID {
		return true
	}
	flight := role.Transfer
	return flight != nil && flight.ID == hold.TransferID && flight.To == hold.ServerID && flight.Epoch+1 == hold.BindEpoch
}

// deleteSlotIf 删掉 key 上满足 match 的 slot：不存在或不满足时什么也不做。删除带着读到的版本与 match
// 一起比较（DeleteIf）；读与删之间 slot 变了就重读再判一次。
func (s *Service) deleteSlotIf(ctx context.Context, key string, match func(Slot) bool) error {
	deleter, ok := s.cfg.Slots.(versionstore.ConditionalDeleter[string, Slot])
	if !ok {
		return fmt.Errorf("account: slot store must support atomic identity-checked DeleteIf")
	}
	var err error
	for range 3 {
		slot, found, getErr := s.cfg.Slots.Get(ctx, key)
		if getErr != nil {
			return getErr
		}
		if !found || !match(slot.Value) {
			return nil
		}
		if err = deleter.DeleteIf(ctx, key, slot, match); !errors.Is(err, versionstore.ErrVersionMismatch) {
			return err
		}
	}
	return fmt.Errorf("%w: slot %s keeps changing: %v", ErrConflict, key, err)
}

func (s *Service) loadRole(ctx context.Context, playerID int64) (Role, error) {
	role, found, err := s.cfg.Roles.Get(ctx, playerID)
	if err != nil {
		return Role{}, err
	}
	if !found {
		return Role{}, fmt.Errorf("%w: player %d", ErrRoleMissing, playerID)
	}
	return role.Value.clone(), nil
}

// bindingOf 把角色记录变成 RoleBinding，承载服经唯一的解析点 servingServer 给出。
func (s *Service) bindingOf(ctx context.Context, role Role) (RoleBinding, error) {
	serving, err := s.servingServer(ctx, role.ServerID)
	if err != nil {
		return RoleBinding{}, err
	}
	return RoleBinding{
		PlayerID: role.PlayerID, ServerID: role.ServerID, ServingServerID: serving, BindEpoch: role.BindEpoch,
		TransferSeq: role.TransferSeq, Transfer: cloneOf(role.Transfer), LastTransfer: cloneOf(role.LastTransfer),
	}, nil
}

// withBinding 让一个拒绝带上当前的绑定。绑定解析不了时只返回拒绝本身。
func (s *Service) withBinding(ctx context.Context, role Role, refusal error) (RoleBinding, error) {
	binding, err := s.bindingOf(ctx, role)
	if err != nil {
		return RoleBinding{}, refusal
	}
	return binding, refusal
}

func validTransferID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" || len(id) > MaxTransferIDBytes {
		return "", fmt.Errorf("%w: transfer id is %d bytes, want 1 to %d", ErrRangeInvalid, len(id), MaxTransferIDBytes)
	}
	return id, nil
}

// boundedNote 是总线调用方的备注 / 原因：可空，只限制长度。
func boundedNote(raw string) (string, error) {
	note := strings.TrimSpace(raw)
	if len(note) > MaxAdminNoteBytes {
		return "", fmt.Errorf("%w: note is %d bytes, limit %d", ErrRangeInvalid, len(note), MaxAdminNoteBytes)
	}
	return note, nil
}
