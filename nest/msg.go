package nest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

func (m *Msg) setRemoteWriteBatch(batch entity.RemoteWriteBatch) {
	if m != nil {
		m.RemoteWriteBatch = batch
	}
}

// CurrentRemoteWriteBatchContains reports whether the active Nest dispatch
// prepared a fenced remote write entry for the entity. Infrastructure hooks
// use it to reject a remote delete that was not declared in the message
// targets before locks were acquired.
func CurrentRemoteWriteBatchContains(entityID int64) bool {
	msg := currentNestDispatchMsg()
	if msg == nil || msg.RemoteWriteBatch == nil {
		return false
	}
	for _, id := range msg.RemoteWriteBatch.EntityIDs() {
		if id == entityID {
			return true
		}
	}
	return false
}

func (m *Msg) finalizeRemoteWriteBatch(tx *RollbackTx) error {
	if m == nil || m.RemoteWriteBatch == nil || m.remoteFinalized {
		return nil
	}
	if tx == nil {
		return entity.ErrRemoteCommitNotFinalized
	}
	outcome := entity.NewRemoteTransactionOutcome(
		entity.RemoteTransactionID(tx.ID()), tx.handler, tx.requestID(), true, uint8(tx.durability),
	)
	outcome.PersistChanges = tx
	outcome.DeleteIntents = tx
	if err := m.RemoteWriteBatch.FinalizeLocked(outcome); err != nil {
		return err
	}
	for _, commit := range m.RemoteWriteBatch.Commits() {
		commit := commit.Clone()
		if err := tx.AddMutation(EntityMutation{
			EntityID: commit.EntityID,
			Resource: "remote_entity",
			Version:  commit.NextVersion,
			Schema:   commit.Schema,
			Codec:    "remote",
			Remote:   &commit,
		}); err != nil {
			return err
		}
	}
	m.remoteFinalized = true
	return nil
}

func (m *Msg) finishRemoteWriteBatch(ctx context.Context, dispatchErr error) error {
	if m == nil || m.RemoteWriteBatch == nil {
		return nil
	}
	if m.localExecutor != nil {
		ctx = entity.WithLocalExecutor(ctx, m.localExecutor)
	}
	batch := m.RemoteWriteBatch
	m.RemoteWriteBatch = nil
	var err error
	committed := false
	// 是否已提交只看提交路径记录的事实，不从 dispatchErr 的类型推断：提交之后的 release
	// hook panic、回复前的其他错误都会以普通错误出现，旧实现据此把已持久的事务 Abort
	// （RR-20260926-32，与 RR-13/14 同根）。提交后的错误只随回复报告。
	switch {
	case m.remoteIndeterminate:
		// WAL may already contain the transaction. Close transfers ownership of
		// the held gates/leases to the manager's status-driven finalizer.
	case m.remoteCommitted:
		_, err = batch.Commit(ctx)
		committed = err == nil
		if err != nil {
			// 本地已持久提交，Remote 确认超时 / 结果未知 / 被明确拒绝：此刻没有持久结论，不能 Confirm；
			// 提交后工作在 Close 之前交给批次，由它拿到结论后执行一次（RR-20260926-37）。
			m.deferPostRemoteCommit(batch)
		}
	default:
		cause := dispatchErr
		if cause == nil {
			cause = entity.ErrRemoteCommitNotFinalized
		}
		err = batch.Abort(ctx, cause)
	}
	err = errors.Join(err, batch.Close(ctx))
	// 提交事实独立于释放/回调错误，不能遗失 Sync Confirm 或再次 Abort。
	// remoteConfirmed 让回复路径给此后的释放/回调错误加 ErrAfterCommitFailed（RR-20260926-46）。
	m.remoteConfirmed = committed
	if committed {
		if m.localExecutor != nil {
			var callbackErr error
			executeErr := m.localExecutor(func() { callbackErr = m.runPostRemoteCommit() })
			err = errors.Join(err, executeErr, callbackErr)
		} else {
			err = errors.Join(err, m.runPostRemoteCommit())
		}
	}
	return err
}

