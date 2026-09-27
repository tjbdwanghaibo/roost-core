package nest

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"log/slog"
	"sync"
	"time"
)

const NestSyncTimeout = 5 * time.Second

var (
	ErrHandlerNotFound    = errors.New("nest: handler not found")
	ErrInvalidMessage     = errors.New("nest: invalid message")
	ErrGetterNotSet       = errors.New("nest: entity getter not set")
	ErrQueueFull          = errors.New("nest: dispatch queue full")
	ErrEntityNotFound     = errors.New("nest: entity not found")
	ErrEntityTypeMismatch = errors.New("nest: entity type mismatch")
	// ErrLockTimeout 是取锁超时 / 锁冲突类暂时性错误。只有回复不带任何“可能已提交”哨兵（ErrCommitIndeterminate、
	// entity.ErrRemotePersistenceIndeterminate、ErrAfterCommitFailed、ErrNestedTransactionCommitted），也不带
	// ErrNonRollbackNotRequeued 或不带锁超时形态的 ErrCreatedEntityLockConflict 时，它才表示“未提交（未开始或已回滚）、
	// 框架已自动重排到上限”，可以重试；链上有前述哨兵时以它们为准（RR-20260926-77，判别表见 docs/USER_GUIDE.md §4）。
	ErrLockTimeout                    = errors.New("nest: lock timeout")
	ErrNestTimeout                    = errors.New("nest: sync timeout")
	ErrNestCanceled                   = errors.New("nest: sync canceled")
	ErrNestStopped                    = errors.New("nest: stopped")
	ErrNestFenced                     = errors.New("nest: admission fenced")
	ErrDelayTooLong                   = errors.New("nest: delayed dispatch exceeds maximum delay")
	ErrParamMismatch                  = errors.New("nest: param mismatch")
	ErrAsyncInHandler                 = errors.New("nest: async dispatch from nest handler")
	ErrSyncInHandler                  = errors.New("nest: sync dispatch from nest handler")
	ErrEntityLockGroupMix             = errors.New("nest: mixed entity lock groups")
	ErrEntityLockGroupChanged         = errors.New("nest: entity lock group changed")
	ErrEntityGroupTransitionPending   = errors.New("nest: entity group transition pending")
	ErrEntityGroupTransitionScheduled = errors.New("nest: entity group transition scheduled")
	ErrInvalidEntityLockGroup         = errors.New("nest: invalid entity lock group")
	ErrRollbackUnsupported            = errors.New("nest: rollback is not supported by all participants")
	ErrRollbackFailed                 = errors.New("nest: rollback failed")
	// ErrAfterCommitFailed reports that a transaction is durable but one of its
	// after-commit callbacks panicked. The committed state stands; what failed
	// is work that ran after it, and the caller needs to know the difference
	// from a rollback (RR-20260911-06).
	//   - 是否可能已提交：已提交（消息自己的事务）。能否重试：不得重试。
	//   - 它只覆盖消息自己的事务：handler 内嵌套独立事务已提交而外层失败时回复是 ErrNestedTransactionCommitted，不带本哨兵，
	//     所以判断“是否可能已提交”不能只看它（RR-20260926-77）。
	ErrAfterCommitFailed = errors.New("nest: transaction committed but after-commit work failed")
	// ErrEntityReleaseFailed 表示准入后的解锁 hook 失败。它不表示事务被拒绝，
	// 调用方仍需检查同时返回的 ErrCommitIndeterminate，不能据此重试整笔业务。
	ErrEntityReleaseFailed           = errors.New("nest: entity release failed after admission")
	ErrTransactionClosed             = errors.New("nest: transaction is already closed")
	ErrCommitterRequired             = errors.New("nest: durable transaction committer is required")
	ErrDurableRemoteWriteUnsupported = errors.New("nest: durable remote write requires a lease-aware WAL committer")
	ErrRemoteBroadcastUnsupported    = errors.New("nest: remote-managed entities cannot use broadcast dispatch")
	ErrCommitRejected                = errors.New("nest: transaction commit rejected")
	// ErrPipelinedCommitterRequired means a handler declared
	// DurabilityPipelined but the configured committer does not implement
	// PipelinedTransactionCommitter. This is a deployment configuration error
	// and is reported instead of silently degrading to strict commits.
	ErrPipelinedCommitterRequired = errors.New("nest: pipelined durability requires a PipelinedTransactionCommitter")
	// ErrPipelinedNotAllowed means a handler declared DurabilityPipelined but
	// the engine's pipelined allowlist does not include it. Early lock
	// release changes commit semantics, so production rollout is gated per
	// handler; see NEST_PIPELINED_COMMIT.md.
	ErrPipelinedNotAllowed = errors.New("nest: handler is not on the pipelined durability allowlist")
	// ErrCommitIndeterminate means the storage device returned an error after
	// commit bytes may have reached durable media. The process must be fenced
	// and recovered from WAL; rolling the in-memory state back could create a
	// second, conflicting history.
	//   - 是否可能已提交：可能。能否重试：不得重试，等实例从 WAL 恢复后按业务幂等键核对。
	//   - 消息自己的事务与 handler 内嵌套独立事务结果未知时都已 fence 引擎（RR-20260926-76）。
	ErrCommitIndeterminate = errors.New("nest: transaction commit outcome is indeterminate")
	// ErrCreatedEntityLockConflict 表示 handler 内新建实体时，新实体的锁按锁序不能等待且已被其他持有者占用
	// （RR-20260926-48 / 64）。可回滚的事务同时带 ErrLockTimeout：整条回滚后由 Nest 自动重新准入。
	// 不能回滚的 handler（RollbackNone、memory 快路径）不带 ErrLockTimeout：冲突前的内存修改已生效且不撤销，
	// 消息不自动重排，这个错误原样（或补在业务错误上）回复调用方；是否重试由业务按 handler 的幂等性决定。
	//   - 是否可能已提交：两种形态本身都未提交。但“与 ErrLockTimeout 并存 = 已回滚、已重排到上限”只在不带
	//     ErrNestedTransactionCommitted 时成立：外层已有嵌套提交时同样两者并存，却是“已回滚、未重排、嵌套部分已提交”（RR-20260926-77）。
	//   - 能否重试：带 ErrLockTimeout 且不带任何“可能已提交”哨兵时可重试；不带 ErrLockTimeout 时框架未重排、修改未回滚，
	//     业务确认 handler 幂等后才可重试。
	ErrCreatedEntityLockConflict = errors.New("nest: created entity is locked by another holder")
	// ErrNestedTransactionCommitted 表示这条消息自己的事务没有提交（回滚或失败），但 handler 内嵌套的独立事务
	// （RunIsolatedTransaction 等）已经持久提交或结果未知（RR-20260926-65）。消息按已越过提交点处理，框架不自动重排；
	// 调用方不能把它当作“什么都没发生”重试整笔业务。原因错误仍可 errors.Is。
	//   - 是否可能已提交：是（嵌套部分已提交或结果未知）。能否重试：不得整笔重试，即使链上同时有 ErrLockTimeout（RR-20260926-77）。
	ErrNestedTransactionCommitted = errors.New("nest: a nested isolated transaction committed before the message failed")
	// ErrNonRollbackNotRequeued 表示不能回滚的 handler（memory 快路径，或消息自己的事务是 RollbackNone）已经开始执行后，
	// 以锁超时 / 组迁移类暂时性错误失败（RR-20260926-73）。可回滚的事务遇到这类错误会整条回滚后由 Nest 自动重新准入；
	// 这里 handler 失败前已做的内存修改不撤销，重排会让它们重复生效，所以框架不重排，原因错误（如 ErrLockTimeout）保留在链上。
	//   - 是否可能已提交：消息自己的事务没有提交（memory 快路径未准入、Remote 批次已 Abort），但失败前的内存修改仍在；
	//     消息已越过提交点（例如嵌套独立事务已提交）时不补本哨兵，由 ErrNestedTransactionCommitted 等说明。
	//   - 能否重试：框架不重试。业务确认 handler 幂等（或先读回实体当前状态）后才能自行重试，不能按“锁超时=未执行”盲目重发。
	// handler 开始执行之前的准入失败（声明目标取锁超时、组迁移待定等）不带它，照常由 Nest 重新准入。
	ErrNonRollbackNotRequeued = errors.New("nest: handler cannot roll back; transient failure after it started was not requeued")
	// ErrNestedTransactionRollbackConflict 表示 handler 内嵌套的独立事务（RunIsolatedTransaction）要持久写的实体，已被外层可回滚事务
	// （RollbackState / RollbackUndo）登记了回滚快照（RR-20260926-74）。外层随后失败回滚会把快照恢复到内存，覆盖嵌套事务已持久的
	// 结果，所以嵌套事务在写任何持久记录之前被拒绝并自身回滚；外层快照不变。外层是 memory handler（无回滚快照）时不受此限。
	//   - 是否可能已提交：否。嵌套事务没有写任何持久记录，它的内存修改已撤销；外层事务照常由业务决定继续或失败。
	//   - 能否重试：原样重试仍会被拒绝（结构性冲突，不是暂时性错误）。把这次写入并入外层事务，或让嵌套事务只写外层没有捕获的实体。
	ErrNestedTransactionRollbackConflict = errors.New("nest: nested isolated transaction writes an entity the enclosing transaction may roll back")
	// ErrNestedTransactionInRemoteMessage 表示在带 Remote 批次的消息里调用了 RunIsolatedTransaction（RR-20260926-75）。Remote 批次只随
	// 消息自己的事务 Commit 或 Abort；嵌套独立事务若经过它，会替外层 finalize 并把批次标成已提交，外层失败时 Remote 修改照样发布。
	// 框架直接拒绝：call 不执行、没有任何提交、批次与消息的 Remote 状态不变。
	//   - 是否可能已提交：否。独立事务没有执行；消息自己的事务照常由业务决定继续或失败，其 Remote 批次按它的结果 Commit / Abort。
	//   - 能否重试：原样重试仍会被拒绝（结构性限制）。把写入并入消息自己的事务，或放到不声明 Remote 目标的消息里执行。
	ErrNestedTransactionInRemoteMessage = errors.New("nest: nested isolated transaction is not supported in a message with a remote write batch")
)

