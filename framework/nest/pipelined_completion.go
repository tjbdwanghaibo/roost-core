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
}

// completionOrder 是一个实体完成链上的一节。
type completionOrder struct {
	pump        *completionPump
	entityID    int64
	prev        chan struct{}
	mine        chan struct{}
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
	// 仍持实体锁时占位：链序即提交顺序。
	order := pump.link(entityID)
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

// stop 排空全部已接受的完成并停止池。在派发器排空后调用，不会有新的提交与关闭竞争。
func (p *completionPump) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() {
		// 关闭队列之前等在途的 submit 全部结束。
		p.closeMu.Lock()
		p.closed.Store(true)
		p.closeMu.Unlock()
		close(p.queue)
		select {
		case <-p.done:
			err = p.pool.StopWithContext(ctx)
		case <-ctx.Done():
			// 仍发起池停止：泵在后台继续排空，被拒绝的 Dispatch 会降级为就地执行，所以这里停止能限制泄漏，
			// 不让池 goroutine 在超时的关闭之后永远存活。
			err = errors.Join(ctx.Err(), p.pool.StopWithContext(ctx))
		}
	})
	return err
}