// deferPostRemoteCommit 把 Sync Confirm 与 AfterCommit 交给 Remote 批次的持久结论（RR-20260926-37）。
// 结论为已提交时回调按注册顺序在本地执行入口（快池）运行，每个回调独立隔离 panic；原请求已经或即将以
// “结果未知”的错误回复（二者可能并发），回调错误只记录日志与指标。回调看到的是原请求的上下文快照
// （RR-20260926-61，见 deferredRequestSnapshot），与结论及时到达时经快池续行执行看到的一致。Nest 已停机或已 fence
// 时 Remote 收尾不会调用闭包（不离池执行，计数告警），这些回调就不执行。结论为拒绝时只丢弃批次内 Remote 实体的
// Sync 门与事实（RejectEntities，RR-20260926-58）：本地部分已经持久提交，本地实体的门按 Confirm 放行；
// AfterCommit 不执行。批次不接手（没有后续持久结论）时什么也不做：没有持久结论就不 Confirm，门保持冻结。
// 闭包只捕获值，不引用会被回收复用的 Msg。
func (m *Msg) deferPostRemoteCommit(batch entity.RemoteWriteBatch) {
	handler := m.Name
	callbacks := m.postRemoteCommit
	mutation := m.remoteSyncMutation
	m.postRemoteCommit = nil
	m.remoteSyncMutation = nil
	deferrer, ok := batch.(entity.RemoteOutcomeDeferrer)
	if !ok {
		metrics.IncCounter("nest.remote.post_commit_without_outcome_total", metrics.Labels{"handler": handler}, 1)
		return
	}
	remoteIDs := batch.EntityIDs()
	snapshot := deferredRequestSnapshot()
	outcome := func(committed bool) {
		if !committed {
			mutation.RejectEntities(remoteIDs)
			return
		}
		if current := fctx.CurrentContext(); current != nil && snapshot.Valid {
			previous := current.Snapshot()
			current.ApplySnapshot(snapshot)
			defer current.ApplySnapshot(previous)
		}
		for _, callback := range callbacks {
			if err := runCommitCallback(callback); err != nil {
				metrics.IncCounter("nest.remote.deferred_after_commit_error_total", metrics.Labels{"handler": handler}, 1)
				slog.Error("nest: deferred remote after-commit callback failed", "handler", handler, "err", err)
			}
		}
	}
	if deferrer.DeferUntilDurableOutcome(outcome) {
		return
	}
	metrics.IncCounter("nest.remote.post_commit_without_outcome_total", metrics.Labels{"handler": handler}, 1)
}

// deferredRequestSnapshot 捕获慢阶段此刻的请求上下文（快阶段写回的请求数据已在其中），供延迟执行的提交后回调使用
// （RR-20260926-61）。两处与原样不同：去掉慢阶段注入的本地执行器（执行器属于正在结束的这次派发，不能随快照进入
// 快阶段）；Base 与请求 ctx 的取消脱钩——结论到达时回复早已发出、请求 ctx 通常已取消，取消等待不等于撤销业务，
// 已提交事务的回调不应因此失败。Base 上的值（请求元数据等）保留。
func deferredRequestSnapshot() fctx.ContextSnapshot {
	snapshot := fctx.CaptureSnapshot()
	if snapshot.Valid && snapshot.Base != nil {
		snapshot.Base = entity.WithLocalExecutor(context.WithoutCancel(snapshot.Base), nil)
	}
	return snapshot
}

func (m *Msg) abortRemoteWriteBatchLocked(cause error) error {
	if m == nil || m.RemoteWriteBatch == nil {
		return nil
	}
	return m.RemoteWriteBatch.Abort(context.Background(), cause)
}

