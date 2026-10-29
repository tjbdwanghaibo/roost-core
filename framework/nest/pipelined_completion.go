package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// pipelined 提交第二阶段（见 NEST_PIPELINED_COMMIT.md §10）：派发 worker 不在提交票据上等待，事务的完成
// （AfterCommit 回调、释放通知、回复）交给完成泵，worker 继续处理下一条消息。
//
// 顺序契约：完成在仍持实体锁时提交，泵按 FIFO 转发，执行按主实体哈希到 worker 池——所以同一实体的完成按提交
// （LSN）顺序执行，与派发 worker 就地执行时完全相同。泵本身只等票据并转发；业务回调在池里运行，不会队头阻塞
// 其他实体完成的解析。

const (
	defaultCompletionWorkers  = 4
	defaultCompletionQueueCap = 8192
)

// completionRoute 是一笔事务的 pipelined 完成路线。pump 是引擎的完成泵（nil = 本引擎没有异步完成）；deferred 表示
// 本事务的 handler 被 NestOptionWithPipelinedAsyncCompletion 点名使用异步完成（按 handler，维护者 2026-10-29 决定）。
// pump 非 nil 而 deferred 为假：第一阶段 worker 内等待，只在完成链上占位（见 commitPipelined）。
type completionRoute struct {
	pump     *completionPump
	deferred bool
}

// pipelinedCompletion 是提交给完成泵的一项：等 ticket 持久后执行 complete。
type pipelinedCompletion struct {
	ticket      CommitTicket
	entityID    int64
	complete    func(error)
	handler     string
	stages      bool
	submittedAt time.Time
}

// completionTask 是完成池的任务包装（worker.Pool 要求任务实现 OnRelease）。
type completionTask struct {
	run func()
}

func (t *completionTask) OnRelease() {}

// completionPump 按 FIFO 等待提交票据，再把完成交给按实体哈希的 worker 池；结果不确定时经 fence 停止引擎。
type completionPump struct {
	queue chan pipelinedCompletion
	pool  *worker.Pool[*completionTask]
	done  chan struct{}
	fence func(error)
	// closeMu 让 submit 中的 closed 检查与入队成为一步，入队不会与 stop 关闭队列竞争。
	closeMu sync.RWMutex
	closed  atomic.Bool
	once    sync.Once

	// chains 是按实体的顺序链。实体的链尾在持实体锁时登记，所以链序即提交（LSN）顺序；每条执行路径——池 worker、
	// 泵内就地执行、派发 worker 降级——都先等前驱完成再运行，过载时同实体的完成顺序也无条件成立。
	chainMu sync.Mutex
	chains  map[int64]chan struct{}

	// asyncPending 按实体 ID 分片计数“已在链上占位、尚未释放的异步完成节点”（linkAsync 加、release 减）。普通事务
	// （strict / async / memory）提交时只读自己声明实体所在分片的计数：为 0 走快路径，不碰 chainMu、不分配；非 0 才在
	// 链上占位并在回复前等待（见 orderBehindAsyncCompletions）。
	//
	// 放在泵里、按 ID 分片而不是放在实体上：链本身按 ID 登记，同 ID 的实例被替换（Destroy 后重建、卸载后重载）时
	// 计数仍跟着 ID；也不给 entity 包加只为 Nest 服务的字段。分片只会多报（同分片别的 ID 有在途异步完成）：多报的
	// 代价是一次链登记，前驱为空时不等待；不会漏报——计数在持实体锁时增加，读方取得同一把锁之后才读，减少发生在
	// 节点完成（AfterCommit、回复）之后。
	asyncPending [completionPendingShards]completionPendingShard
	// orderedCommits 统计普通事务因同实体有在途异步完成而在链上排队的次数（慢路径才写；测试与排障用）。
	orderedCommits atomic.Uint64
}

// completionPendingShards 是 asyncPending 的分片数（2 的幂）。每片独占一条缓存行，读多写少，256 片约 16KB。
const completionPendingShards = 256