func NewParamCountMismatchError(handler string, got int, want int) error {
	return fmt.Errorf("%w: handler=%s got=%d want=%d", ErrParamMismatch, handler, got, want)
}

func NewEntityCountMismatchError(handler string, got int, want int) error {
	return fmt.Errorf("%w: handler=%s entities=%d want=%d", ErrEntityTypeMismatch, handler, got, want)
}

func NewResultTypeMismatchError(handler string, want string, got any) error {
	return fmt.Errorf("%w: handler=%s result want=%s got=%T", ErrParamMismatch, handler, want, got)
}

func NewParamTypeMismatchError(handler string, idx int, want string, got any) error {
	return fmt.Errorf("%w: handler=%s param=%d want=%s got=%T", ErrParamMismatch, handler, idx, want, got)
}

type NestMgr struct {
	dispatcher             *Dispatcher
	ticker                 *Ticker
	getter                 entity.Getter
	loadedChecker          entity.LoadedChecker // getter 可选实现的只读“是否已加载”；nil 时准入不判冷（RR-20260926-47）
	remoteSnapshotResolver RemoteSnapshotResolver
	remoteManager          entity.IRemoteEntityManager
	committer              TransactionCommitter
	entitySync             entity.SyncCommitObserver
	syncTimeout            time.Duration
	// slowLockThreshold is the entity-lock hold time at which a dispatch is
	// counted and logged as slow. Zero disables the warning; the metric is
	// recorded regardless.
	stageMetrics      bool
	slowLockThreshold time.Duration
	lifecycleMu       sync.Mutex
	started           bool
	stopped           bool
	stopDone          chan struct{}
	stopErr           error
	fenceErr          error
	groupLocks        *entityLockGroupLockManager
	handlers          map[HandlerName]handlerEntry
	// pipelinedAllow, when non-nil, is the set of handler names permitted to
	// run with DurabilityPipelined. Nil permits all handlers (development
	// default); production deployments should pin an explicit allowlist so a
	// handler cannot adopt early lock release without an operations review.
	pipelinedAllow map[string]struct{}
	// completions, when non-nil, enables Phase 2 async completion: dispatch
	// workers hand the post-durability work (AfterCommit, reply) to the pump
	// instead of parking on the commit ticket. Nil keeps the Phase 1
	// in-worker wait.
	completions *completionPump
}

