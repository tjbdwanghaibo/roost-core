package nest

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

var (
	// ErrPeerGroupChangeUnavailable：不是当前单一显式目标的业务事务，或该目标/提交协议不支持变更。
	ErrPeerGroupChangeUnavailable = errors.New("nest: peer group change unavailable in this handler")
	// ErrPeerGroupChangeBusy 可识别的 Try 预留冲突；不自动重放业务 handler。
	ErrPeerGroupChangeBusy              = errors.New("nest: peer group change reservation busy")
	ErrIndependentGroupRequestInHandler = errors.New("nest: independent group requests are not allowed in a business transaction; use JoinEntityGroup or LeaveEntityGroup")
)

// 帧不进入 fctx、消息克隆或异步完成泵。每次调用在业务代码之前冻结显式目标，
// 不能由业务改写参数切片、Cast 到另一个实例或另开 Guard 来换目标。
type nestDispatchFrame struct {
	msg     *Msg
	guard   *entity.GuardScope
	ctx     *fctx.Context
	subject entity.IThreadSafeEntity
	change  *peerGroupChange
	policy  DurabilityPolicy
}

// participants 已取得锁，但可能包含组或伴生域的隐式成员；唯一性的依据是原消息。
func peerHandlerSubject(msg *Msg, participants []entity.IThreadSafeEntity) entity.IThreadSafeEntity {
	if msg == nil || msg.Type == MsgTypeBroadcast || len(participants) == 0 {
		return nil
	}
	var id int64
	switch msg.Type {
	case MsgTypeSingle, MsgTypeTryBatch:
		id = msg.Tid
	case MsgTypeMulti, MsgTypeMultiGroup:
		one := func(ids []int64) bool {
			for _, next := range ids {
				if next == 0 || id != 0 && next != id {
					return false
				}
				id = next
			}
			return true
		}
		if !one(msg.Tids) {
			return nil
		}
		for _, group := range msg.GroupTIds {
			if !one(group) {
				return nil
			}
		}
	default:
		return nil
	}
	if id == 0 {
		return nil
	}
	for _, e := range participants {
		if e != nil && e.GUId() == id {
			return e
		}
	}
	return nil
}

// 这里的门仅排斥独立异步请求，不是权限凭据，也不改变 handler 仍可读取的已提交组视图。
var peerReservations = struct {
	sync.Mutex
	active map[entity.IThreadSafeEntity]*peerGroupChange
}{active: make(map[entity.IThreadSafeEntity]*peerGroupChange)}

func beginIndependentGroupChange(e entity.IThreadSafeEntity, state entity.EntityGroupTransitionState, target int64) bool {
	peerReservations.Lock()
	defer peerReservations.Unlock()
	return peerReservations.active[e] == nil && e.Base().BeginGroupTransition(entitypermit.System(), state, target)
}

// JoinEntityGroup 在当前业务事务确定成功后，把单一显式本地目标加入 groupID。
// 仅可从未入组状态加入；不把 Join 当成跨组 Move，也不改变当前调用的授权范围。
func JoinEntityGroup(groupID int64) error {
	if groupID == 0 {
		return ErrInvalidEntityLockGroup
	}
	return stagePeerGroupChange(groupID)
}

// LeaveEntityGroup 在当前业务事务确定成功后，让单一显式本地目标离开其原组。
func LeaveEntityGroup() error { return stagePeerGroupChange(0) }

type peerGroupChange struct {
	entity      entity.IThreadSafeEntity
	old, target int64
	epoch       uint64
	store       entityGroupStore
	mgr         *NestMgr
	guard       *entity.GuardScope
	unlock      func()
	cont        *GroupTransitionContinuation
}

// holdsPeerChangeSubject is for ReleaseCast: an explicit target's dispatch lock
// is not a transient Cast borrow, even before this worker stages a Join.
func holdsPeerChangeSubject(e entity.IThreadSafeEntity) bool {
	f := currentNestDispatchFrame()
	return f != nil && f.guard != nil && f.subject == e && f.guard == entity.CurrentGuardScope()
}

func checkPeerGroupKind(e entity.IThreadSafeEntity) error {
	if companionRemote(e) {
		return errors.Join(ErrPeerGroupUnsupported, entity.ErrEntityGroupDomain)
	}
	if entity.IsCompanionParticipant(e.GetEntityKind()) {
		if err := entity.CheckEntityGroupMember(e); err != nil {
			return errors.Join(ErrPeerGroupUnsupported, err)
		}
	}
	return nil
}

func stagePeerGroupChange(target int64) error { return stagePeerGroupTransition(target, false) }

