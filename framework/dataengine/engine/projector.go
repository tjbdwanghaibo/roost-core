package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

var (
	errProjectorTransactionHeld = errors.New("dataengine projector: transaction is still under entity lock")
	errProjectorBatchComplete   = errors.New("dataengine projector: replay batch complete")
	ErrProjectionBackpressure   = errors.New("dataengine projector: unacknowledged transaction limit reached")
)

// ProjectionStore 把一笔已准入的事务记录投影到持久存储；必须对同一持久身份幂等。
type ProjectionStore interface {
	Project(context.Context, coredata.CommitRecord) error
}

// BatchProjectionStore 支持本地多 DAO 事务批量投影；必须保留每笔事务的持久身份，
// 保证后续版本写入后仍能重放已成功的批次、多 mutation 事务与带 effect / 回执 / Remote 写的记录
// （重放返回首次投影的结论，不重复暂存）。Projector 据此对这些记录合并 checkpoint；
// 只有单 mutation 快路记录（projectsOnFastPath）不在此列，仍逐条确认。
type BatchProjectionStore interface {
	ProjectionStore
	ProjectBatch(context.Context, []coredata.CommitRecord) error
}

// ParallelProjectionStore 承诺（SupportsParallelProjection 为真时）：冲突键互不相交的记录可以并发调用 Project /
// ProjectBatch；ProjectBatch 与非快路记录的单条投影写持久事务标记，后续版本写入后重放仍识别为已应用。
// Projector 据此在 ProjectionWorkers > 1 时按实体分片并行投影（projector_parallel.go）；未声明的 Store 保持串行。
type ParallelProjectionStore interface {
	BatchProjectionStore
	SupportsParallelProjection() bool
}

// RemoteParallelProjectionStore 承诺 Remote 适配器（存储与提交应用器）可被并发调用：并行投影时含 Remote 写、
// 互不共享 Entity 的记录才能分进不同通道；未声明时 Remote 记录之间保持串行（仍可与普通记录并行）。
type RemoteParallelProjectionStore interface {
	ProjectionStore
	SupportsRemoteParallelProjection() bool
}

type ProjectorOptions struct {
	RetryMin           time.Duration
	RetryMax           time.Duration
	IdlePoll           time.Duration
	ReplayBatchRecords int
	ReplayBatchBytes   int
	ReplayReadBytes    int // 一轮回放保留的逻辑字节上限；首条超大记录允许独占。
	// ProjectionWorkers 是按实体分片并行投影的通道数上限（同时在途的投影调用数），0 取缺省 32，1 = 串行（含 Remote）。
	// 带回执 / effect / Remote 写的记录每条一个 Mongo 事务，吞吐约为“通道数 ÷ 单条事务时延”；缺省 8 时本机联盟 Join
	// 在约 150 次/秒封顶（单条约 50ms，2026-11-05），32 让通道数不先于 Mongo 成为上限。
	// 只在 Store 实现 ParallelProjectionStore 时生效。
	ProjectionWorkers int
	// EffectOrder 是带 effect 的记录之间的投影顺序范围（只在并行投影时起作用），空取 EffectOrderEntity。
	EffectOrder        EffectOrder
	CheckpointRecords  int           // 连续成功记录的 ack 阈值；1 保留逐单元确认。
	CheckpointInterval time.Duration // 投影单元之间检查，阻塞的存储调用仍由其 context 控制。
	MaxUnackedRecords  uint64        // 0 不限制；拒绝发生于 WAL 准入前。
	WarnUnackedRecords uint64        // 0 不设独立预警；达到准入上限也会报告预警。
	CloseWAL           bool
	// OnFatal 在首次确定性投影冲突后异步调用一次（独立 goroutine，panic 被吞掉）。
	// 调用前 fatal 已对准入、Flush 与实体等待方可见；回调里可以同步调用 Close / Shutdown。
	OnFatal func(error)
	// OnEffectsStaged 在一个投影单元把 effect 暂存进 outbox（Mongo 事务已提交）之后同步调用，
	// 必须不阻塞、并发安全（并行窗口的多个通道可能同时调用）；装配接到 OutboxWorker.Kick，让 effect 不必等 outbox 的下一次轮询。只是提示：不调用、晚调用都不影响
	// 投影与持久语义（outbox 仍按轮询兜底）。nil 不通知。
	OnEffectsStaged func()
	// ManualReplay 不启动后台回放循环，回放只由调用方的 Flush / ReplayPass 驱动。
	// 供需要逐步控制回放与 ack 的外部夹具使用（例如注入“投影成功、checkpoint 丢失”后
	// 检查中间状态）；Close 后这些入口同样返回 ErrRuntimeStopped。生产装配不设置。
	ManualReplay bool
}

