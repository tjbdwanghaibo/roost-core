package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// NestSyncTimeout 是同步请求等待回复的默认上限，NestOptionWithSyncTimeout 可覆盖。
const NestSyncTimeout = 5 * time.Second

var (
	ErrHandlerNotFound    = errors.New("nest: handler not found")
	ErrInvalidMessage     = errors.New("nest: invalid message")
	ErrGetterNotSet       = errors.New("nest: entity getter not set")
	ErrQueueFull          = errors.New("nest: dispatch queue full")
	ErrEntityNotFound     = errors.New("nest: entity not found")
	ErrEntityTypeMismatch = errors.New("nest: entity type mismatch")
	// ErrLockTimeout 是取锁超时 / 锁冲突类暂时性错误。只有回复不带任何“可能已提交”哨兵（ErrCommitIndeterminate、
	// entity.ErrRemotePersistenceIndeterminate、entity.ErrRemoteCommitTimeout、ErrAfterCommitFailed、ErrRemotePartRejected、
	// ErrNestedTransactionCommitted），也不带 ErrNonRollbackNotRequeued 或不带锁超时形态的 ErrCreatedEntityLockConflict 时，
	// 它才表示“未提交（未开始或已回滚）、框架已自动重排到上限”，可以重试；链上有前述哨兵时以它们为准。
	// strict 等 Remote 确认到截止的回复同时带 ErrRemoteCommitTimeout 与 ErrRemotePersistenceIndeterminate；
	// 自定义 Remote 实现可能只返回前者，所以两者都列。
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
	// ErrAfterCommitFailed 表示事务已持久，但某个提交后回调 panic。已提交的状态保持有效，失败的是提交之后的工作，
	// 调用方需要把它与回滚区分开。
	//   - 是否可能已提交：已提交（消息自己的事务）。能否重试：不得重试。
	//   - 它只覆盖消息自己的事务：handler 内嵌套独立事务（或消息自己的事务结束后、收尾阶段调用的独立事务）
	//     已提交而外层失败时回复是 ErrNestedTransactionCommitted，不带本哨兵，所以判断“是否可能已提交”不能只看它。
	ErrAfterCommitFailed = errors.New("nest: transaction committed but after-commit work failed")
	// ErrRemotePartRejected 表示带 Remote 批次的消息自己的本地事务已提交（按 handler 的 durability：strict / async 已持久，
	// memory 为内存提交），而 Remote 部分被明确拒绝。只丢弃被拒绝 Remote 实体的修改（批次回滚、隔离并从权威重载），
	// 同一事务里的本地部分照常生效；提交后回调（AfterCommit）不执行。
	// 原因错误（如 entity.ErrRemoteRejected、entity.ErrRemoteVersionConflict）保留在链上。按 entity 契约，Remote 结果未知时 Commit
	// 返回 entity.ErrRemotePersistenceIndeterminate，不带它的 Commit 失败就是 Remote 没有写入（框架自带的 remoteentity
	// 只有 memory 的明确拒绝不带它；strict / pipelined 等 WAL 投影器结论，WAL 级 Remote 不产生明确拒绝，Commit 失败一律带它）；
	// 两者互斥，与 ErrAfterCommitFailed（Remote 也已确认）同样互斥。
	//   - 是否可能已提交：部分已提交（本地部分已提交，Remote 部分未提交）。能否重试：不得整笔重试（本地部分会重复执行）；
	//     按业务只补做 Remote 部分，或读回状态后再决定。
	ErrRemotePartRejected = errors.New("nest: local transaction committed but its remote part was rejected")
	// ErrEntityReleaseFailed 表示准入后的解锁 hook 失败。它不表示事务被拒绝，
	// 调用方仍需检查同时返回的 ErrCommitIndeterminate，不能据此重试整笔业务。
	ErrEntityReleaseFailed           = errors.New("nest: entity release failed after admission")
	ErrTransactionClosed             = errors.New("nest: transaction is already closed")
	ErrCommitterRequired             = errors.New("nest: durable transaction committer is required")
	ErrDurableRemoteWriteUnsupported = errors.New("nest: durable remote write requires a lease-aware WAL committer")
	ErrRemoteBroadcastUnsupported    = errors.New("nest: remote-managed entities cannot use broadcast dispatch")
	// ErrCommitRejected 表示事务在写任何持久记录之前被明确拒绝：committer / Enqueue 拒绝，Remote 批次 FinalizeLocked
	// 拒绝、准备提交记录失败、嵌套事务写外层快照被拒（ErrNestedTransactionRollbackConflict）、fence 之后不交给 committer
	// （ErrNestFenced）。原因错误仍可 errors.Is。
	//   - 是否可能已提交：否。可回滚事务已回滚；不能回滚的事务（带 Remote 批次的 memory handler）内存修改不撤销。
	ErrCommitRejected = errors.New("nest: transaction commit rejected")
	// ErrMemoryTransactionPersistentWrite 表示 durability=memory 的事务（rollback=state|undo，或带 Remote 批次的 rollback=none）
	// 改了本地持久字段（或用 AddMutation 加了本地 mutation）：memory 事务不写任何持久记录，这些改动无处可去。
	// 提交点整笔拒绝，回复同时带 ErrCommitRejected，错误点名实体（collection/id）与字段；可回滚的事务已按回滚策略撤销内存修改，
	// rollback=none 的（带 Remote 批次）不撤销内存修改、Remote 批次 Abort。
	//   - 是否可能已提交：否。能否重试：不能原样重试——这是 handler 声明与业务写法的矛盾：把 durability 改成 strict / async，
	//     或把字段声明为 nopersist。
	ErrMemoryTransactionPersistentWrite = errors.New("nest: memory transaction changed persistent fields")
	// ErrPipelinedCommitterRequired 表示 handler 声明了 DurabilityPipelined，但装配的 committer 没有实现
	// PipelinedTransactionCommitter。这是部署配置错误，直接报告，不静默降级为 strict 提交。
	ErrPipelinedCommitterRequired = errors.New("nest: pipelined durability requires a PipelinedTransactionCommitter")
	// ErrPipelinedNotAllowed 表示 handler 声明了 DurabilityPipelined，但不在引擎的 pipelined 白名单内。提前放锁会改变提交
	// 语义，所以生产环境按 handler 逐个放开；见 NEST_PIPELINED_COMMIT.md。
	ErrPipelinedNotAllowed = errors.New("nest: handler is not on the pipelined durability allowlist")
	// ErrDurabilityUnset 是注册期错误：手写 HandlerMeta 的 Rollback 不为 none，却没有显式写 Durability。
	// Durability 零值不隐含 memory，要在 memory / async / strict / pipelined 里选一个；
	// //roost:nest 生成的 meta 总是写明 durability，不受影响。
	//   - 是否可能已提交：否（注册失败，handler 不可用）。能否重试：不能原样重试，补上 Durability 后重新注册。
	ErrDurabilityUnset = errors.New("nest: handler meta durability not declared")
	// ErrCommitIndeterminate 表示存储设备在提交字节可能已到达持久介质之后报错。进程必须 fence 并从 WAL 恢复；
	// 回滚内存状态可能制造第二条相互冲突的历史。
	//   - 是否可能已提交：可能。能否重试：不得重试，等实例从 WAL 恢复后按业务幂等键核对。
	//   - 消息自己的事务、handler 内嵌套独立事务，以及消息自己的事务结束后收尾阶段（Guard post-release、解锁后回调）
	//     调用的独立事务，结果未知时都已 fence 引擎。
	ErrCommitIndeterminate = errors.New("nest: transaction commit outcome is indeterminate")
	// ErrCreatedEntityLockConflict 表示 handler 内新建实体时，新实体的锁按锁序不能等待且已被其他持有者占用，
	// 或同 ID 的上一个实例仍在撤销 / 销毁收尾。可回滚的事务同时带 ErrLockTimeout：整条回滚后由 Nest 自动重新准入。
	// 不能回滚的 handler（RollbackNone、memory 快路径）不带 ErrLockTimeout：冲突前的内存修改已生效且不撤销，
	// 消息不自动重排，这个错误原样（或补在业务错误上）回复调用方；是否重试由业务按 handler 的幂等性决定。
	//   - 是否可能已提交：两种形态本身都未提交。但“与 ErrLockTimeout 并存 = 已回滚、已重排到上限”只在不带
	//     ErrNestedTransactionCommitted 时成立：外层已有嵌套提交时同样两者并存，却是“已回滚、未重排、嵌套部分已提交”。
	//   - 能否重试：带 ErrLockTimeout 且不带任何“可能已提交”哨兵时可重试；不带 ErrLockTimeout 时框架未重排、修改未回滚，
	//     业务确认 handler 幂等后才可重试。
	ErrCreatedEntityLockConflict = errors.New("nest: created entity is locked by another holder")
	// ErrNestedTransactionCommitted 表示这条消息自己的事务没有提交（回滚或失败），但 handler 内嵌套的独立事务
	// （RunIsolatedTransaction 等）已经持久提交或结果未知；消息自己的事务结束后、收尾阶段（Guard post-release、
	// 解锁后回调）调用的独立事务也不认领消息，同样由本哨兵说明，不会被当成消息自己的提交。消息按已越过提交点处理，
	// 框架不自动重排；调用方不能把它当作“什么都没发生”重试整笔业务。原因错误仍可 errors.Is。
	//   - 是否可能已提交：是（嵌套部分已提交或结果未知）。能否重试：不得整笔重试，即使链上同时有 ErrLockTimeout。
	//   - 带 Remote 批次的消息自己的本地事务已提交、Remote 结果未知或被明确拒绝（链上同时有 entity.ErrRemotePersistenceIndeterminate
	//     或 ErrRemotePartRejected，以它们为先）时也可能带本哨兵：PrepareRemoteWriteBatch 期间调用的独立事务已提交。
	//     这时回复外层文本是“nest: a nested isolated transaction also committed”，不说“消息失败”；
	//     errors.Is(err, ErrNestedTransactionCommitted) 照常成立。
	ErrNestedTransactionCommitted = errors.New("nest: a nested isolated transaction committed before the message failed")
	// ErrNonRollbackNotRequeued 表示不能回滚的 handler（memory 快路径，或消息自己的事务是 RollbackNone）已经开始执行后，
	// 以锁超时 / 组迁移类暂时性错误失败。可回滚的事务遇到这类错误会整条回滚后由 Nest 自动重新准入；
	// 这里 handler 失败前已做的内存修改不撤销，重排会让它们重复生效，所以框架不重排，原因错误（如 ErrLockTimeout）保留在链上。
	//   - 是否可能已提交：消息自己的事务没有提交（memory 快路径未准入、Remote 批次已 Abort），但失败前的内存修改仍在；
	//     消息已越过提交点（例如嵌套独立事务已提交）时不补本哨兵，由 ErrNestedTransactionCommitted 等说明。
	//   - 能否重试：框架不重试。业务确认 handler 幂等（或先读回实体当前状态）后才能自行重试，不能按“锁超时=未执行”盲目重发。
	// handler 开始执行之前的准入失败（声明目标取锁超时、组迁移待定等）不带它，照常由 Nest 重新准入。
	ErrNonRollbackNotRequeued = errors.New("nest: handler cannot roll back; transient failure after it started was not requeued")
	// ErrNestedTransactionRollbackConflict 表示 handler 内嵌套的独立事务（RunIsolatedTransaction）要持久写的实体，已被外层可回滚事务
	// （RollbackState / RollbackUndo）登记了回滚快照。外层随后失败回滚会把快照恢复到内存，覆盖嵌套事务已持久的
	// 结果，所以嵌套事务在写任何持久记录之前被拒绝并自身回滚；外层快照不变。外层是 memory handler（无回滚快照）时不受此限。
	// 不经 DAO、用 AddMutation 直接加入的原始 mutation 按实体 ID 同样判断。
	//   - 是否可能已提交：否。嵌套事务没有写任何持久记录，它的内存修改已撤销；外层事务照常由业务决定继续或失败。
	//   - 能否重试：原样重试仍会被拒绝（结构性冲突，不是暂时性错误）。把这次写入并入外层事务，或让嵌套事务只写外层没有捕获的实体。
	ErrNestedTransactionRollbackConflict = errors.New("nest: nested isolated transaction writes an entity the enclosing transaction may roll back")
	// ErrNestedTransactionInRemoteMessage 表示在带 Remote 批次的消息里调用了 RunIsolatedTransaction：handler 内，
	// 消息自己的事务开始之前的 I/O 阶段准备里（批次挂上之后），或消息自己的事务结束之后、批次收尾之前的收尾阶段
	// （Guard post-release、解锁后回调）。Remote 批次只随消息自己的事务 Commit 或 Abort；独立事务若经过它，会替外层
	// finalize 并把批次标成已提交，外层失败时 Remote 修改照样发布。框架直接拒绝：call 不执行、没有任何提交、批次与消息的
	// Remote 状态不变。
	// “带 Remote 批次”从批次挂到消息上开始算：IRemoteEntityManager.PrepareRemoteWriteBatch 执行期间批次还没挂上，这时调用
	// RunIsolatedTransaction 不返回本错误，按不认领消息的嵌套独立事务处理：照常执行并提交，不碰随后才挂上的批次；消息自己的
	// 事务随后没提交时回复带 ErrNestedTransactionCommitted，结果未知时 fence，与纯本地消息的收尾阶段相同。框架自带的
	// remoteentity.Manager 在这一步只做所有权准入与冷加载，本身不调用 RunIsolatedTransaction。
	//   - 是否可能已提交：否。独立事务没有执行；消息自己的事务照常由业务决定继续或失败，其 Remote 批次按它的结果 Commit / Abort。
	//   - 能否重试：原样重试仍会被拒绝（结构性限制）。把写入并入消息自己的事务，或放到不声明 Remote 目标的消息里执行。
	ErrNestedTransactionInRemoteMessage = errors.New("nest: nested isolated transaction is not supported in a message with a remote write batch")
)