// completionPendingShard 是一片计数，补齐到 64 字节避免相邻分片伪共享。
type completionPendingShard struct {
	n atomic.Int64
	_ [56]byte
}

// pendingShard 返回实体 ID 所在的计数分片（乘法散列取高 8 位：ID 的低位按种类 / 序号编排，不能直接取模）。
func (p *completionPump) pendingShard(entityID int64) *completionPendingShard {
	return &p.asyncPending[(uint64(entityID)*0x9E3779B97F4A7C15)>>56]
}

// asyncCompletionPending 报告实体 ID 所在分片是否有在途的异步完成。只做一次原子读；调用方须持该实体的锁，
// 结论才对“本事务之前提交的异步完成”成立。
func (p *completionPump) asyncCompletionPending(entityID int64) bool {
	return p.pendingShard(entityID).n.Load() != 0
}

// completionOrder 是一个实体完成链上的一节。
type completionOrder struct {
	pump        *completionPump
	entityID    int64
	prev        chan struct{}
	mine        chan struct{}
	async       bool // 异步完成节点：登记时计入 asyncPending，释放时扣回
	releaseOnce sync.Once
}

// link 在实体的完成顺序里为本事务占位。必须在持实体锁时调用。
func (p *completionPump) link(entityID int64) *completionOrder {
	order := &completionOrder{pump: p, entityID: entityID, mine: make(chan struct{})}
	p.chainMu.Lock()
	order.prev = p.chains[entityID]
	p.chains[entityID] = order.mine
	p.chainMu.Unlock()
	return order
}

// linkAsync 同 link，并把节点计入 asyncPending：之后在这个实体上提交的普通事务要排在它后面（见
// orderBehindAsyncCompletions）。必须在持实体锁时调用，计数先于登记增加。
func (p *completionPump) linkAsync(entityID int64) *completionOrder {
	p.pendingShard(entityID).n.Add(1)
	order := p.link(entityID)
	order.async = true
	return order
}

// await 等待同一实体的上一个完成结束。不会死锁：前驱只等严格更早的节点和自己的票据，票据由 WAL 独立完成。
func (o *completionOrder) await() {
	if o == nil || o.prev == nil {
		return
	}
	<-o.prev
}

// release 发布本完成已结束；后面没有登记后继时删除该实体的链条目。
//
// 有意设计为幂等：占位发生在泵决定能否接手之前，降级路径上调用方要能防御性地履行释放义务而不重复关闭。
// 未释放的节点会永久阻塞该实体此后的所有完成，既无超时也无指标可见。
func (o *completionOrder) release() {
	if o == nil {
		return
	}
	o.releaseOnce.Do(func() {
		close(o.mine)
		o.pump.chainMu.Lock()
		if o.pump.chains[o.entityID] == o.mine {
			delete(o.pump.chains, o.entityID)
		}
		o.pump.chainMu.Unlock()
		if o.async {
			// 最后扣回：读到 0 的普通事务可以确信此前的异步完成（含回调与回复）都已结束。
			o.pump.pendingShard(o.entityID).n.Add(-1)
		}
	})
}

// newCompletionPump 构造完成泵；workers、queueCap <= 0 取默认值。
func newCompletionPump(workers, queueCap int) *completionPump {
	if workers <= 0 {
		workers = defaultCompletionWorkers
	}
	if queueCap <= 0 {
		queueCap = defaultCompletionQueueCap
	}
	p := &completionPump{
		queue:  make(chan pipelinedCompletion, queueCap),
		done:   make(chan struct{}),
		chains: make(map[int64]chan struct{}),
	}
	p.pool = worker.NewPool[*completionTask](worker.PoolConfig{
		Name:      "nest_pipelined_completion",
		WorkerNum: workers,
		QueueCap:  queueCap,
	}, func(task *completionTask) { task.run() })
	return p
}

// start 启动完成池与泵。
func (p *completionPump) start() {
	p.pool.Start()
	go p.run()
}

