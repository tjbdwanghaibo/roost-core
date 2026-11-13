package cellworld

import (
	"context"
	"errors"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// PendingOwner 存在 Actor 的 DAO 中。所有权 I/O 只携带值副本，完成时必须核对同一 Token；
// 队列满、提交回滚或回调投递失败都由 Pending / Frozen / Gone 的心跳重试，不遗失收尾责任。
type PendingOwner struct {
	Token    string        `msgpack:"id"`
	Transfer OwnerTransfer `msgpack:"t"`
}

type ownerWorkKind uint8

const (
	ownerClaimWork ownerWorkKind = iota + 1
	ownerAcceptWork
	ownerBeginWork
	ownerRejectWork
	ownerRetireWork
	ownerDeathWork
)

type ownerWork struct {
	kind     ownerWorkKind
	state    ActorState
	transfer OwnerTransfer
}
type ownerResult struct {
	work   ownerWork
	record OwnerRecord
	err    error
}

func (n *Node) ownerProcess() OwnerProcess {
	return OwnerProcess{SID: n.cfg.SID, Incarnation: n.cfg.Incarnation}
}
func (n *Node) ownerTransfer(st ActorState) OwnerTransfer {
	if st.Handoff == nil {
		return OwnerTransfer{}
	}
	return OwnerTransfer{ID: st.Handoff.ID, Target: n.cfg.Grid.SID(st.Handoff.Target), Epoch: st.Handoff.Epoch}
}
func (n *Node) ownerWork(st ActorState) ownerWork {
	w := ownerWork{state: st}
	switch st.Role {
	case RoleDying:
		w.kind = ownerDeathWork
	case RolePending:
		w.kind = ownerClaimWork
		if st.Pending != nil && st.Pending.Transfer.ID != "" {
			w.kind = ownerAcceptWork
			w.transfer = st.Pending.Transfer
		}
	case RoleFrozen:
		w.kind = ownerBeginWork
		w.transfer = n.ownerTransfer(st)
	case RoleGone:
		w.kind = ownerRetireWork
	}
	return w
}

func (n *Node) enqueueOwner(w ownerWork) {
	n.ownerMu.Lock()
	defer n.ownerMu.Unlock()
	if n.ownerBusy[w.state.ID] {
		return
	}
	select {
	case n.ownerQueue <- w:
		n.ownerBusy[w.state.ID] = true
	default:
		n.stats.ownershipQueueFull.Add(1)
	}
}

// runOwners 不读取实体，不运行玩法。存储请求结束后只把不可变结果投回 Nest；
// 同一 ID 同时最多一个 I/O，避免慢存储时心跳重试填满整个队列。
func (n *Node) runOwners(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-n.ownerQueue:
			opctx, cancel := context.WithTimeout(ctx, n.cfg.OwnershipTimeout)
			n.stats.ownershipAttempts.Add(1)
			record, err := n.executeOwner(opctx, w)
			if errors.Is(err, ErrOwnerConflict) || errors.Is(err, ErrOwnerClosed) {
				n.stats.ownershipConflicts.Add(1)
			} else if err != nil {
				n.stats.ownershipErrors.Add(1)
			}
			cancel()
			if ctx.Err() == nil {
				result := ownerResult{work: w, record: record, err: err}
				if err := n.cfg.Nest.DispatchMulti(ctx, hOwnerResult, []int64{n.cfg.Scene, w.state.ID}, nest.Params{result}); err != nil {
					n.stats.dispatchErrors.Add(1)
				}
			}
			n.ownerMu.Lock()
			delete(n.ownerBusy, w.state.ID)
			n.ownerMu.Unlock()
		}
	}
}