func (mgr *NestMgr) groupLockManager() *entityLockGroupLockManager {
	if mgr == nil {
		return nil
	}
	mgr.lifecycleMu.Lock()
	if mgr.groupLocks == nil {
		mgr.groupLocks = newEntityLockGroupLockManager()
	}
	locks := mgr.groupLocks
	mgr.lifecycleMu.Unlock()
	return locks
}

// Fence immediately rejects new and queued dispatches without attempting to
// roll back any transaction whose durable outcome is indeterminate. It is
// idempotent and is intentionally separate from Shutdown so in-flight handlers
// can finish their fail-stop paths before the application drains workers.
func (mgr *NestMgr) Fence(cause error) {
	if mgr == nil || cause == nil {
		return
	}
	mgr.lifecycleMu.Lock()
	if mgr.fenceErr == nil {
		mgr.fenceErr = errors.Join(ErrNestFenced, cause)
	}
	fenced := mgr.fenceErr
	mgr.lifecycleMu.Unlock()
	if mgr.dispatcher != nil {
		mgr.dispatcher.Fence(fenced)
	}
}

func (mgr *NestMgr) FenceError() error {
	if mgr == nil {
		return ErrNestStopped
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	return mgr.fenceErr
}

type HandlerName struct {
	value string
}

type Params []any

func NewHandlerName(value string) HandlerName {
	return HandlerName{value: value}
}

func NewParams(values ...any) Params {
	return Params(values)
}

func (n HandlerName) String() string {
	return n.value
}

func (mgr *NestMgr) TickDuration() time.Duration {
	if mgr == nil || mgr.ticker == nil {
		return 100 * time.Millisecond
	}
	return mgr.ticker.Duration()
}

type NestOpts struct {
	FastPool, SlowPool     WorkerPoolConfig
	Getter                 entity.Getter
	RemoteSnapshotResolver RemoteSnapshotResolver
	RemoteManager          entity.IRemoteEntityManager
	WorkerNum              int
	HbWorkerNum            int
	RemoteWorkers          int
	MsgCap                 int
	DelayedMsgCap          int
	MaxDelay               time.Duration
	TickDuration           time.Duration
	SyncTimeout            time.Duration
	Committer              TransactionCommitter
	EntitySync             entity.SyncCommitObserver
	PipelinedAllowlist     []string
	PipelinedAsync         bool
	PipelinedAsyncWorkers  int
	PipelinedAsyncQueueCap int
	SlowLockThreshold      time.Duration
	SlowLockThresholdSet   bool
	StageMetrics           bool
}

type NestOption func(*NestOpts)

var (
	// NestOptionWithWorkerPools 设置快慢两个执行池；非正数字段沿用默认或旧配置。
	NestOptionWithWorkerPools = func(fast, slow WorkerPoolConfig) NestOption {
		return func(opts *NestOpts) { opts.FastPool, opts.SlowPool = fast, slow }
	}
	// NestOptionWithRemoteWorkers 兼容旧配置，设置慢池并发。
	// Deprecated: 使用 NestOptionWithWorkerPools。
	NestOptionWithRemoteWorkers = func(workers int) NestOption {
		return func(opts *NestOpts) { opts.RemoteWorkers = workers }
	}
	// NestOptionWithStageMetrics 启用按 handler/stage 的耗时指标，默认关闭。
	// 旧 dispatch.cost 与 lock_hold 指标保持原口径。
	NestOptionWithStageMetrics = func(enabled bool) NestOption {
		return func(opts *NestOpts) { opts.StageMetrics = enabled }
	}
	NestOptionWithEntitySync = func(observer entity.SyncCommitObserver) NestOption {
		return func(opts *NestOpts) { opts.EntitySync = observer }
	}
	NestOptionWithGetter = func(getter entity.Getter) NestOption {
		return func(opts *NestOpts) {
			opts.Getter = getter
		}
	}
	NestOptionWithRemoteSnapshotResolver = func(resolver RemoteSnapshotResolver) NestOption {
		return func(opts *NestOpts) {
			opts.RemoteSnapshotResolver = resolver
		}
	}
	NestOptionWithRemoteEntityManager = func(manager entity.IRemoteEntityManager) NestOption {
		return func(opts *NestOpts) {
			opts.RemoteManager = manager
		}
	}
	NestOptionWithWorkerNumAndMsgCap = func(workerNum, hbWorkerNum, msgCap int) NestOption {
		return func(opts *NestOpts) {
			opts.WorkerNum = workerNum
			opts.HbWorkerNum = hbWorkerNum
			opts.MsgCap = msgCap
		}
	}
	NestOptionWithDelayedAdmission = func(capacity int, maxDelay time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.DelayedMsgCap = capacity
			opts.MaxDelay = maxDelay
		}
	}
	NestOptionWithTickDuration = func(tickDuration time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.TickDuration = tickDuration
		}
	}
	NestOptionWithSyncTimeout = func(timeout time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.SyncTimeout = timeout
		}
	}
	NestOptionWithTransactionCommitter = func(committer TransactionCommitter) NestOption {
		return func(opts *NestOpts) {
			opts.Committer = committer
		}
	}
	// NestOptionWithPipelinedAllowlist restricts DurabilityPipelined to the
	// named handlers; dispatching any other pipelined handler fails with
	// ErrPipelinedNotAllowed. Not calling it (or passing no names) permits
	// every handler — production deployments should always pin a list.
	// NestOptionWithSlowLockThreshold sets the warn threshold for the
	// per-handler entity-lock hold time. Every dispatch records its hold in
	// the nest.handler.lock_hold metric; holds at or beyond the threshold
	// additionally count nest.handler.lock_hold.slow.total and log a
	// warning. Zero disables the warning (the metric is always recorded);
	// the default is 100ms.
	NestOptionWithSlowLockThreshold = func(threshold time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.SlowLockThreshold = threshold
			opts.SlowLockThresholdSet = true
		}
	}
	NestOptionWithPipelinedAllowlist = func(names ...string) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAllowlist = append(opts.PipelinedAllowlist, names...)
		}
	}
	// NestOptionWithPipelinedAsyncCompletion enables Phase 2 of the pipelined
	// commit: the dispatch worker no longer parks on the commit ticket —
	// AfterCommit hooks and the reply run on a completion pool (hashed by
	// entity, so same-entity completions keep commit order) once the record
	// is durable. Hooks therefore run without the request context or entity
	// locks; see NEST_PIPELINED_COMMIT.md §10 before enabling. workers and
	// queueCap <= 0 select defaults (4, 8192); a full queue degrades single
	// transactions back to the Phase 1 in-worker wait.
	NestOptionWithPipelinedAsyncCompletion = func(workers, queueCap int) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAsync = true
			opts.PipelinedAsyncWorkers = workers
			opts.PipelinedAsyncQueueCap = queueCap
		}
	}
)