func (m *Msg) markRemoteWriteIndeterminateLocked(cause error) error {
	if m == nil || m.RemoteWriteBatch == nil {
		return nil
	}
	if err := m.RemoteWriteBatch.Indeterminate(context.Background(), cause); err != nil {
		return err
	}
	m.remoteIndeterminate = true
	return nil
}

// markTransactionAdmitted 记录本条消息自己的事务已越过提交点；nil 接收者（嵌套事务、无派发调用）无操作。
func (m *Msg) markTransactionAdmitted() {
	if m != nil {
		m.txAdmitted = true
	}
}

// markTransactionCommitted 记录本条消息自己的事务已确定提交；nil 接收者无操作。
func (m *Msg) markTransactionCommitted() {
	if m != nil {
		m.txCommitted = true
	}
}

// markNestedTransactionCommitted 记录这条消息的 handler 内，一个不认领消息的嵌套独立事务已持久提交或结果未知
// （RR-20260926-65）；nil 接收者（不在派发中）无操作。
func (m *Msg) markNestedTransactionCommitted() {
	if m != nil {
		m.nestedTxCommitted = true
	}
}

// markCreateLockConflictWithoutRollback 记录这条消息里，不能回滚的 handler 内新建实体遇到了锁冲突
// （RR-20260926-64）；nil 接收者无操作。
func (m *Msg) markCreateLockConflictWithoutRollback() {
	if m != nil {
		m.createLockConflictNoRollback = true
	}
}

// requeueAllowed 判断失败的这条消息能否由框架自动重新准入：没有越过提交点（RR-20260926-49），
// 且不是“不能回滚的 handler 在冲突前已做了修改”（RR-20260926-64）。
func (m *Msg) requeueAllowed(err error) bool {
	if m == nil {
		return false
	}
	return !m.transactionPastCommitPoint(err) && !m.createLockConflictNoRollback
}

// replyAfterCommit 判断回复里的错误是否都发生在提交之后：纯本地事务看 txCommitted，带 Remote 批次的看 remoteConfirmed
// （本地已提交而 Remote 确认未知 / 被拒绝不是“已提交”，RR-20260926-46）。
func (m *Msg) replyAfterCommit() bool {
	if m == nil {
		return false
	}
	if m.remoteCommitted {
		return m.remoteConfirmed
	}
	return m.txCommitted
}

// transactionPastCommitPoint 判断这条消息是否可能已经提交了业务：自己的事务越过提交点、handler 内嵌套的独立事务
// 已持久提交或结果未知（RR-20260926-65）、Remote 本地已持久提交或结果未知，或回复已声明“提交之后失败”/“结果不确定”。
// 这样的消息不能重新准入，否则已提交内容会再执行一次（RR-20260926-49）。
func (m *Msg) transactionPastCommitPoint(err error) bool {
	if m == nil {
		return false
	}
	return m.txAdmitted || m.nestedTxCommitted || m.remoteCommitted || m.remoteIndeterminate ||
		errors.Is(err, ErrAfterCommitFailed) || errors.Is(err, ErrCommitIndeterminate)
}

func (m *Msg) addAfterUnlock(fn func()) {
	if m != nil && fn != nil {
		m.afterUnlock = append(m.afterUnlock, fn)
	}
}

func (m *Msg) runAfterUnlock() {
	if m == nil {
		return
	}
	callbacks := m.afterUnlock
	m.afterUnlock = nil
	for _, callback := range callbacks {
		if callback != nil {
			callback()
		}
	}
}

func (m *Msg) addPostRemoteCommit(callbacks ...func()) {
	if m != nil {
		m.postRemoteCommit = append(m.postRemoteCommit, callbacks...)
	}
}

func (m *Msg) runPostRemoteCommit() error {
	callbacks := m.postRemoteCommit
	m.postRemoteCommit = nil
	var failed error
	for _, callback := range callbacks {
		failed = errors.Join(failed, runCommitCallback(callback))
	}
	return failed
}

type MsgType uint8