func DefaultProjectorOptions() ProjectorOptions {
	return ProjectorOptions{
		RetryMin: 10 * time.Millisecond, RetryMax: 5 * time.Second,
		IdlePoll: time.Second, ReplayBatchRecords: 256, ReplayBatchBytes: 4 << 20, ReplayReadBytes: 4 << 20, CloseWAL: true,
		CheckpointRecords: 256, CheckpointInterval: 20 * time.Millisecond,
		ProjectionWorkers: 32,
	}
}

// EffectOrder 决定带 effect 的记录之间按什么范围保持 WAL 顺序（配置 dataengine.projection.effect_order）。
// 它只影响并行投影的冲突键：同一实体（文档 _id）、同一回执、同一 effect ID、同一事务的记录在两种取值下都按 WAL 顺序投影。
type EffectOrder string

const (
	// EffectOrderEntity（缺省，维护者 2026-11-05）：带 effect 的记录只靠实体 / 回执 / effect ID / 事务键串行；不相干实体的
	// 带 effect 记录可以在不同通道并行投影，它们的 effect 暂存（以及随后被 outbox 认领）的先后可能与 WAL 不同。
	// outbox 的发布（多个认领循环、失败 Nack 后继续发后面的）与 JetStream 消费（共享 durable、并发在途、重投）本来就不保证全局顺序。
	EffectOrderEntity EffectOrder = "entity"
	// EffectOrderWAL（可选，缺省关闭）：全部带 effect 的记录共享一个顺序类，彼此按 WAL 顺序逐条投影，effect 进 outbox 的
	// 暂存顺序与 WAL 相同（2026-11-05 之前的唯一行为）。只给依赖“不同实体的 effect 按 WAL 顺序暂存”的进程打开。
	EffectOrderWAL EffectOrder = "wal"
)

type ProjectorStats struct {
	Committed                uint64
	Projected                uint64 // 成功投影次数，含幂等重放；不能与 Committed 相等代替数据一致性校验
	WALUnacked               uint64
	ProjectionFailures       uint64
	FatalProjectionConflicts uint64
	LastError                string
	AdmissionRejected        uint64
	BacklogWarning           bool
	// UnloadingEntities 是当前处于运维卸载屏障（entity.unload）的实体数；UnloadAdmissionRejected 是因此以
	// coredata.ErrEntityUnloading 拒绝的准入次数。
	UnloadingEntities       uint64
	UnloadAdmissionRejected uint64
	// ParallelWindows 是以并行通道投影的窗口数，ParallelLanes 是这些窗口的通道数合计（÷ ParallelWindows 为平均通道数）。
	ParallelWindows uint64
	ParallelLanes   uint64
}

