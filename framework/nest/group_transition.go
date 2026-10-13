package nest

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const (
	// entityGroupDispatchRequeueDelay 是暂时性错误重新准入延迟的下限，实际延迟见 transientRequeueDelay。
	entityGroupDispatchRequeueDelay = 5 * time.Millisecond
	// entityGroupDispatchRequeueMax 是一条消息因暂时性错误被框架自动重新准入的次数上限。
	entityGroupDispatchRequeueMax = 400

	// 锁组迁移取不到组锁 / 实体锁时按固定间隔重试，受次数与截止时间双重限制。
	entityGroupTransitionRetryDelay  = 5 * time.Millisecond
	entityGroupTransitionRetryMax    = 400
	entityGroupTransitionLockTimeout = 2 * time.Second
)

// GroupTransitionRequest 是一次实体锁组迁移（加入 / 离开 / 移动）的内部消息内容，重试时原样复用。
type GroupTransitionRequest struct {
	EntityID      int64
	TargetGroupID int64
	State         entity.EntityGroupTransitionState
	Attempts      int
	Deadline      time.Time
	Continuation  *GroupTransitionContinuation
}

// GroupTransitionContinuation 是迁移成功后要派发的业务消息；它接管发起方消息的回复通道。
type GroupTransitionContinuation struct {
	Handler string
	Params  []any
	RetChan chan any
	Context fctx.ContextSnapshot
}

// GroupTransitionOptions 是迁移请求的可选项。
type GroupTransitionOptions struct {
	Continuation *GroupTransitionContinuation
}

// GroupTransitionOption 配置迁移请求。
type GroupTransitionOption func(*GroupTransitionOptions)

// GroupTransitionWithContinuation 让迁移成功后以 name/params 派发一条单目标消息，并把当前消息的回复交给它；
// 迁移失败时当前消息的调用方收到迁移错误。只能在带回复通道的派发中使用。
func GroupTransitionWithContinuation(name HandlerName, params Params) GroupTransitionOption {
	return func(opts *GroupTransitionOptions) {
		opts.Continuation = &GroupTransitionContinuation{
			Handler: name.String(),
			Params:  []any(params),
		}
	}
}

// RequestJoinEntityLockGroup 请求把实体加入锁组 groupID。请求异步执行；实体已有待定迁移时返回
// ErrEntityGroupTransitionPending。
func (mgr *NestMgr) RequestJoinEntityLockGroup(entityID int64, groupID int64, opts ...GroupTransitionOption) error {
	if groupID == 0 {
		return ErrInvalidEntityLockGroup
	}
	return mgr.requestEntityLockGroupTransition(entityID, entity.EntityGroupTransitionJoin, groupID, opts...)
}

// RequestLeaveEntityLockGroup 请求让实体离开当前锁组，语义同 RequestJoinEntityLockGroup。
func (mgr *NestMgr) RequestLeaveEntityLockGroup(entityID int64, opts ...GroupTransitionOption) error {
	return mgr.requestEntityLockGroupTransition(entityID, entity.EntityGroupTransitionLeave, 0, opts...)
}

// RequestMoveEntityLockGroup 请求把实体从当前锁组移到 groupID，语义同 RequestJoinEntityLockGroup。
func (mgr *NestMgr) RequestMoveEntityLockGroup(entityID int64, groupID int64, opts ...GroupTransitionOption) error {
	if groupID == 0 {
		return ErrInvalidEntityLockGroup
	}
	return mgr.requestEntityLockGroupTransition(entityID, entity.EntityGroupTransitionMove, groupID, opts...)
}