func (n *Node) executeOwner(ctx context.Context, w ownerWork) (OwnerRecord, error) {
	if err := n.checkLease(); err != nil {
		return OwnerRecord{}, err
	}
	st := w.state
	switch w.kind {
	case ownerDeathWork:
		if st.Death == nil {
			return OwnerRecord{}, ErrOwnerConflict
		}
		return n.cfg.Owners.recordDeath(ctx, st.Grant, *st.Death)
	case ownerClaimWork:
		return n.cfg.Owners.claim(ctx, ownerClaim{Generation: st.SpawnGeneration, Actor: st.ID, Kind: st.Kind, Base: st.Base, Entry: st.Entry, Process: n.ownerProcess(), Token: st.Grant.ID, Epoch: st.Ver.Epoch})
	case ownerAcceptWork:
		return n.cfg.Owners.accept(ctx, st.ID, st.Grant, w.transfer, n.ownerProcess())
	case ownerBeginWork:
		return n.cfg.Owners.begin(ctx, st.ID, st.Grant, w.transfer)
	case ownerRejectWork:
		return n.cfg.Owners.reject(ctx, st.ID, st.Grant, w.transfer, n.ownerProcess())
	case ownerRetireWork:
		if st.Grant.ID == "" {
			return OwnerRecord{}, nil
		}
		closed := uint64(0)
		if st.CloseEntry {
			closed = st.Entry
		}
		return n.cfg.Owners.retireEntry(ctx, st.ID, st.Grant, closed)
	}
	return OwnerRecord{}, ErrOwnerConflict
}