func stagePeerGroupTransition(target int64, move bool) error {
	frame := currentNestDispatchFrame()
	if frame == nil || frame.guard == nil || frame.guard != entity.CurrentGuardScope() || frame.ctx != fctx.CurrentContext() || frame.subject == nil || frame.msg.engine == nil || !frame.msg.txInFlight || len(frame.msg.handlerEntities) == 0 {
		return ErrPeerGroupChangeUnavailable
	}
	msg, e := frame.msg, frame.subject
	if tx := CurrentRollbackTx(); tx != nil && tx.dispatch != msg || frame.policy == DurabilityPipelined || msg.HasRemote || msg.RemoteWriteBatch != nil || !frame.guard.Guard().GuardedEntity(e) {
		return ErrPeerGroupChangeUnavailable
	}
	if frame.change != nil {
		return ErrPeerGroupChangeBusy
	}
	if err := entity.CheckEntityGroupMember(e); err != nil {
		return errors.Join(ErrPeerGroupUnsupported, err)
	}
	if err := msg.engine.FenceError(); err != nil {
		return err
	}
	b := e.Base()
	old := b.GroupLockID()
	if !move && (target == 0) == (old == 0) || move && (target == 0 || old == 0 || old == target) {
		return ErrInvalidEntityLockGroup
	}
	store := groupStoreOf(msg.engine.getter)
	if store == nil || old != 0 && store.GetGroupEntity(old, e.GUId()) != e {
		return ErrPeerGroupChangeUnavailable
	}
	// 已有实体锁：只 Try 预留，绝不持实体等另一 worker 的组锁。Leave 重入当前已持组锁。
	groupID := target
	if groupID == 0 {
		groupID = old
	}
	unlock, ok := msg.engine.tryLockEntityLockGroups([]int64{groupID})
	if !ok {
		return ErrPeerGroupChangeBusy
	}
	ch := &peerGroupChange{entity: e, old: old, target: target, epoch: b.GroupEpoch(), store: store, mgr: msg.engine, guard: frame.guard, unlock: unlock}
	taken := false
	defer func() {
		if !taken {
			unlock()
		}
	}()
	if err := ch.validate(); err != nil {
		return err
	}
	// 对已有的自定义 store 不要求换接口；no-op 让 store 自己在其 Manager 下核对实例归属。
	if err := store.UpdateEntityGroup(entitypermit.System(), e, old); err != nil {
		return err
	}
	peerReservations.Lock()
	defer peerReservations.Unlock()
	if peerReservations.active[e] != nil || b.GroupTransitionPending() {
		return ErrPeerGroupChangeBusy
	}
	peerReservations.active[e], frame.change, taken = ch, ch, true
	return nil
}

func (c *peerGroupChange) validate() error {
	if c == nil {
		return nil
	}
	if c.guard != entity.CurrentGuardScope() || !c.guard.Guard().GuardedEntity(c.entity) {
		return ErrPeerGroupChangeUnavailable
	}
	if err := c.mgr.FenceError(); err != nil {
		return fmt.Errorf("%w: peer relation not published (%v)", ErrNestFenced, err)
	}
	if err := entity.CheckEntityGroupMember(c.entity); err != nil {
		return err
	}
	b := c.entity.Base()
	if b.GroupTransitionPending() || b.GroupEpoch() != c.epoch || b.GroupLockID() != c.old {
		return ErrPeerGroupChangeBusy
	}
	return nil
}

func (c *peerGroupChange) publish() error {
	if c == nil {
		return nil
	}
	if err := c.validate(); err != nil {
		return err
	}
	if err := c.store.UpdateEntityGroup(entitypermit.System(), c.entity, c.target); err != nil {
		return err
	}
	if c.cont != nil {
		msg := currentNestDispatchMsg()
		msg.peerContinuation = &GroupTransitionRequest{EntityID: c.entity.GUId(), Continuation: c.cont}
	}
	return nil
}

func peerContinuationOutcome(f *nestDispatchFrame, err error) error {
	if f != nil && f.change != nil && f.change.cont != nil {
		if f.msg.awaitPlan != nil {
			return errors.Join(ErrIndependentGroupRequestInHandler, err)
		}
		if err == ErrEntityGroupTransitionScheduled {
			return nil
		}
	}
	return err
}

func (f *nestDispatchFrame) finishPeerChange() {
	if c := f.change; c != nil {
		peerReservations.Lock()
		delete(peerReservations.active, c.entity)
		peerReservations.Unlock()
		f.change = nil
		c.unlock()
	}
	f.guard, f.subject, f.ctx = nil, nil, nil
}