// requestEntityLockGroupTransition 在实体上登记待定迁移（此后该实体的普通派发以 ErrEntityGroupTransitionPending
// 重新准入），再派发一条迁移消息。
func (mgr *NestMgr) requestEntityLockGroupTransition(entityID int64, state entity.EntityGroupTransitionState, targetGroupID int64, opts ...GroupTransitionOption) error {
	if mgr == nil || mgr.dispatcher == nil || mgr.getter == nil {
		return ErrNestStopped
	}
	fullID, err := entity.NormalizeFullID(entityID, entity.EntityKindNone)
	if err != nil {
		return err
	}
	meta := entity.ResolveEntityID(fullID)
	ent, err := mgr.dispatchGetter().Get(nestBaseContext(), meta.FullID, meta.Category)
	if err != nil {
		return err
	}
	if ent == nil || ent.Base() == nil {
		return ErrEntityNotFound
	}
	if !ent.Base().BeginGroupTransition(state, targetGroupID) {
		return ErrEntityGroupTransitionPending
	}
	opt := &GroupTransitionOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(opt)
		}
	}
	if opt.Continuation != nil {
		if err := bindGroupTransitionContinuation(opt.Continuation); err != nil {
			ent.Base().ClearGroupTransition()
			return err
		}
	}

	msg := GenMsg(MsgTypeGroupTransition)
	msg.Tid = meta.FullID
	msg.Name = "entity_lock_group_transition"
	msg.GroupTransition = &GroupTransitionRequest{
		EntityID:      meta.FullID,
		TargetGroupID: targetGroupID,
		State:         state,
		Deadline:      time.Now().Add(entityGroupTransitionLockTimeout),
		Continuation:  opt.Continuation,
	}
	bindMsgContext(msg, false)
	observeEntityGroupTransition(msg.GroupTransition, "request")
	mgr.dispatcher.sendMsg(msg)
	return nil
}

// groupTransitionDispatch 执行迁移消息：TryLock 涉及的组锁与实体锁（取不到则延时重试），确认迁移仍是待定的那一次，
// 更新实体的锁组并清除待定标记，成功后派发续行。失败时把错误回复给续行的调用方。
func (mgr *NestMgr) groupTransitionDispatch(req *GroupTransitionRequest) (ret any, err error) {
	defer func() {
		if err != nil && !errors.Is(err, ErrEntityGroupTransitionScheduled) {
			completeGroupTransitionContinuation(req, err)
		}
	}()
	if mgr == nil || mgr.getter == nil || req == nil {
		return nil, ErrNestStopped
	}
	// 取锁用 runNestLogic 作用域里的 Guard；放在取组锁之前，没有作用域时不留任何副作用。
	guard, err := dispatchScopeGuard()
	if err != nil {
		return nil, err
	}
	fullID, err := entity.NormalizeFullID(req.EntityID, entity.EntityKindNone)
	if err != nil {
		return nil, err
	}
	meta := entity.ResolveEntityID(fullID)
	ent, err := mgr.dispatchGetter().Get(nestBaseContext(), meta.FullID, meta.Category)
	if err != nil {
		return nil, err
	}
	if ent == nil || ent.Base() == nil || !ent.Touch() {
		return nil, ErrEntityNotFound
	}
	defer ent.UnTouch()

	groupIDs := transitionLockGroupIDs(ent.Base().GroupLockID(), req.TargetGroupID, req.State)
	unlockGroups, ok := mgr.tryLockEntityLockGroups(groupIDs)
	if !ok {
		return nil, mgr.retryGroupTransition(req, "group_lock_busy")
	}
	defer unlockGroups()

	acquired, err := tryLockDispatchEntities(guard, []entity.IThreadSafeEntity{ent})
	if err != nil {
		return nil, mgr.retryGroupTransition(req, "entity_lock_busy")
	}
	defer releaseDispatchEntities(guard, acquired)

	if ent.Base().GroupTransitionState() != req.State ||
		ent.Base().GroupTransitionTargetID() != req.TargetGroupID {
		return nil, fmt.Errorf("%w: stale transition", ErrEntityGroupTransitionPending)
	}
	groups := groupStoreOf(mgr.getter)
	if groups == nil {
		ent.Base().ClearGroupTransition()
		return nil, ErrEntityNotFound
	}

	targetGroupID := req.TargetGroupID
	switch req.State {
	case entity.EntityGroupTransitionJoin, entity.EntityGroupTransitionMove:
	case entity.EntityGroupTransitionLeave:
		targetGroupID = 0
	default:
		ent.Base().ClearGroupTransition()
		return nil, ErrInvalidEntityLockGroup
	}
	if err := groups.UpdateEntityGroup(ent, targetGroupID); err != nil {
		ent.Base().ClearGroupTransition()
		return nil, err
	}
	ent.Base().ClearGroupTransition()
	observeEntityGroupTransition(req, "success")
	mgr.dispatchGroupTransitionContinuation(req)
	return nil, nil
}