// Projector 负责持久准入（WAL 前的额度与屏障检查、WAL 追加）和 WAL → Mongo 投影。
// Effect 只由 ProjectionStore 暂存进 outbox；投递到消息代理归 OutboxWorker，
// 不在 WAL 确认路径上。
type Projector struct {
	operations operation.Lifetime
	wal        *nestwal.WAL
	store      ProjectionStore
	opts       ProjectorOptions
	ack        func(context.Context, corenest.CommitFence) error
	now        func() time.Time

	ctx        context.Context
	cancel     context.CancelFunc
	kick       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	flushGate  operationGate
	replayGate operationGate

	heldMu              sync.RWMutex
	held                map[coredata.TransactionID]struct{}
	admitted            map[coredata.TransactionID]struct{}
	pendingEntities     map[int64]map[coredata.TransactionID]*entityProjection
	pendingTransactions map[coredata.TransactionID]*entityProjection
	errMu               sync.RWMutex
	lastErr             error
	fatalErr            error
	fatalOnce           sync.Once
	// walTerminalErr：WAL 已 terminal，本进程不会再确认任何记录；errMu 保护，
	// 与 fatalErr 一样先写入、再唤醒全部等待方，reserve 在 heldMu 下复查。
	walTerminalErr  error
	walTerminalOnce sync.Once
	ticketMu        sync.Mutex
	tickets         map[coredata.TransactionID]*projectionTicket
	// unloadingEntities：实体 → 运维卸载屏障的令牌（entity_unload.go）。heldMu 保护，与准入同一临界区检查。
	unloadingEntities map[int64]uint64
	unloadToken       uint64

	// localExecutor 是 Nest 绑定的业务池入口（BindLocalExecutor），运维卸载经它在业务池取实体锁。
	localExecutor atomic.Pointer[func(func()) error]

	committed         atomic.Uint64
	projected         atomic.Uint64
	walUnacked        atomic.Uint64
	failures          atomic.Uint64
	fatalConflicts    atomic.Uint64
	admissionRejected atomic.Uint64
	unloadRejected    atomic.Uint64
	parallelWindows   atomic.Uint64
	parallelLanes     atomic.Uint64
	// laneFailed 是测试缝：并行窗口某通道失败、停止补位标志已置位之后调用。生产为 nil。
	laneFailed func()
}

// NewProjector 校验并补齐选项后启动 WAL terminal 监视；ManualReplay 为假时同时启动后台回放循环。
func NewProjector(wal *nestwal.WAL, store ProjectionStore, options ProjectorOptions) (*Projector, error) {
	if wal == nil || store == nil {
		return nil, errors.New("dataengine projector: WAL and store are required")
	}
	defaults := DefaultProjectorOptions()
	if options.ReplayReadBytes < 0 {
		return nil, errors.New("dataengine projector: replay read bytes must not be negative")
	}
	if options.ReplayReadBytes == 0 {
		options.ReplayReadBytes = defaults.ReplayReadBytes
	}
	if options.ProjectionWorkers < 0 || options.ProjectionWorkers > 64 {
		return nil, errors.New("dataengine projector: projection workers must be between 0 and 64")
	}
	if options.ProjectionWorkers == 0 {
		options.ProjectionWorkers = defaults.ProjectionWorkers
	}
	switch options.EffectOrder {
	case "":
		options.EffectOrder = EffectOrderEntity
	case EffectOrderEntity, EffectOrderWAL:
	default:
		return nil, fmt.Errorf("dataengine projector: unknown effect order %q (want %q or %q)", options.EffectOrder, EffectOrderEntity, EffectOrderWAL)
	}
	if options.CheckpointRecords < 0 || options.CheckpointInterval < 0 ||
		(options.MaxUnackedRecords > 0 && options.WarnUnackedRecords > options.MaxUnackedRecords) {
		return nil, errors.New("dataengine projector: invalid checkpoint or backlog limits")
	}
	if options.CheckpointRecords == 0 {
		options.CheckpointRecords = defaults.CheckpointRecords
	}
	if options.CheckpointInterval == 0 {
		options.CheckpointInterval = defaults.CheckpointInterval
	}
	if options.RetryMin <= 0 {
		options.RetryMin = defaults.RetryMin
	}
	if options.RetryMax <= 0 {
		options.RetryMax = defaults.RetryMax
	}
	if options.RetryMax < options.RetryMin {
		return nil, errors.New("dataengine projector: retry max is smaller than retry min")
	}
	if options.IdlePoll <= 0 {
		options.IdlePoll = defaults.IdlePoll
	}
	if options.ReplayBatchRecords <= 0 {
		options.ReplayBatchRecords = defaults.ReplayBatchRecords
	}
	if options.ReplayBatchBytes <= 0 {
		options.ReplayBatchBytes = defaults.ReplayBatchBytes
	}
	ctx, cancel := context.WithCancel(context.Background())
	projector := &Projector{
		wal: wal, store: store, opts: options, ack: wal.Ack, now: time.Now, ctx: ctx, cancel: cancel,
		kick: make(chan struct{}, 1), done: make(chan struct{}), held: make(map[coredata.TransactionID]struct{}), admitted: make(map[coredata.TransactionID]struct{}),
		tickets: make(map[coredata.TransactionID]*projectionTicket),
	}
	go projector.watchWALTerminal()
	metrics.SetGauge("dataengine.projection.pending", nil, 0)
	if options.ManualReplay {
		// 没有后台循环：Close 不需要等待，done 从一开始就处于“循环已退出”。
		close(projector.done)
		return projector, nil
	}
	go projector.run()
	projector.signal()
	return projector, nil
}