// run 是泵循环：按提交顺序等每个票据，再把完成交给池；队列关闭并排空后退出。
func (p *completionPump) run() {
	defer close(p.done)
	for entry := range p.queue {
		// 票据按 LSN 顺序完成（前缀持久），跨实体略微乱序入队的条目因 FIFO 等待最多多一个组提交批次的延迟。
		observeNestStage(entry.handler, "commit_queue", entry.submittedAt)
		waitStart := startNestStage(entry.stages)
		<-entry.ticket.Done()
		observeNestStage(entry.handler, "durable_wait", waitStart)
		err := entry.ticket.Err()
		complete := entry.complete
		readyAt := startNestStage(entry.stages)
		task := &completionTask{run: func() {
			observeNestStage(entry.handler, "completion_queue", readyAt)
			complete(err)
		}}
		if p.pool.Dispatch(entry.entityID, task) != nil {
			// 池拒绝（饱和或停止中）：就地执行而不是丢弃——完成是必须送达的持久承诺。complete 会等实体前驱，
			// 顺序在这里同样成立；代价是池饱和期间泵的队头延迟。
			//
			// 与池 worker 相同的恢复边界：这个 goroutine 就是泵本身，逃逸的 panic 会让进程退出。
			goroutine.SafeFunc(task.run)
		}
	}
}

// submit 把一个完成交给泵。调用方持实体锁，所以从不阻塞：队列满返回 false，调用方降级为第一阶段的 worker 内等待。
func (p *completionPump) submit(entry pipelinedCompletion) bool {
	if p == nil {
		return false
	}
	// 读锁让 closed 检查与发送相对 stop 成为原子的一步；stop 取得写锁后才关闭队列。
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed.Load() {
		return false
	}
	select {
	case p.queue <- entry:
		return true
	default:
		return false
	}
}

// completionHandoff 显式记录回复所有权和解锁屏障；排序位置仍在锁内建立。
// deferred=true 后 tx 归完成池；否则由当前 worker 执行 complete，并兜底释放排序位置。
type completionHandoff struct {
	deferred     bool
	complete     func(error)
	releaseOrder func()
	unlocked     chan struct{}
	releaseErr   error // 解锁屏障关闭前写入，完成方只能在屏障后读取。
}

// prepareCompletion 只复制完成所需值，不让池化 Msg 逃到完成 goroutine。
func prepareCompletion(pump *completionPump, msg *Msg, es []entity.IThreadSafeEntity, tx *RollbackTx, ticket CommitTicket, handler string, ret any) *completionHandoff {
	handoff := &completionHandoff{unlocked: make(chan struct{})}
	retChan := msg.RetChan
	waitStart := time.Now()
	entityID := completionEntityID(es, msg)
	// 仍持实体锁时占位：链序即提交顺序。计入在途异步完成，此后同实体的普通事务在回复前等它（orderBehindAsyncCompletions）。
	order := pump.linkAsync(entityID)
	complete := func(ticketErr error) {
		unlockStart := startNestStage(tx.stageMetrics)
		<-handoff.unlocked
		observeNestStage(handler, "completion_unlock", unlockStart)
		orderStart := startNestStage(tx.stageMetrics)
		order.await()
		observeNestStage(handler, "completion_order", orderStart)
		defer observeNestStage(handler, "completion", startNestStage(tx.stageMetrics))
		defer order.release()
		metrics.ObserveDuration("nest.pipelined.durable_wait", metrics.Labels{"handler": handler}, time.Since(waitStart))
		if ticketErr != nil {
			// 与 worker 内路径相同的结论：结果不确定从不回滚——后继可能已基于这一状态，最终历史由 WAL 回放决定。
			tx.abandon()
			if pump.fence != nil {
				pump.fence(ticketErr)
			}
			metrics.IncCounter("nest.pipelined.async_total", metrics.Labels{"result": "indeterminate"}, 1)
			failure := errors.Join(ticketErr, handoff.releaseErr)
			if retChan != nil {
				retChan <- failure
			} else {
				slog.Error("nest pipelined completion failed", "handler", handler, "err", failure)
			}
			return
		}
		if commitErr := errors.Join(tx.commit(true), handoff.releaseErr); commitErr != nil {
			// WAL 已成功，释放 hook 或完成回调失败仍需报告；不回滚、不回复业务成功。
			// 事务已提交：统一带 ErrAfterCommitFailed，保留原因链。
			if !errors.Is(commitErr, ErrAfterCommitFailed) {
				commitErr = fmt.Errorf("%w: %w", ErrAfterCommitFailed, commitErr)
			}
			metrics.IncCounter("nest.pipelined.async_total", metrics.Labels{"result": "completion_failed"}, 1)
			if retChan != nil {
				retChan <- commitErr
			} else {
				slog.Error("nest pipelined completion failed", "handler", handler, "err", commitErr)
			}
			return
		}
		metrics.IncCounter("nest.pipelined.async_total", metrics.Labels{"result": "ok"}, 1)
		if retChan != nil {
			retChan <- ret
		}
	}
	handoff.complete = complete
	handoff.releaseOrder = order.release
	if pump.submit(pipelinedCompletion{ticket: ticket, entityID: entityID, complete: complete, handler: handler, stages: tx.stageMetrics, submittedAt: startNestStage(tx.stageMetrics)}) {
		msg.deferredCompletion = true
		handoff.deferred = true
		return handoff
	}
	metrics.IncCounter("nest.pipelined.async_total", metrics.Labels{"result": "degraded"}, 1)
	// 调用方经 complete() 回复，派发路径不能再发 RetChan。
	msg.deferredCompletion = true
	return handoff
}