// retryGroupTransition 延时重发同一迁移请求并返回 ErrEntityGroupTransitionScheduled（不回复）；次数或截止时间
// 耗尽时撤销待定标记并返回 ErrLockTimeout。
func (mgr *NestMgr) retryGroupTransition(req *GroupTransitionRequest, reason string) error {
	if mgr == nil || req == nil {
		return ErrNestStopped
	}
	if req.Deadline.IsZero() {
		req.Deadline = time.Now().Add(entityGroupTransitionLockTimeout)
	}
	req.Attempts++
	if req.Attempts >= entityGroupTransitionRetryMax || time.Now().After(req.Deadline) {
		err := fmt.Errorf("%w: entity_group_transition %s", ErrLockTimeout, reason)
		mgr.abortGroupTransition(req)
		observeEntityGroupTransition(req, "timeout")
		return err
	}
	if mgr.dispatcher == nil {
		return ErrNestStopped
	}
	observeEntityGroupTransition(req, "retry")
	msg := GenMsg(MsgTypeGroupTransition)
	msg.Tid = req.EntityID
	msg.Name = "entity_lock_group_transition"
	msg.GroupTransition = req
	if cur := currentNestDispatchMsg(); cur != nil {
		msg.Context = cur.Context.Clone()
	}
	mgr.dispatcher.delaySendMsg(entityGroupTransitionRetryDelay, msg)
	return ErrEntityGroupTransitionScheduled
}

// abortGroupTransition 撤销实体上仍属于本请求的待定迁移标记。
func (mgr *NestMgr) abortGroupTransition(req *GroupTransitionRequest) {
	if mgr == nil || mgr.getter == nil || req == nil {
		return
	}
	fullID, err := entity.NormalizeFullID(req.EntityID, entity.EntityKindNone)
	if err != nil {
		return
	}
	meta := entity.ResolveEntityID(fullID)
	ent, err := mgr.dispatchGetter().Get(nestBaseContext(), meta.FullID, meta.Category)
	if err != nil || ent == nil || ent.Base() == nil {
		return
	}
	if ent.Base().GroupTransitionState() == req.State &&
		ent.Base().GroupTransitionTargetID() == req.TargetGroupID {
		ent.Base().ClearGroupTransition()
	}
}

// bindGroupTransitionContinuation 把当前消息的回复通道与上下文转给续行，当前消息此后不再回复。
func bindGroupTransitionContinuation(cont *GroupTransitionContinuation) error {
	if cont == nil {
		return nil
	}
	cur := currentNestDispatchMsg()
	if cur == nil || cur.RetChan == nil {
		return ErrInvalidEntityLockGroup
	}
	cont.RetChan = cur.RetChan
	cont.Context = cur.Context.Clone()
	cur.RetChan = nil
	return nil
}

// completeGroupTransitionContinuation 迁移失败时把 err 回复给续行的调用方（至多一次）。
func completeGroupTransitionContinuation(req *GroupTransitionRequest, err error) {
	if req == nil || req.Continuation == nil || req.Continuation.RetChan == nil || err == nil {
		return
	}
	req.Continuation.RetChan <- err
	req.Continuation.RetChan = nil
}

// dispatchGroupTransitionContinuation 迁移成功后以续行的 handler、参数与回复通道派发单目标消息。
func (mgr *NestMgr) dispatchGroupTransitionContinuation(req *GroupTransitionRequest) {
	if mgr == nil || mgr.dispatcher == nil || req == nil || req.Continuation == nil {
		return
	}
	cont := req.Continuation
	if cont.Handler == "" || cont.RetChan == nil {
		return
	}
	msg := GenMsg(MsgTypeSingle)
	msg.Tid = req.EntityID
	msg.Name = cont.Handler
	msg.Params = cont.Params
	msg.RetChan = cont.RetChan
	msg.Context = cont.Context.Clone()
	checkRemoteId(msg, req.EntityID)
	cont.RetChan = nil
	mgr.dispatcher.sendMsg(msg)
}