type projectionTicket struct {
	done chan struct{}
	err  error
}

func (ticket *projectionTicket) Done() <-chan struct{} { return ticket.done }

func (ticket *projectionTicket) Err() error {
	select {
	case <-ticket.done:
		return ticket.err
	default:
		return nil
	}
}

// CommitSystem 持久准入一笔框架内部事务（memory 级别按 strict 处理），返回的票据
// 在 Mongo 投影完成后才结束，而不是 WAL fsync 后。
func (projector *Projector) CommitSystem(ctx context.Context, record coredata.CommitRecord) (coredata.ProjectionTicket, error) {
	if projector == nil || projector.wal == nil {
		return nil, errors.New("dataengine projector: not initialized")
	}
	if !projector.operations.Begin() {
		return nil, ErrRuntimeStopped
	}
	defer projector.operations.End()
	if fatal := projector.fatal(); fatal != nil {
		return nil, fatal
	}
	if record.Durability == coredata.DurabilityMemory {
		record.Durability = coredata.DurabilityStrict
	}
	ticket := &projectionTicket{done: make(chan struct{})}
	projector.ticketMu.Lock()
	if _, duplicate := projector.tickets[record.ID]; duplicate {
		projector.ticketMu.Unlock()
		return nil, fmt.Errorf("dataengine projector: duplicate system transaction %s", record.ID.String())
	}
	projector.tickets[record.ID] = ticket
	projector.ticketMu.Unlock()
	if err := projector.reserve(record, false); err != nil {
		projector.removeTicket(record.ID)
		return nil, err
	}
	if _, err := projector.wal.Append(ctx, record); err != nil {
		projector.discard(record.ID)
		projector.removeTicket(record.ID)
		return nil, err
	}
	projector.committed.Add(1)
	projector.signal()
	return ticket, nil
}

// Commit 是 Nest 的阻塞提交入口：准入后调用 WAL.Append，async 只等写入，strict 等所在批 fsync。
func (projector *Projector) Commit(ctx context.Context, record corenest.CommitRecord) error {
	if projector == nil || projector.wal == nil {
		return errors.New("dataengine projector: not initialized")
	}
	if !projector.operations.Begin() {
		return ErrRuntimeStopped
	}
	defer projector.operations.End()
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	if err := projector.reserve(record, true); err != nil {
		return err
	}
	appendStart := hoptrace.Now()
	if _, err := projector.wal.Append(ctx, record); err != nil {
		projector.discard(record.ID)
		return err
	}
	if !appendStart.IsZero() {
		// 逐跳计时：WAL 追加（strict 等所在批 fsync）；投影等待从这里开始算。
		hoptrace.Since("h07.de.wal_append", appendStart)
		hoptrace.Mark("tx-committed/", record.ID.String())
	}
	projector.committed.Add(1)
	projector.signal()
	return nil
}

// Enqueue 是 Nest pipelined 提交入口：准入后把记录交给 WAL group-commit，返回的票据在所在批 fsync 后结束。
func (projector *Projector) Enqueue(ctx context.Context, record corenest.CommitRecord) (corenest.CommitTicket, error) {
	if projector == nil || projector.wal == nil {
		return nil, errors.New("dataengine projector: not initialized")
	}
	if !projector.operations.Begin() {
		return nil, ErrRuntimeStopped
	}
	defer projector.operations.End()
	if fatal := projector.fatal(); fatal != nil {
		return nil, fatal
	}
	if err := projector.reserve(record, true); err != nil {
		return nil, err
	}
	ticket, err := projector.wal.Enqueue(ctx, record)
	if err != nil {
		projector.discard(record.ID)
		return nil, err
	}
	projector.committed.Add(1)
	// Commit 同步追加后立即唤醒回放；pipelined 记录要等 fsync 才可回放，而事务释放
	// 通常早于 fsync，那时唤醒会扑空并睡满 IdlePoll，所以要等票据结束再唤醒。
	go projector.signalWhenDurable(record.ID, ticket)
	return ticket, nil
}