// errNestedTransactionAlsoCommitted 是 ErrNestedTransactionCommitted 在“消息自己的本地事务也已提交”时的外层表述：
// 带 Remote 批次的消息本地已提交、Remote 结果未知或被明确拒绝，同时 PrepareRemoteWriteBatch 期间开的独立事务已提交。
// 外层若沿用“…committed before the message failed”会与内层“local transaction committed …”矛盾，所以只换文本，
// errors.Is(err, ErrNestedTransactionCommitted) 照常成立。
var errNestedTransactionAlsoCommitted error = nestedTransactionAlsoCommitted{}

type nestedTransactionAlsoCommitted struct{}

func (nestedTransactionAlsoCommitted) Error() string {
	return "nest: a nested isolated transaction also committed"
}

func (nestedTransactionAlsoCommitted) Is(target error) bool {
	return target == ErrNestedTransactionCommitted
}

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

// NestMgr 是一个实例级 Nest 引擎：持有派发器（short / long / io 三池）、帧 ticker、实体 Getter、
// 事务 committer 与 Remote 协作入口。引擎一次性使用，Shutdown 后不能再启动。
type NestMgr struct {
	dispatcher             *Dispatcher
	ticker                 *Ticker
	getter                 entity.Getter
	loadedChecker          entity.LoadedChecker // getter 可选实现的只读“是否已加载”；nil 时准入不判冷
	remoteSnapshotResolver RemoteSnapshotResolver
	remoteManager          entity.IRemoteEntityManager
	committer              TransactionCommitter
	entitySync             entity.SyncCommitObserver
	syncTimeout            time.Duration
	stageMetrics           bool
	// slowLockThreshold 是实体锁持有时长的慢告警阈值：达到即计数并打日志。0 关闭告警，指标照常记录。
	slowLockThreshold time.Duration
	lifecycleMu       sync.Mutex
	started           bool
	stopped           bool
	stopDone          chan struct{}
	stopErr           error
	fenceErr          error
	groupLocks        *entityLockGroupLockManager
	handlers          map[HandlerName]handlerEntry
	// pipelinedAllow 非 nil 时是允许使用 DurabilityPipelined 的 handler 名单；nil 允许全部（开发默认）。
	// 生产部署应固定名单，避免 handler 未经评审就采用提前放锁。
	pipelinedAllow map[string]struct{}
	// completions 非 nil 时启用 pipelined 第二阶段异步完成：派发 worker 把持久之后的工作（AfterCommit、
	// 回复）交给完成泵，不在提交票据上等待。nil 时保持第一阶段在 worker 内等待。
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

// Fence 立即拒绝新的与已排队的派发，不尝试回滚任何持久结果不确定的事务。它是幂等的，并且有意与
// Shutdown 分开：在途 handler 先走完自己的 fail-stop 路径，应用再排空 worker。首个 cause 胜出。
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

// FenceError 返回 Fence 记录的错误（包裹 ErrNestFenced）；未 fence 时为 nil。
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

// CurrentTick 返回引擎 Ticker 的当前帧号；没有 Ticker 时为 0。
func (mgr *NestMgr) CurrentTick() uint64 {
	if mgr == nil {
		return 0
	}
	return mgr.ticker.CurrentTick()
}

func (mgr *NestMgr) TickDuration() time.Duration {
	if mgr == nil || mgr.ticker == nil {
		return 100 * time.Millisecond
	}
	return mgr.ticker.Duration()
}

type NestOpts struct {
	ShortPool, IOPool, LongPool WorkerPoolConfig
	Getter                      entity.Getter
	RemoteSnapshotResolver      RemoteSnapshotResolver
	RemoteManager               entity.IRemoteEntityManager

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
	// NestOptionWithSlowLockThreshold 设置每个 handler 实体锁持有时长的告警阈值。每次派发都把持有时长记入
	// nest.handler.lock_hold；达到阈值的另计 nest.handler.lock_hold.slow.total 并打告警日志。0 关闭告警
	// （指标照常记录），默认 100ms。
	NestOptionWithSlowLockThreshold = func(threshold time.Duration) NestOption {
		return func(opts *NestOpts) {
			opts.SlowLockThreshold = threshold
			opts.SlowLockThresholdSet = true
		}
	}
	// NestOptionWithPipelinedAllowlist 把 DurabilityPipelined 限制在列出的 handler；其他 pipelined handler 的派发
	// 返回 ErrPipelinedNotAllowed。不调用（或不传名字）时允许全部 handler——生产部署应始终固定名单。
	NestOptionWithPipelinedAllowlist = func(names ...string) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAllowlist = append(opts.PipelinedAllowlist, names...)
		}
	}
	// NestOptionWithPipelinedAsyncCompletion 启用 pipelined 提交第二阶段：派发 worker 不再在提交票据上等待，
	// 记录持久后 AfterCommit 与回复在完成池执行（按实体哈希，同实体的完成保持提交顺序）。因此这些回调不带请求
	// 上下文、也不持实体锁；启用前先读 NEST_PIPELINED_COMMIT.md §10。workers 与 queueCap <= 0 取默认值
	// （4、8192）；队列满时单笔事务降级为第一阶段的 worker 内等待。
	NestOptionWithPipelinedAsyncCompletion = func(workers, queueCap int) NestOption {
		return func(opts *NestOpts) {
			opts.PipelinedAsync = true
			opts.PipelinedAsyncWorkers = workers
			opts.PipelinedAsyncQueueCap = queueCap
		}
	}
)