// transitionLockGroupIDs 返回迁移需要持有的组锁（升序、去重、不含 0）：加入取目标组，离开取当前组，移动取两者。
func transitionLockGroupIDs(currentGroupID int64, targetGroupID int64, state entity.EntityGroupTransitionState) []int64 {
	ids := make([]int64, 0, 2)
	add := func(id int64) {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	switch state {
	case entity.EntityGroupTransitionJoin:
		add(targetGroupID)
	case entity.EntityGroupTransitionLeave:
		add(currentGroupID)
	case entity.EntityGroupTransitionMove:
		add(currentGroupID)
		add(targetGroupID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// tryLockEntityLockGroups 按顺序 TryLock 全部组锁，任何一把取不到就逆序放掉已取的并返回 false；
// 成功时返回逆序释放的函数。
func (mgr *NestMgr) tryLockEntityLockGroups(groupIDs []int64) (func(), bool) {
	locks := mgr.groupLockManager()
	if locks == nil {
		return func() {}, false
	}
	locked := make([]*entityLockGroupLockEntry, 0, len(groupIDs))
	releaseLocked := func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locks.release(locked[i])
		}
	}
	for _, groupID := range groupIDs {
		if groupID == 0 {
			continue
		}
		entry, ok := locks.acquire(groupID)
		if !ok {
			releaseLocked()
			return func() {}, false
		}
		locked = append(locked, entry)
	}
	return releaseLocked, true
}

// rejectPendingEntityGroupTransition 在派发取锁前拒绝有待定锁组迁移的目标，消息随后重新准入。
func rejectPendingEntityGroupTransition(es []entity.IThreadSafeEntity) error {
	for _, ent := range es {
		if ent != nil && ent.Base() != nil && ent.Base().GroupTransitionPending() {
			return ErrEntityGroupTransitionPending
		}
	}
	return nil
}

// requeuePendingEntityGroupTransition 让因目标待定迁移而失败的普通消息延时重新准入。
func requeuePendingEntityGroupTransition(mgr *NestMgr, msg *Msg, err error) bool {
	if mgr == nil || mgr.dispatcher == nil || msg == nil || !errorsIsEntityGroupPending(err) {
		return false
	}
	if msg.Type == MsgTypeGroupTransition {
		return false
	}
	return requeueNestDispatch(mgr, msg, "group_transition_pending")
}

// requeueTransientDispatch 让因锁超时 / 锁组变化失败的普通消息延时重新准入。
func requeueTransientDispatch(mgr *NestMgr, msg *Msg, err error) bool {
	reason, ok := transientDispatchRequeueReason(err)
	if mgr == nil || mgr.dispatcher == nil || msg == nil || !ok {
		return false
	}
	if msg.Type == MsgTypeGroupTransition {
		return false
	}
	return requeueNestDispatch(mgr, msg, reason)
}

// isRequeueableDispatchError 判断 err 是否属于 dispatchNest 会自动重新准入的错误类别（组迁移待定或暂时性锁错误）。
func isRequeueableDispatchError(err error) bool {
	_, transient := transientDispatchRequeueReason(err)
	return transient || errorsIsEntityGroupPending(err)
}

// transientDispatchRequeueReason 返回暂时性错误的重排原因（指标标签）；不是暂时性错误时返回 false。
func transientDispatchRequeueReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrLockTimeout):
		return "lock_timeout", true
	case errors.Is(err, ErrEntityLockGroupChanged):
		return "group_changed", true
	default:
		return "", false
	}
}