func (projector *Projector) signalWhenDurable(id coredata.TransactionID, ticket corenest.CommitTicket) {
	select {
	case <-ticket.Done():
		// 写入结果未知（WAL 已 terminal）时本进程不会再投影这条记录；等它的冷加载
		// 立即拿到 WAL 错误，而不是等到各自截止时间。
		if err := ticket.Err(); err != nil {
			projector.finishEntities(id, err)
		}
		projector.signal()
	case <-projector.ctx.Done():
	}
}

func (projector *Projector) DurableLSN() uint64 {
	if projector == nil || projector.wal == nil {
		return 0
	}
	return projector.wal.DurableLSN()
}

// TransactionReleased 在 Nest 释放事务的实体锁后调用；之后该记录才允许投影。
func (projector *Projector) TransactionReleased(id corenest.TransactionID) {
	projector.release(id)
	projector.signal()
}

// Flush 先让 WAL 落盘，再反复执行回放直到一轮没有可处理的记录。
func (projector *Projector) Flush(ctx context.Context) error {
	if projector == nil || projector.wal == nil {
		return nil
	}
	if !projector.operations.Begin() {
		return ErrRuntimeStopped
	}
	defer projector.operations.End()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := projector.flushGate.acquire(ctx); err != nil {
		return err
	}
	defer projector.flushGate.release()
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	if err := projector.wal.Sync(ctx); err != nil {
		return err
	}
	for {
		processed, err := projector.ReplayPass(ctx)
		if err != nil {
			projector.recordFailure(err)
			return err
		}
		if processed == 0 {
			projector.setLastError(nil)
			return nil
		}
	}
}

// OverrideAck 替换 checkpoint 确认钩子，只供集成夹具注入“投影已落库、checkpoint 丢失”
// 的重启场景；生产装配不调用。
func (projector *Projector) OverrideAck(ack func(context.Context, corenest.CommitFence) error) {
	if projector == nil || ack == nil {
		return
	}
	projector.ack = ack
}

func (projector *Projector) recordFailure(err error) {
	projector.failures.Add(1)
	projector.setLastError(err)
	projector.isFatalProjection(err)
}

// isFatalProjection 判断 err 是否为确定性投影冲突；是则首次触发 fatal：记录 fatalErr、
// 唤醒全部等待方，再异步调用 OnFatal。
func (projector *Projector) isFatalProjection(err error) bool {
	// 批次退回逐条处理只是路由信号，不是结论，无论从哪条路径传来都不能 fence 投影器。
	if errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
		return false
	}
	if !errors.Is(err, ErrProjectionConflict) && !errors.Is(err, ErrTransactionIdentity) && !errors.Is(err, ErrReceiptIdentity) {
		return false
	}
	projector.fatalOnce.Do(func() {
		projector.fatalConflicts.Add(1)
		projector.errMu.Lock()
		projector.fatalErr = err
		projector.errMu.Unlock()
		// fatal 之后本进程不再投影 fatal 记录及其后的任何记录，所以唤醒全部待投影
		// 等待方（不只本批次），让冷加载与系统票据立即拿到可判别的 fatal。fatalErr
		// 已先于唤醒写入，reserve 在同一把 heldMu 下复查，唤醒之后不会再有新的等待项。
		projector.completeAllTickets(err)
		if onFatal := projector.opts.OnFatal; onFatal != nil {
			// 异步投递（与 nestwal.Options.OnFatal 相同）：这里可能在 ReplayPass/Flush 持有
			// operation、或后台循环尚未退出时被调用，同步回调里 Close 会等自己。
			// fatalErr 与等待方唤醒已在上面完成，所以回调开始前准入已被拒绝、
			// 等待方已拿到 fatal；回调只负责进程级 fence，允许在其中同步 Close。
			go func() {
				defer func() { _ = recover() }()
				onFatal(err)
			}()
		}
	})
	return true
}