// NewEngine 构造一个实例级 Nest 引擎，调用方把它的 Client 注入生成的 sender。引擎一次性使用，
// Shutdown 后不能重启。
func NewEngine(opts ...NestOption) *NestMgr {
	entity.FreezeBusinessPools()
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
	ret.dispatcher = NewDispatcher("nest", params.ShortPool.Workers, params.ShortPool.QueueCap, func(msg *Msg) {
		if msg.remoteLogic != nil {
			msg.remoteLogic.run(ret)
			return
		}
		NestDispatch(ret, msg)
	})
	ret.dispatcher.ioConfig = params.IOPool
	ret.dispatcher.longConfig = params.LongPool
	ret.dispatcher.remoteHandler = func(msg *Msg) { dispatchNest(ret, msg, true) }
	if checker, ok := params.Getter.(entity.LoadedChecker); ok && checker != nil {
		ret.loadedChecker = checker
		ret.dispatcher.coldTargets = ret.declaredTargetsNeedIOPreparation
	}
	ret.dispatcher.stageMetrics = params.StageMetrics
	ret.dispatcher.ConfigureDelayedAdmission(params.DelayedMsgCap, params.MaxDelay)
	ret.ticker = NewTicker(params.TickDuration)
	// 把本引擎的 RunLocal 交给需要在业务池持锁执行框架步骤的组件，三者共用这一个入口：
	//   - committer（DataEngine）：驱逐被跳过的原生步骤留下的实体；
	//   - Remote Manager 的后台收尾（finalizer）：持久拒绝后的回滚与仅内存卸载、确认无结论时延迟执行的提交后回调；
	//   - Getter（entity.ManagerAccess）的共享冷加载：领头的 I/O 阶段请求截止离开后加载仍在进行，发布实体
	//     不能再经那条已结束消息的业务续行。
	runLocal := func(fn func()) error { return ret.RunLocal(context.Background(), fn) }
	if binder, ok := params.Committer.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	if binder, ok := params.RemoteManager.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	if binder, ok := params.Getter.(LocalExecutorBinder); ok {
		binder.BindLocalExecutor(runLocal)
	}
	return ret
}

