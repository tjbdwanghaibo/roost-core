package nest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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
		entity.RemoteTransactionID(tx.ID()), tx.handler, tx.requestID(), true, uint8(tx.durability.Record()),
	)
	outcome.PersistChanges = tx
	outcome.DeleteIntents = tx
	if err := m.RemoteWriteBatch.FinalizeLocked(outcome); err != nil {
		return err
	}
	for _, commit := range m.RemoteWriteBatch.Commits() {
		commit := commit.Clone()
		kind := dataengine.MutationPut
		if commit.Delete {
			kind = dataengine.MutationDelete
		}
		if err := tx.AddMutation(EntityMutation{
			Key:             dataengine.DocumentKey{ID: commit.EntityID, Resource: "remote_entity"},
			Kind:            kind,
			ExpectedVersion: commit.BaseVersion,
			NextVersion:     commit.NextVersion,
			Schema:          commit.Schema,
			Codec:           "remote",
			Remote:          &commit,
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
			// 结果未知的 Commit 错误带 entity.ErrRemotePersistenceIndeterminate（entity 契约，判别表第 2 行）；不带它的就是
			// Remote 没有写入的明确拒绝。记下事实，由 dispatchNest 给回复加 ErrRemotePartRejected（RR-20260928-03）。
			// 这个判据要求 Remote 实现在“没等到结论”时也带未知哨兵：remoteentity 的 strict Commit 自 RR-20260928-08 起如此。
			m.remotePartRejected = !errors.Is(err, entity.ErrRemotePersistenceIndeterminate)
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
// 结论为已提交时回调按注册顺序在本地执行入口（业务池）运行，每个回调独立隔离 panic；原请求已经或即将以
// “结果未知”的错误回复（二者可能并发），回调错误只记录日志与指标。回调看到的是原请求的上下文快照
// （RR-20260926-61，见 deferredRequestSnapshot），与结论及时到达时经业务池续行执行看到的一致。Nest 已停机或已 fence
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

// deferredRequestSnapshot 捕获I/O 阶段此刻的请求上下文（业务阶段写回的请求数据已在其中），供延迟执行的提交后回调使用
// （RR-20260926-61）。两处与原样不同：去掉I/O 阶段注入的本地执行器（执行器属于正在结束的这次派发，不能随快照进入
// 业务阶段）；Base 与请求 ctx 的取消脱钩——结论到达时回复早已发出、请求 ctx 通常已取消，取消等待不等于撤销业务，
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

// markNestedTransactionCommitted 记录这条消息的 handler 内（或消息自己的事务结束后的收尾阶段，RR-20260926-84），一个不认领消息的
// 独立事务已持久提交或结果未知（RR-20260926-65）；nil 接收者（不在派发中）无操作。
func (m *Msg) markNestedTransactionCommitted() {
	if m != nil {
		m.nestedTxCommitted = true
	}
}

// fenceEngine 以 cause fence 执行这条消息的 Nest 引擎（NestMgr.Fence）；nil 接收者或不经 dispatchNest 的消息无操作。
func (m *Msg) fenceEngine(cause error) {
	if m != nil && m.engine != nil {
		m.engine.Fence(cause)
	}
}

// markCreateLockConflictWithoutRollback 记录这条消息里，不能回滚的 handler 内新建实体遇到了锁冲突
// （RR-20260926-64）；nil 接收者无操作。
func (m *Msg) markCreateLockConflictWithoutRollback() {
	if m != nil {
		m.createLockConflictNoRollback = true
	}
}

// markNoRollbackHandlerStarted 记录这条消息自己的事务不能回滚、且业务 handler 已开始执行（RR-20260926-73）；
// nil 接收者（嵌套事务、无派发调用）无操作。
func (m *Msg) markNoRollbackHandlerStarted() {
	if m != nil {
		m.noRollbackHandlerStarted = true
	}
}

// requeueAllowed 判断失败的这条消息能否由框架自动重新准入：没有越过提交点（RR-20260926-49），
// 且不是“不能回滚的 handler 已经开始执行”——冲突前 / 失败前已做的修改不撤销，重排会让它们重复生效
// （新建实体冲突 RR-20260926-64，任意锁超时 / 组迁移类错误 RR-20260926-73）。handler 开始之前的准入失败照常重排。
func (m *Msg) requeueAllowed(err error) bool {
	if m == nil {
		return false
	}
	return !m.transactionPastCommitPoint(err) && !m.createLockConflictNoRollback && !m.noRollbackHandlerStarted
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

// Msg 承载请求数据和一次派发的运行状态。池归还时 clean 整体清零；Clone 只复制可重排的请求数据。
// 原消息跨 I/O 与业务池串行交接：I/O 等续行 done 后才重新访问，不依靠字段原子化并发共享。
type Msg struct {
	// 请求与目标。单目标直接存 Tid；多目标保留调用顺序、重复项和组边界。
	// 调度器另建排序去重的 ID 集合，不能原地改写这里的业务参数。
	Name            string
	Tids            []int64
	GroupTIds       [][]int64
	Params          []any
	Context         fctx.ContextSnapshot
	RetChan         chan any
	GroupTransition *GroupTransitionRequest
	Tid             int64
	RefCount        int
	PendingRequeues int
	Type            MsgType
	Cost            bool
	HasRemote       bool

	// Remote 批次与发布责任。FinalizeLocked 在业务锁内生成提交内容；
	// I/O 阶段确认/关闭批次，未决结果连同 Sync 门和回调移交持久结论处理者。
	RemoteWriteBatch    entity.RemoteWriteBatch
	remoteSyncMutation  *entity.SyncMutation
	postRemoteCommit    []func()
	remoteFinalized     bool // 已完成批次内容归集，不能重复 Finalize。
	remoteIndeterminate bool // 本地持久结果未知，收尾不能推断为 Abort。
	remoteCommitted     bool // 本地持久提交成功，Remote 收尾须 Commit，不能因释放错误 Abort。
	remoteConfirmed     bool // Remote Commit 也成功；其后的错误归为提交后失败。
	remotePartRejected  bool // 本地已提交，Remote 明确拒绝；与 confirmed 互斥，未知不置位。

	// 消息自身事务。handler goroutine 写，派发收尾在 handler/续行结束后读取。
	txInFlight                   bool // 自身事务正在执行；识别嵌套事务还必须检查入口的 claimsMessage。
	txAdmitted                   bool // 已越过准入点，不能重排；pipelined 尚可能没有持久结论。
	txCommitted                  bool // 自身事务已确定提交；Remote 消息的完整成功仍以 remoteConfirmed 为准。
	txNoRollback                 bool // 自身事务不回滚；与“handler 已经执行”是两件事。
	noRollbackHandlerStarted     bool // 不可回滚 handler 已开始，暂时性错误也不能整段重排。
	createLockConflictNoRollback bool // 保留创建锁冲突事实，即使业务替换错误，仍能正确分类回复。
	nestedTxCommitted            bool // 独立嵌套事务已提交或结果未知；外层失败也不能重排整段。
	deferredCompletion           bool // 回复已交完成泵；派发路径不得再次发送 RetChan。

	// 执行资源与引用。getter 可临时包装为仅加载态访问；prepared 独立负责 Touch 引用归还。
	getter        entity.Getter
	prepared      *preparedGetter
	engine        *NestMgr           // Await 调度，以及独立事务结果未知时 fence 引擎。
	localExecutor func(func()) error // I/O 收尾借用业务 worker 运行需要实体锁的步骤。
	remoteLogic   *remoteLogicCall   // 内部续行信封；不是重新准入的业务请求。
	afterUnlock   []func()           // Guard 释放后执行，不能与 Remote 确认后的回调合并。

	// Await 只持有当前业务段的数据；恢复段正常入队，不复用原段的 ID tail。
	awaitPlan       *awaitPlan
	resumeEntry     *handlerEntry
	handlerEntities []entity.IThreadSafeEntity
	ioWork          func()
	afterQueue      func() // 原段释放 tail 后启动 I/O，不进入上下文快照。

	// 观测与调度标志。转 I/O 保留作业身份和 tail，不重复准入或释放消息。
	queuedAt     time.Time
	stageMetrics bool
	ioReroute    bool // 声明目标在取 Guard 前变冷，需要原位转 I/O 准备。
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