// completionEntityID 选择完成链的主实体键：第一个非 nil 实体。
// 完成链只保证相同主实体的顺序；业务准入则按全部声明 ID 建依赖链，
// 两者不是同一层约束，也不依赖把 ID 哈希到固定 worker。
func completionEntityID(es []entity.IThreadSafeEntity, msg *Msg) int64 {
	for _, e := range es {
		if e != nil {
			return e.GUId()
		}
	}
	return msg.Key()
}

// commitOrdering 是不走 pipelined 完成路径的事务（strict / async / memory，以及回退到 strict 路径的 pipelined）在完成链上
// 的占位：只在它的某个声明实体有在途异步完成时建立。维护者 2026-10-29 决定（选 A）：同一实体上每笔事务对外可见的
// 效果——回复、AfterCommit——都按提交（LSN / 准入）序；异步完成事务 T1 放锁后，同实体上紧接着提交的普通事务 T2 的
// 回复与 AfterCommit 不能先于 T1 的完成，async 的 T2 回复更不能把 T1 尚未持久的状态带出进程。
//
// 时序（全部在派发 goroutine 上）：
//  1. 开始执行 handler 之前、仍持声明实体的锁：按声明实体读 asyncPending，有在途的才在这些实体的链上占位（链序 = 锁序 =
//     提交序），并把 await 登记为 Guard 解锁后的第一个回调；
//  2. 事务照常在锁内提交（strict 等 fsync、async 写入、memory 无记录），AfterCommit 照常排到 Guard 解锁之后；
//  3. Guard 释放全部实体锁之后先执行 await 等前驱完成，再执行嵌套独立事务与本事务的 AfterCommit，最后 release；
//     回复由 dispatchNest 在这之后发出。
//
// 不持锁等待：await 在 Guard 释放全部实体锁之后运行，等待期间本事务不持任何实体锁，只占着派发 worker 与同 ID 准入链尾。
// 不会死锁：await 只等链上严格更早的节点；异步完成节点只等它自己的解锁屏障（它在放锁时就已关闭，早于本事务取得锁）、
// 更早的节点与 WAL 票据（WAL 独立推进），它的执行（完成池、泵内就地、派发 worker 降级）都不需要本事务占着的派发 worker
// 或准入链尾；普通节点同理只等更早的节点。一个节点的全部登记都在它持有全部相关锁时完成，前驱放锁之后后继才能取得
// 同一把锁再登记，所以“等待”只指向登记更早的节点，构不成环。前提（第一阶段 pipelined 占位早已依赖它）：异步完成的
// AfterCommit 不同步等待同一实体上的 Nest 请求——那个请求排在本事务占着的准入链尾之后。框架内的异步完成只有 saga 原生
// 步骤，回调只记下结论。
//
// 多实体：本事务按全部声明实体检查（主实体与次实体），每个有在途异步完成的实体各占一节、全部等完；异步完成事务自己
// 只在主实体（completionEntityID）上占位，所以只碰它次实体的普通事务不排在它后面。handler 内 Cast 动态取得的实体
// 不检查（与同 ID 准入顺序只覆盖声明 ID 一致）。没有 Guard 作用域（不经派发的直调）时不排序。
type commitOrdering struct {
	guard   *entity.EntityGuard
	orders  []*completionOrder
	handler string
	stages  bool
}