// Start 启动 worker 池与帧 ticker。Shutdown 之前重复调用无副作用。
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

// Shutdown 停止准入、排空已接受的工作并停止 ticker。可并发调用，所有调用方看到同一结果；
// 每个调用方只在自己的 ctx 内等待。
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
	// 关闭一旦开始，已接受的工作必须完成，即使某个调用方的截止时间先到：否则新引擎可能在旧 worker
	// 仍修改实体时启动。调用方的 ctx 只约束它自己的等待。
	go func() {
		mgr.ticker.Stop()
		err := mgr.dispatcher.OnDestroyWithContext(context.Background())
		// 延迟完成也是已接受的工作：承诺给调用方的回复与 AfterCommit 都要在关闭结束前送达。
		// 派发器已排空，不会再有新的提交。
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

// Running 报告 Start 已完成、Shutdown 未开始且引擎未被 fence。
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
// 装配层用它给 Sync 的 DurableWatermark 接线，pipelined 事务在 WAL 持久化前不外发。
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
	// SendOptionIO 将声明目标的加载及前后置 I/O 放入 I/O 池；handler 始终在业务池。
	// Getter 实现 entity.LoadedChecker（ManagerAccess 已实现）时，声明目标中有未加载实体会在统一准入处
	// 自动走 I/O 阶段；显式 IO 仍有效，用于强制 I/O 准备，也是未实现 LoadedChecker 的
	// 自定义 Getter 预加载冷目标的方式。
	SendOptionIO = func() SendOpt {
		return func(opt *sendOptParam) { opt.Cost = true }
	}
)