// watchWALTerminal 在 WAL 进入 terminal 时立即唤醒全部待投影等待方。WAL terminal 之后
// Ack 恒失败、准入恒失败，本进程不会再推进任何记录；只在等待前后检查 Healthy 的等待方要等到自己的截止
// 时间或 Close。Projector 关闭时退出。
func (projector *Projector) watchWALTerminal() {
	select {
	case <-projector.wal.Terminated():
		projector.walTerminated(projector.wal.Healthy())
	case <-projector.ctx.Done():
	}
}

// observeAckError 把 checkpoint 失败里的 WAL terminal 当作同一个信号：nestwal.Ack 只在 WAL 已 terminal
// （或本次 fsync 失败、随即粘滞为 terminal）时返回 ErrCommitIndeterminate。
func (projector *Projector) observeAckError(err error) {
	if errors.Is(err, corenest.ErrCommitIndeterminate) {
		projector.walTerminated(err)
	}
}

// walTerminated 与 fatal 同一不变量：先写 walTerminalErr，再唤醒全部待投影等待方与系统票据；reserve 在
// heldMu 下复查，唤醒之后不会再登记新的等待项。错误保留 errors.Is(corenest.ErrCommitIndeterminate)：
// 已准入记录的投影结果对本进程是未知的（可能已落库、ack 丢失），不是明确失败。
func (projector *Projector) walTerminated(cause error) {
	if cause == nil {
		return
	}
	projector.walTerminalOnce.Do(func() {
		err := fmt.Errorf("dataengine projector: WAL is terminal, this process will not confirm pending projections: %w", cause)
		projector.errMu.Lock()
		projector.walTerminalErr = err
		projector.errMu.Unlock()
		projector.completeAllTickets(err)
	})
}

func (projector *Projector) walTerminal() error {
	projector.errMu.RLock()
	err := projector.walTerminalErr
	projector.errMu.RUnlock()
	return err
}

func (projector *Projector) Stats() ProjectorStats {
	stats := ProjectorStats{
		Committed: projector.committed.Load(), Projected: projector.projected.Load(),
		WALUnacked:         projector.walUnacked.Load(),
		AdmissionRejected:  projector.admissionRejected.Load(),
		ProjectionFailures: projector.failures.Load(), FatalProjectionConflicts: projector.fatalConflicts.Load(),
		UnloadAdmissionRejected: projector.unloadRejected.Load(),
		ParallelWindows:         projector.parallelWindows.Load(), ParallelLanes: projector.parallelLanes.Load(),
	}
	projector.heldMu.RLock()
	stats.UnloadingEntities = uint64(len(projector.unloadingEntities))
	projector.heldMu.RUnlock()
	warning := projector.opts.WarnUnackedRecords
	if warning == 0 {
		warning = projector.opts.MaxUnackedRecords
	}
	stats.BacklogWarning = warning > 0 && stats.WALUnacked >= warning
	projector.errMu.RLock()
	if projector.lastErr != nil {
		stats.LastError = projector.lastErr.Error()
	}
	projector.errMu.RUnlock()
	return stats
}

func (projector *Projector) Healthy() error {
	if projector == nil || projector.wal == nil {
		return errors.New("dataengine projector: not initialized")
	}
	projector.errMu.RLock()
	err := errors.Join(projector.lastErr, projector.fatalErr, projector.walTerminalErr)
	projector.errMu.RUnlock()
	return errors.Join(projector.wal.Healthy(), err)
}