// requeueNestDispatch 克隆消息并延时重新准入，回复通道转给克隆；达到 entityGroupDispatchRequeueMax 次后不再重排。
func requeueNestDispatch(mgr *NestMgr, msg *Msg, reason string) bool {
	if mgr == nil || mgr.dispatcher == nil || msg == nil || msg.PendingRequeues >= entityGroupDispatchRequeueMax {
		return false
	}
	next := msg.Clone()
	next.RefCount = 0
	next.PendingRequeues = msg.PendingRequeues + 1
	msg.RetChan = nil
	metrics.IncCounter("nest.dispatch.requeue.total", metrics.Labels{
		"reason": reason,
		"type":   msg.Type.String(),
	}, 1)
	mgr.dispatcher.delaySendMsg(transientRequeueDelay(), next)
	return true
}

// transientRequeueDelay 返回一次重新准入的延迟：下限 entityGroupDispatchRequeueDelay，加 [0, 下限) 的均匀抖动。
//
// 抖动用来打破对称冲突。交叉创建、交叉 Cast 这类冲突里两条消息互相持有对方要的锁、一起回滚，
// 固定延迟会让它们在同一时刻重新准入；延迟队列只有一个定时器，醒来晚了会把所有已到期消息背靠背放出，两侧残留的
// 错开也被抹平，于是每一轮都重演同一冲突，只能等调度噪声偶然错开（生成工程实测正常负载下 35%～53% 的运行耗尽 400 次上限）。
// 每次独立取样后，两侧下一轮仍落进同一冲突窗口的概率小于 1 且与上一轮无关，连续 400 轮都撞上的概率可以忽略。
// 下限不变，所以 400 次上限对应的最短重排窗口（约 2s）不缩短；平均延迟从 5ms 变为 7.5ms。
// 延迟只决定消息何时回到准入队列，不占用 worker、不持锁。
func transientRequeueDelay() time.Duration {
	return entityGroupDispatchRequeueDelay + rand.N(entityGroupDispatchRequeueDelay)
}

func errorsIsEntityGroupPending(err error) bool {
	return errors.Is(err, ErrEntityGroupTransitionPending)
}

// observeEntityGroupTransition 记录迁移结果指标。
func observeEntityGroupTransition(req *GroupTransitionRequest, result string) {
	if req == nil {
		return
	}
	metrics.IncCounter("nest.entity_group.transition.total", metrics.Labels{
		"state":  entityGroupTransitionStateName(req.State),
		"result": result,
	}, 1)
}

func entityGroupTransitionStateName(state entity.EntityGroupTransitionState) string {
	switch state {
	case entity.EntityGroupTransitionJoin:
		return "join"
	case entity.EntityGroupTransitionLeave:
		return "leave"
	case entity.EntityGroupTransitionMove:
		return "move"
	default:
		return "unknown"
	}
}

// currentNestDispatchMsgs 按 goroutine 记录正在派发的消息（map[int64]*Msg），供 handler 内的框架入口取得
// 所属消息的提交事实与 Remote 批次。
var currentNestDispatchMsgs sync.Map // map[int64]*Msg

// pushCurrentNestDispatchMsg 把 msg 登记为当前 goroutine 的派发消息，返回恢复上一层登记的函数。
func pushCurrentNestDispatchMsg(msg *Msg) func() {
	if msg == nil {
		return func() {}
	}
	gid := goroutine.GoID()
	prev, _ := currentNestDispatchMsgs.Load(gid)
	if prev == msg {
		// 只省掉当前消息身份的重复登记；Guard、事务与 fctx 仍各自建立作用域。
		// 外层持有同一登记，内层退出（包括 panic）不能删除它。
		return func() {}
	}
	currentNestDispatchMsgs.Store(gid, msg)
	return func() {
		if prev != nil {
			currentNestDispatchMsgs.Store(gid, prev)
		} else {
			currentNestDispatchMsgs.Delete(gid)
		}
	}
}

// currentNestDispatchMsg 返回当前 goroutine 正在派发的消息；不在派发中时为 nil。
func currentNestDispatchMsg() *Msg {
	value, ok := currentNestDispatchMsgs.Load(goroutine.GoID())
	if !ok {
		return nil
	}
	msg, _ := value.(*Msg)
	return msg
}