// checkRemoteId 在目标 ID 属于 Remote 托管 kind 时把消息标记为需要 Remote 准备（同时走 I/O 阶段）。
// 准备阶段再按运行期标记决定走 Remote 还是本地快路径。
func checkRemoteId(msg *Msg, id int64) {
	if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
		msg.HasRemote = true
		msg.Cost = true
	}
}

// checkRemoteIds 是 checkRemoteId 的切片形式，任一 ID 命中即标记。
func checkRemoteIds(msg *Msg, ids []int64) {
	for _, id := range ids {
		if shouldPrepareRemoteID(entity.ResolveEntityID(id)) {
			msg.HasRemote = true
			msg.Cost = true
			return
		}
	}
}

// checkRemoteGroups 是 checkRemoteId 的分组形式，任一 ID 命中即标记。
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

// shouldPrepareRemoteID 报告 ID 是否携带 Remote 能力位且其 kind 已注册为 Remote 托管。
func shouldPrepareRemoteID(meta entity.EntityIDMeta) bool {
	if meta.Kind == entity.EntityKindNone || !meta.RemoteCapable {
		return false
	}
	if !entity.IsEntityKindRemoteCapable(meta.Kind) {
		return false
	}
	return entity.IsEntityKindRemoteManaged(meta.Kind)
}

// bindMsgContext 把调用方上下文快照绑定到消息；carryBase 为 false（异步消息）时只保留可跨异步边界的部分。
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

// asyncMessageContextSnapshot 生成异步消息携带的上下文。异步入队是新的配置准入边界：固定当前
// RuntimeConfig，后续 reload 不改在队消息；自续发的下一跳重新取当前代际，避免长链永久钉住旧快照。
// Meta / Trace 可传递，Base、KV、Frame、SyncWait 等执行局部状态（尤其事务）不能跨异步边界。
func asyncMessageContextSnapshot(snapshot fctx.ContextSnapshot) fctx.ContextSnapshot {
	return fctx.ContextSnapshot{
		Valid:  true,
		Config: fctx.RuntimeConfig(),
		Meta:   snapshot.Meta,
		Trace:  snapshot.Trace.Clone(),
	}
}

// NestOptionWithWorkerPools 分别配置短业务、长业务和 I/O 池；三者共用 ID 顺序。
func NestOptionWithWorkerPools(short, long, io WorkerPoolConfig) NestOption {
	return func(o *NestOpts) { o.ShortPool = short; o.LongPool = long; o.IOPool = io }
}