// Close 停止准入并等待在途操作与后台回放退出，再以 context.Canceled 结束剩余等待方；
// CloseWAL 为真时最后关闭 WAL。不做最终 Flush，需要排空时用 Shutdown。
func (projector *Projector) Close(ctx context.Context) error {
	if projector == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	drained := projector.operations.Stop()
	projector.closeOnce.Do(projector.cancel)
	select {
	case <-drained:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-projector.done:
		projector.completeAllTickets(context.Canceled)
		if projector.opts.CloseWAL {
			return projector.wal.Close(ctx)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown 在尚未关闭时先 Flush 再 Close；已开始关闭则只 Close。
func (projector *Projector) Shutdown(ctx context.Context) error {
	if projector != nil && projector.ctx.Err() != nil {
		return projector.Close(ctx)
	}
	return errors.Join(projector.Flush(ctx), projector.Close(ctx))
}

// reserve 在同一锁内检查与预留额度，不能让并发 Commit 越过上限。
func (projector *Projector) reserve(record coredata.CommitRecord, held bool) error {
	id := record.ID
	projector.heldMu.Lock()
	defer projector.heldMu.Unlock()
	// 调用方在准入前已查过 fatal；这里在 heldMu 下复查，关闭“查过之后才 fatal”的窗口：
	// fatal 唤醒扫描也持 heldMu，之后登记的等待项将无人唤醒。
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	if terminal := projector.walTerminal(); terminal != nil {
		return terminal
	}
	if _, exists := projector.admitted[id]; !exists {
		// 运维卸载屏障（entity.unload）：同一临界区，登记之后不会再有写到该实体的记录准入。
		if err := projector.checkUnloadingEntitiesLocked(record); err != nil {
			return err
		}
		if limit := projector.opts.MaxUnackedRecords; limit > 0 && uint64(len(projector.admitted)) >= limit {
			projector.admissionRejected.Add(1)
			return ErrProjectionBackpressure
		}
		projector.admitted[id] = struct{}{}
		projector.trackEntitiesLocked(record)
		metrics.SetGauge("dataengine.projection.pending", nil, int64(projector.walUnacked.Add(1)))
	}
	if held {
		projector.held[id] = struct{}{}
	}
	return nil
}

func (projector *Projector) discard(id coredata.TransactionID) {
	projector.finishEntities(id, nil)
	projector.heldMu.Lock()
	delete(projector.held, id)
	if _, ok := projector.admitted[id]; ok {
		delete(projector.admitted, id)
		metrics.SetGauge("dataengine.projection.pending", nil, int64(projector.walUnacked.Add(^uint64(0))))
	}
	projector.heldMu.Unlock()
}

func (projector *Projector) release(id coredata.TransactionID) {
	projector.heldMu.Lock()
	delete(projector.held, id)
	projector.heldMu.Unlock()
}

func (projector *Projector) isHeld(id coredata.TransactionID) bool {
	projector.heldMu.RLock()
	_, held := projector.held[id]
	projector.heldMu.RUnlock()
	return held
}

func (projector *Projector) signal() {
	select {
	case projector.kick <- struct{}{}:
	default:
	}
}

func (projector *Projector) setLastError(err error) {
	projector.errMu.Lock()
	projector.lastErr = err
	projector.errMu.Unlock()
}

func (projector *Projector) fatal() error {
	projector.errMu.RLock()
	err := projector.fatalErr
	projector.errMu.RUnlock()
	return err
}

func (projector *Projector) completeProjection(id coredata.TransactionID, err error) {
	projector.finishEntities(id, err)
	projector.ticketMu.Lock()
	ticket := projector.tickets[id]
	if ticket != nil {
		delete(projector.tickets, id)
		ticket.err = err
		close(ticket.done)
	}
	projector.ticketMu.Unlock()
}

func (projector *Projector) removeTicket(id coredata.TransactionID) {
	projector.ticketMu.Lock()
	delete(projector.tickets, id)
	projector.ticketMu.Unlock()
}

func (projector *Projector) completeAllTickets(err error) {
	projector.heldMu.Lock()
	for id := range projector.pendingTransactions {
		projector.finishEntitiesLocked(id, err)
	}
	projector.heldMu.Unlock()
	projector.ticketMu.Lock()
	for id, ticket := range projector.tickets {
		delete(projector.tickets, id)
		ticket.err = err
		close(ticket.done)
	}
	projector.ticketMu.Unlock()
}

var _ corenest.TransactionCommitter = (*Projector)(nil)
var _ corenest.TransactionReleaseNotifier = (*Projector)(nil)
var _ corenest.PipelinedTransactionCommitter = (*Projector)(nil)
var _ coredata.SystemCommitter = (*Projector)(nil)