func (n *Node) handleOwnerResult(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	result := params[0].(ownerResult)
	w, record := result.work, result.record
	fx := &txEffects{}
	// 未知结论不激活、不恢复；心跳以原身份重试。明确冲突只允许放弃本地未激活 / 已冻结的实例。
	conflict := errors.Is(result.err, ErrOwnerConflict) || errors.Is(result.err, ErrOwnerClosed)
	if result.err != nil && !conflict {
		return nil, nil
	}
	if w.kind == ownerRejectWork {
		if result.err == nil {
			fx.send(w.state.Grant.Process.SID, &wireMsg{Kind: kindOfferReply, OfferReply: &handoffReply{ID: w.state.ID, Epoch: w.transfer.Epoch}})
		}
		n.registerCommit(fx)
		return nil, nil
	}
	a := actorOf(es, w.state.ID)
	if a == nil {
		return nil, nil
	}
	st := a.CellState()
	now := time.Now()
	switch w.kind {
	case ownerDeathWork:
		if st.Death == nil || w.state.Death == nil || st.Death.ID != w.state.Death.ID || st.Grant != w.state.Grant {
			return nil, nil
		}
		if result.err == nil {
			n.completeDeath(r, a, st, record, now, fx)
		} else if st.Role == RoleDying || st.Role == RoleReal {
			// 明确失去授权的旧实例不能永久停在 Dying，也不能恢复写入。
			// 只移除本地旧版本；新写者的共享记录由 retire 的 Grant CAS 保护。
			prev := st
			st.Role = RoleGone
			n.setState(a, prev, st)
			n.realChanged(st.ID, st.Ver.Epoch, false, fx)
			n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true}}, now, &fx.sends)
			fx.owners = append(fx.owners, n.ownerWork(st))
		}
	case ownerClaimWork, ownerAcceptWork:
		if st.Role != RolePending || st.Pending == nil || w.state.Pending == nil || *st.Pending != *w.state.Pending {
			return nil, nil
		}
		expected := st.Pending.Token
		if w.kind == ownerClaimWork {
			expected = st.Grant.ID
		}
		if result.err == nil && record.Phase == ownerActive && record.Grant.ID == expected && record.Grant.Process == n.ownerProcess() {
			source := st.Grant.Process.SID
			st.Grant = record.Grant
			st.Life = record.Life
			st.Death = nil
			st.DeathAcked = record.DeathAcked
			if record.Death.ID != "" {
				fact := record.Death
				st.Death = &fact
				st.HP = 0
			}
			st.Ver.Epoch = max(st.Ver.Epoch, record.Grant.Epoch)
			if st.CloseEntry {
				st.Role, st.Pending = RoleGone, nil
				a.SetCellState(st)
				r.locals.del(st.ID)
				fx.owners = append(fx.owners, n.ownerWork(st))
				break
			}
			n.install(r, a, st, now, fx)
			if w.kind == ownerAcceptWork {
				fx.send(source, &wireMsg{Kind: kindOfferReply, OfferReply: &handoffReply{ID: st.ID, Epoch: st.Ver.Epoch, Accepted: true}})
			}
		} else {
			if st.Kind == KindPlayer && st.Base != 0 {
				fx.send(st.Base, &wireMsg{Kind: kindEnterRefused, EnterRefused: &enterRefused{ID: st.ID, Hint: record.Grant.Process.SID}})
			}
			if w.kind == ownerClaimWork && st.Grant.Epoch != 0 {
				// 本实例已持有授权（同进程现存 Real 的重新入场被拒，例如死亡回执未确认）：它在转 Pending 时已停写，
				// 必须按自己的 Grant 退役后再销毁，否则共享记录永远指向一个不存在的本进程实例。
				// 退役是 Grant CAS：期间授权若被别人接手（本进程被判死），冲突结论只删本地实例，不碰新写者。
				prev := st
				st.Role, st.Pending = RoleGone, nil
				st.Ver.Seq++
				n.setState(a, prev, st)
				n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true}}, now, &fx.sends)
				fx.owners = append(fx.owners, n.ownerWork(st))
				break
			}
			// 未取得授权的临时实例不能退役别人的 Grant。
			st.Role, st.Pending, st.Grant = RoleGone, nil, OwnerGrant{}
			a.SetCellState(st)
			r.locals.del(st.ID)
			fx.destroys = append(fx.destroys, a)
		}
	case ownerBeginWork:
		if st.Role != RoleFrozen || st.Handoff == nil || n.ownerTransfer(st) != w.transfer || st.Grant != w.state.Grant {
			return nil, nil
		}
		if result.err == nil && record.Grant == st.Grant && record.Phase == ownerFrozen && record.Transfer == w.transfer {
			fx.sends = append(fx.sends, n.offerTo(&st))
		} else if result.err == nil && record.Grant == st.Grant && record.Phase == ownerActive && record.Transfer == w.transfer {
			n.handoffConcluded(st, false, now, fx)
			prev := st
			st.Role, st.Handoff = RoleReal, nil
			st.RetryAfter = now.Add(n.cfg.RejectBackoff).UnixNano()
			n.setState(a, prev, st)
			n.stats.handoffsRejected.Add(1)
			n.realChanged(st.ID, st.Ver.Epoch, true, fx)
			n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleReal}}, now, &fx.sends)
		} else {
			// 授权已交走 / 关闭，旧实例只能退出；即使确认丢失，也不根据超时恢复写入。
			n.handoffConcluded(st, true, now, fx)
			handoff := st.Handoff
			prev := st
			st.Role, st.Handoff = RoleGone, nil
			n.setState(a, prev, st)
			n.applyReports(r, []ActorReport{{State: st.EntityState, Role: RoleGone, Removed: true, Handoff: handoff}}, now, &fx.sends)
			fx.owners = append(fx.owners, n.ownerWork(st))
		}
	case ownerRetireWork:
		if st.Role != RoleGone || st.Grant != w.state.Grant || st.Entry != w.state.Entry {
			return nil, nil
		}
		// CAS 冲突表示该 Grant 已不再是权威，不会删除新写者的记录。
		fx.destroys = append(fx.destroys, a)
	}
	n.registerCommit(fx)
	return nil, nil
}

func (n *Node) checkLease() error {
	if n.cfg.CheckLease != nil {
		return n.cfg.CheckLease()
	}
	return nil // Base-only；Cell 配置在 NewNode 已拒绝缺失 guard。
}

// guardTransaction 覆盖入口及提交前；提交后结果与出站还会再次核验。
// Node 是可比较的参与者，同一事务的多个 ActorBeat 只登记一次。
func (n *Node) guardTransaction() error {
	if err := n.checkLease(); err != nil {
		return err
	}
	return n.beginTransaction()
}