// NewEngine constructs an instance-scoped Nest engine. Callers inject its
// Client into generated senders. An engine is single-use and cannot be
// restarted after Shutdown.
func NewEngine(opts ...NestOption) *NestMgr {
	params := &NestOpts{}
	for _, opt := range opts {
		if opt != nil {
			opt(params)
		}
	}
	if !params.SlowLockThresholdSet {
		params.SlowLockThreshold = 100 * time.Millisecond
	}
	ret := &NestMgr{
		getter:                 params.Getter,
		remoteSnapshotResolver: params.RemoteSnapshotResolver,
		remoteManager:          params.RemoteManager,
		committer:              params.Committer,
		entitySync:             params.EntitySync,
		syncTimeout:            params.SyncTimeout,
		slowLockThreshold:      params.SlowLockThreshold,
		stageMetrics:           params.StageMetrics,
		stopDone:               make(chan struct{}),
		groupLocks:             newEntityLockGroupLockManager(),
		handlers:               snapshotHandlerEntries(),
	}
	if ret.entitySync != nil {
		ret.entitySync.BindSyncProducer()
	}
	if len(params.PipelinedAllowlist) > 0 {
		ret.pipelinedAllow = make(map[string]struct{}, len(params.PipelinedAllowlist))
		for _, name := range params.PipelinedAllowlist {
			ret.pipelinedAllow[name] = struct{}{}
		}
	}
	if params.PipelinedAsync {
		ret.completions = newCompletionPump(params.PipelinedAsyncWorkers, params.PipelinedAsyncQueueCap)
		ret.completions.fence = ret.Fence
	}
	if ret.syncTimeout <= 0 {
		ret.syncTimeout = NestSyncTimeout
	}
	ret.dispatcher = NewDispatcher("nest", params.WorkerNum, params.HbWorkerNum, params.MsgCap, func(msg *Msg) {
		if msg.remoteLogic != nil {
			msg.remoteLogic.run(ret)
			return
		}
		NestDispatch(ret, msg)
	})
	if params.FastPool.Workers > 0 {
		ret.dispatcher.workerNum = params.FastPool.Workers
	}
	if params.FastPool.QueueCap > 0 {
		ret.dispatcher.MsgCap = params.FastPool.QueueCap
		ret.dispatcher.DelayedMsgCap = params.FastPool.QueueCap
	}
	ret.dispatcher.slowConfig = params.SlowPool
	ret.dispatcher.remoteWorkers = params.RemoteWorkers
	ret.dispatcher.remoteHandler = func(msg *Msg) { dispatchNest(ret, msg, true) }
	if checker, ok := params.Getter.(entity.LoadedChecker); ok && checker != nil {
		ret.loadedChecker = checker
		ret.dispatcher.coldTargets = ret.declaredTargetsNeedSlowPreparation
	}
	ret.dispatcher.stageMetrics = params.StageMetrics
	ret.dispatcher.ConfigureDelayedAdmission(params.DelayedMsgCap, params.MaxDelay)
	ret.ticker = NewTicker(params.TickDuration)
	// committer 的可选能力：DataEngine 需要在快池持锁执行框架步骤（驱逐被跳过的原生步骤留下的
	// 实体，RR-20260926-30），这里把本引擎的 RunLocal 交给它。
	// Remote Manager 的后台收尾（finalizer）同样需要：持久拒绝后的回滚与仅内存卸载、确认无结论时
	// 延迟执行的提交后回调（RR-20260926-37/39）。两者共用这一个入口。
	runLocal := func(fn func()) error { return ret.RunLocal(context.Background(), fn) }
	if binder, ok := params.Committer.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	if binder, ok := params.RemoteManager.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	// Getter（entity.ManagerAccess）的共享冷加载与调用方解耦：领头的慢阶段请求截止离开后，加载仍在
	// 进行，发布实体不能再经那条已结束消息的快续行，改走这个入口（RR-20260926-54）。
	if binder, ok := params.Getter.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	return ret
}