const (
	MsgTypeSingle MsgType = iota
	MsgTypeMulti
	MsgTypeMultiGroup
	MsgTypeBroadcast
	MsgTypeGroupTransition
)

func (t MsgType) String() string {
	switch t {
	case MsgTypeSingle:
		return "Single"
	case MsgTypeMulti:
		return "Multi"
	case MsgTypeMultiGroup:
		return "MultiGroup"
	case MsgTypeBroadcast:
		return "Broadcast"
	case MsgTypeGroupTransition:
		return "GroupTransition"
	default:
		return "Unknown"
	}
}

// Msg is the internal message routed through the nest worker pool.
type Msg struct {
	RetChan             chan any
	RemoteWriteBatch    entity.RemoteWriteBatch
	Name                string
	Tids                []int64
	GroupTIds           [][]int64
	Params              []any
	Tid                 int64
	RefCount            int
	PendingRequeues     int
	Type                MsgType
	Cost                bool
	HasRemote           bool // message involves remote entities
	Context             fctx.ContextSnapshot
	GroupTransition     *GroupTransitionRequest
	remoteFinalized     bool
	remoteIndeterminate bool
	// remoteCommitted 在本地事务持久提交成功（commitDurable 的 durableCommit 返回 nil）
	// 时由执行 handler 的 goroutine 设置，慢阶段在续行返回后读取（done channel 建立
	// happens-before）。finishRemoteWriteBatch 只凭它决定 Commit / Abort（RR-20260926-32）。
	remoteCommitted bool
	// remoteConfirmed 表示 Remote 批次也已确认提交（finishRemoteWriteBatch 中 Commit 成功）。
	// 此后回复里的任何错误都是提交后的释放/回调失败，由 dispatchNest 统一包 ErrAfterCommitFailed；
	// 结果未知（确认超时）、拒绝与 Abort 都不置位，回复不得带该哨兵（RR-20260926-46）。
	remoteConfirmed bool
	// remoteSyncMutation 是本地已持久提交的 Remote 事务的 Sync 提交门。Remote 确认没有结论时，
	// deferPostRemoteCommit 把它连同 postRemoteCommit 交给批次：拒绝时 Reject（RR-20260926-37）。
	remoteSyncMutation *entity.SyncMutation
	// txInFlight 在本条消息自己的事务执行期间为真，用来区分 handler 内嵌套的独立事务。
	// txAdmitted 表示本条消息自己的事务已越过提交点：strict / memory 持久提交成功、pipelined 记录已被 WAL 接纳、
	// memory 快路径已准入。之后回复里的任何错误（即使链上有锁超时类错误）都不能让它重新准入（RR-20260926-49）。
	// 两者都由执行 handler 的 goroutine 写，dispatchNest 在 handler 返回（或慢阶段续行结束）后读。
	txInFlight bool
	txAdmitted bool
	// txCommitted 表示本条消息自己的事务已确定提交（strict / memory 持久提交成功、pipelined ticket 已持久、memory 快路径
	// 已准入）。纯本地事务此后的释放 / 回调错误由 dispatchNest 包 ErrAfterCommitFailed（RR-20260926-53）；带 Remote 批次的
	// 消息仍以 remoteConfirmed 为准。pipelined ticket 结果未知只置 txAdmitted，不置它。
	txCommitted bool
	// txNoRollback 表示本条消息自己的事务是 RollbackNone（memory 快路径，或带 Remote 批次的 memory handler）：
	// handler 失败时已做的内存修改不撤销。createLockConflictNoRollback 表示这样的消息里 handler 内新建实体遇到了
	// 按锁序不能等待的锁冲突；dispatchNest 据此不重排、回复带 ErrCreatedEntityLockConflict（RR-20260926-64）。
	// 两者都由执行 handler 的 goroutine 写，dispatchNest 在 handler 返回后读（与 txAdmitted 相同）。
	txNoRollback                 bool
	createLockConflictNoRollback bool
	// nestedTxCommitted 表示 handler 内嵌套的独立事务（不认领消息的 RunIsolatedTransaction 等）已持久提交或结果未知：
	// 消息按已越过提交点处理，不重排；自己的事务没有提交时回复带 ErrNestedTransactionCommitted（RR-20260926-65）。
	// 由执行 handler 的 goroutine 写，dispatchNest 在 handler 返回后读。
	nestedTxCommitted bool
	// deferredCompletion marks a pipelined transaction whose reply and
	// AfterCommit hooks were handed to the completion pump: the dispatch
	// path must not send RetChan itself. Reset by clean().
	deferredCompletion bool
	afterUnlock        []func()
	postRemoteCommit   []func()
	getter             entity.Getter
	prepared           *preparedGetter
	localExecutor      func(func()) error
	stageMetrics       bool
	queuedAt           time.Time
	remoteLogic        *remoteLogicCall

	// slowReroute 由快池首跑的 dispatchNest 设置：声明目标在 handler 取 Guard 之前变冷，
	// 派发队列应把同一个作业原位转到慢池准备，而不是回复或释放消息（RR-20260926-25）。
	slowReroute bool
}