// orderBehindAsyncCompletions 在仍持声明实体锁时检查 es 上有没有在途异步完成，有则建立 commitOrdering 并登记解锁后的
// 等待；调用方在事务收尾时调用 finish 登记释放。没有在途（常见路径）返回 nil：每个实体一次原子读，不加锁、不分配。
func (route completionRoute) orderBehindAsyncCompletions(es []entity.IThreadSafeEntity, handler string, stages bool) *commitOrdering {
	pump := route.pump
	if pump == nil {
		return nil
	}
	var ordering *commitOrdering
	for _, e := range es {
		if e == nil {
			continue
		}
		id := e.GUId()
		if !pump.asyncCompletionPending(id) {
			continue
		}
		if ordering == nil {
			scope := entity.CurrentGuardScope()
			if scope == nil || scope.Guard() == nil {
				return nil
			}
			ordering = &commitOrdering{guard: scope.Guard(), handler: handler, stages: stages}
		} else if ordering.holds(id) {
			// es 可以有重复 ID：同一实体只占一节，否则后一节会等前一节——等自己。
			continue
		}
		ordering.orders = append(ordering.orders, pump.link(id))
	}
	if ordering != nil {
		pump.orderedCommits.Add(1)
		ordering.guard.AppendPostRelease(ordering.await)
	}
	return ordering
}

// holds 报告本事务是否已在 entityID 的链上占位。
func (o *commitOrdering) holds(entityID int64) bool {
	for _, order := range o.orders {
		if order.entityID == entityID {
			return true
		}
	}
	return false
}

// await 等全部占位实体上的前驱完成。作为 Guard 解锁后的回调运行，此时不持实体锁。
func (o *commitOrdering) await() {
	start := startNestStage(o.stages)
	for _, order := range o.orders {
		order.await()
	}
	observeNestStage(o.handler, "completion_order", start)
}

// finish 把释放登记在 Guard 解锁回调的末尾：排在本事务的 AfterCommit 之后。调用方用 defer 调用，handler 或提交
// panic 时同样登记——未释放的节点会永久挡住该实体此后的异步完成。
func (o *commitOrdering) finish() {
	o.guard.AppendPostRelease(o.release)
}

func (o *commitOrdering) release() {
	for _, order := range o.orders {
		order.release()
	}
}

// stop 排空全部已接受的完成并停止池。在派发器排空后调用，不会有新的提交与关闭竞争。
//
// 按三步停机：关闭队列只发起一次；每次调用都在自己的 ctx 内等泵排空、再等池停止。ctx 先结束返回 ctx 错误，
// 泵与池保持“停止中”，用新 ctx 重试会继续等同一批完成，排空之后才返回 nil。
func (p *completionPump) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		// 关闭队列之前等在途的 submit 全部结束。
		p.closeMu.Lock()
		p.closed.Store(true)
		p.closeMu.Unlock()
		close(p.queue)
	})
	select {
	case <-p.done:
		return p.pool.StopWithContext(ctx)
	case <-ctx.Done():
		// 仍发起池停止：泵在后台继续排空，被拒绝的 Dispatch 会降级为就地执行，所以这里停止能限制泄漏，
		// 不让池 goroutine 在超时的关闭之后永远存活。池停止可重入，重试时会继续等它。
		return errors.Join(ctx.Err(), p.pool.StopWithContext(ctx))
	}
}