// Start starts the worker pools and frame ticker. Repeated calls before
// Shutdown are harmless.
func (mgr *NestMgr) Start() error {
	if mgr == nil {
		return ErrNestStopped
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	if mgr.stopped {
		return ErrNestStopped
	}
	if mgr.started {
		return nil
	}
	if mgr.getter == nil {
		return ErrGetterNotSet
	}
	mgr.dispatcher.OnInit()
	mgr.dispatcher.OnRun()
	if mgr.completions != nil {
		mgr.completions.start()
	}
	mgr.ticker.Start()
	mgr.started = true
	return nil
}

// Shutdown stops admission, drains accepted work and stops the ticker. It is
// safe for concurrent callers; all callers observe the same result.
func (mgr *NestMgr) Shutdown(ctx context.Context) error {
	if mgr == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mgr.lifecycleMu.Lock()
	if mgr.stopped {
		done := mgr.stopDone
		mgr.lifecycleMu.Unlock()
		return mgr.waitStopped(ctx, done)
	}
	mgr.stopped = true
	started := mgr.started
	done := mgr.stopDone
	mgr.lifecycleMu.Unlock()

	if !started {
		mgr.finishShutdown(nil)
		return nil
	}
	// Once shutdown starts, accepted work must finish even if one caller's
	// deadline expires. This prevents a second engine from starting while old
	// workers still mutate entities. The caller's context bounds only its wait.
	go func() {
		mgr.ticker.Stop()
		err := mgr.dispatcher.OnDestroyWithContext(context.Background())
		// Deferred completions are accepted work: every reply and AfterCommit
		// promised to a caller must be delivered before shutdown finishes.
		// The dispatcher is drained, so no new submissions can arrive.
		if mgr.completions != nil {
			err = errors.Join(err, mgr.completions.stop(context.Background()))
		}
		mgr.finishShutdown(err)
	}()
	return mgr.waitStopped(ctx, done)
}

func (mgr *NestMgr) waitStopped(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		mgr.lifecycleMu.Lock()
		err := mgr.stopErr
		mgr.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (mgr *NestMgr) finishShutdown(err error) {
	mgr.lifecycleMu.Lock()
	mgr.stopErr = err
	close(mgr.stopDone)
	mgr.lifecycleMu.Unlock()
}

// Running reports whether Start completed and Shutdown has not begun.
func (mgr *NestMgr) Running() bool {
	if mgr == nil {
		return false
	}
	mgr.lifecycleMu.Lock()
	defer mgr.lifecycleMu.Unlock()
	return mgr.started && !mgr.stopped && mgr.fenceErr == nil
}

// DurableWatermark 返回装配的 committer 的持久化水位来源：committer 实现
// PipelinedTransactionCommitter 时为其 DurableLSN，否则为 nil（同步持久提交不需要外发水位）。
// 装配层用它给 Sync 的 DurableWatermark 接线，pipelined 事务在 WAL 持久化前不外发（RR-20260926-35）。
func (mgr *NestMgr) DurableWatermark() func() uint64 {
	if mgr == nil {
		return nil
	}
	if pipelined, ok := mgr.committer.(PipelinedTransactionCommitter); ok && pipelined != nil {
		return pipelined.DurableLSN
	}
	return nil
}

type sendOptParam struct {
	Delay time.Duration
	Cost  bool
}

type SendOpt func(*sendOptParam)

var (
	SendOptionWithDelay = func(delay time.Duration) SendOpt {
		return func(opt *sendOptParam) {
			opt.Delay = delay
		}
	}
	// SendOptionSlow 将声明目标的加载及前后置 I/O 放入慢池；handler 始终在快池。
	// Getter 实现 entity.LoadedChecker（ManagerAccess 已实现）时，声明目标中有未加载实体会在统一准入处
	// 自动走慢阶段（RR-20260926-25 / 47）；显式 Slow 仍有效，用于强制慢准备，也是未实现 LoadedChecker 的
	// 自定义 Getter 预加载冷目标的方式。
	SendOptionSlow = func() SendOpt {
		return func(opt *sendOptParam) { opt.Cost = true }
	}
	// Deprecated: 使用 SendOptionSlow；不再把业务 handler 放入独立 Cost 池。
	SendOptionIsCost = func() SendOpt {
		return func(opt *sendOptParam) {
			opt.Cost = true
		}
	}
)

// checkRemoteId checks whether a target ID is remote-capable and marks the
// message for remote preparation. The preparer later decides marked remote vs
// local fast path from the runtime marker store.
func checkRemoteId(msg *Msg, id int64) {
	if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
		msg.HasRemote = true
		msg.Cost = true
	}
}

// checkRemoteIds checks if any target ID in the slice is remote-capable and
// marks the message for remote preparation.
func checkRemoteIds(msg *Msg, ids []int64) {
	for _, id := range ids {
		if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
			msg.HasRemote = true
			msg.Cost = true
			return
		}
	}
}