func (m *Msg) Key() int64 {
	if m.Tid != 0 {
		return m.Tid
	} else if len(m.Tids) > 0 {
		return m.Tids[0]
	} else if len(m.GroupTIds) > 0 && len(m.GroupTIds[0]) > 0 {
		return m.GroupTIds[0][0]
	}
	return 0
}

func (m *Msg) TraceActive() bool {
	return m != nil && m.Context.Trace.Active()
}

func (m *Msg) clean() {
	*m = Msg{}
}

func (m *Msg) OnSend() {
	m.RefCount++
}

func (m *Msg) OnRelease() {
	m.RefCount--
	if m.RefCount == 0 {
		call := m.remoteLogic
		recycleMsg(m)
		if call != nil {
			close(call.done)
		}
	}
}

func (m *Msg) Clone() *Msg {
	ret := &Msg{
		Tid:             m.Tid,
		Type:            m.Type,
		Name:            m.Name,
		Tids:            slices.Clone(m.Tids),
		GroupTIds:       slices.Clone(m.GroupTIds),
		Params:          slices.Clone(m.Params),
		PendingRequeues: m.PendingRequeues,
		RetChan:         m.RetChan,
		Cost:            m.Cost,
		HasRemote:       m.HasRemote,
		Context:         m.Context.Clone(),
		GroupTransition: m.GroupTransition,
		getter:          m.getter,
	}
	return ret
}

func (m *Msg) String() string {
	buf := make([]byte, 0, 128)
	buf = append(buf, "Msg{Name:"...)
	buf = append(buf, m.Name...)
	buf = append(buf, ",Type:"...)
	buf = append(buf, m.Type.String()...)
	if m.Tid != 0 {
		buf = append(buf, ",Tid:"...)
		buf = strconv.AppendInt(buf, m.Tid, 10)
	}
	if len(m.Tids) > 0 {
		buf = append(buf, ",Tids:["...)
		for i, id := range m.Tids {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = strconv.AppendInt(buf, id, 10)
		}
		buf = append(buf, ']')
	}
	buf = append(buf, '}')
	return string(buf)
}

// TickMsg is dispatched each frame tick.
type TickMsg struct {
	Elapsed     int64 // nanoseconds
	FrameNumber uint64
}

var msgPool = sync.Pool{
	New: func() interface{} {
		return &Msg{}
	},
}

func GenMsg(msgType MsgType) *Msg {
	msg := msgPool.Get().(*Msg)
	msg.Type = msgType
	return msg
}

func GenSyncMsg(msgType MsgType) (*Msg, chan any) {
	msg := GenMsg(msgType)
	ch := make(chan any, 1)
	msg.RetChan = ch
	return msg, ch
}

func recycleMsg(m *Msg) {
	if m != nil {
		m.clean()
		msgPool.Put(m)
	}
}