// checkRemoteGroups checks if any target ID in grouped slices is
// remote-capable and marks the message for remote preparation.
func checkRemoteGroups(msg *Msg, groups [][]int64) {
	for _, g := range groups {
		for _, id := range g {
			if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
				msg.HasRemote = true
				msg.Cost = true
				return
			}
		}
	}
}

func shouldPrepareRemoteID(meta entity.EntityIDMeta) bool {
	if meta.Kind == entity.EntityKindNone || !meta.RemoteCapable {
		return false
	}
	if !entity.IsEntityKindRemoteCapable(meta.Kind) {
		return false
	}
	return entity.IsEntityKindRemoteManaged(meta.Kind)
}

func bindMsgContext(msg *Msg, carryBase bool) {
	if msg == nil {
		return
	}
	snapshot := fctx.CaptureSnapshot()
	if !carryBase {
		snapshot = asyncMessageContextSnapshot(snapshot)
	}
	msg.Context = snapshot
}

// asyncMessageContextSnapshot keeps the immutable framework envelope needed
// for tracing, request identity and config-generation consistency, while
// dropping execution-local state. In particular Base values, arbitrary KV
// (which may contain the active rollback transaction), frame and sync wait
// never cross the asynchronous boundary.
func asyncMessageContextSnapshot(snapshot fctx.ContextSnapshot) fctx.ContextSnapshot {
	if !snapshot.Valid {
		snapshot.Valid = true
		snapshot.Config = fctx.RuntimeConfig()
	}
	return fctx.ContextSnapshot{
		Valid:  true,
		Config: snapshot.Config,
		Meta:   snapshot.Meta,
		Trace:  snapshot.Trace.Clone(),
	}
}

func ensureAsyncDispatchAllowed(api string, name HandlerName) {
	if !fctx.InNestHandler() {
		return
	}
	c := fctx.CurrentContext()
	err := fmt.Errorf("%w: api=%s caller=%s target=%s", ErrAsyncInHandler, api, c.Meta.Handler, name.String())
	slog.Error("nest async dispatch from nest handler rejected",
		"err", err,
		"api", api,
		"caller", c.Meta.Handler,
		"target", name.String(),
		"player", c.Meta.PlayerID,
		"frame", c.Frame,
	)
	panic(err)
}
