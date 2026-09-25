package nest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/worker"
)

// Phase 2 of the pipelined commit (see NEST_PIPELINED_COMMIT.md §10): instead
// of parking the dispatch worker on the commit ticket, the transaction's
// completion (AfterCommit hooks, release notification, reply) is handed to a
// completion pump and the worker moves on to its next message.
//
// Ordering contract: completions are submitted while the entity locks are
// still held, the pump forwards them in FIFO order, and execution is hashed
// by primary entity onto a worker pool — so completions of the same entity
// run in commit (LSN) order, exactly as they did when the dispatch worker ran
// them inline. The pump itself only waits on tickets and forwards; business
// hooks run in the pool and can never head-of-line block resolution of other
// entities' completions.

const (
	defaultCompletionWorkers  = 4
	defaultCompletionQueueCap = 8192
)

type pipelinedCompletion struct {
	ticket      CommitTicket
	entityID    int64
	complete    func(error)
	handler     string
	stages      bool
	submittedAt time.Time
}

type completionTask struct {
	run func()
}

func (t *completionTask) OnRelease() {}

type completionPump struct {
	queue chan pipelinedCompletion
	pool  *worker.Pool[*completionTask]
	done  chan struct{}
	fence func(error)
	// closeMu pairs the closed check with the queue send in submit, so a
	// send can never race stop's close of the queue.
	closeMu sync.RWMutex
	closed  atomic.Bool
	once    sync.Once

	// chains carries the per-entity ordering links. The tail node for an
	// entity is installed while the entity lock is held, so the chain order
	// equals commit (LSN) order, and every execution path — pool worker,
	// inline in the pump, and the dispatch-worker fallback — waits for its
	// predecessor before running. That keeps same-entity completion order
	// unconditional, including under overload.
	chainMu sync.Mutex
	chains  map[int64]chan struct{}
}

// completionOrder is one entity's link in the completion chain.
type completionOrder struct {
	pump        *completionPump
	entityID    int64
	prev        chan struct{}
	mine        chan struct{}
	releaseOnce sync.Once
}

// link reserves this transaction's place in its entity's completion order.
// Called while the entity lock is held.
func (p *completionPump) link(entityID int64) *completionOrder {
	order := &completionOrder{pump: p, entityID: entityID, mine: make(chan struct{})}
	p.chainMu.Lock()
	order.prev = p.chains[entityID]
	p.chains[entityID] = order.mine
	p.chainMu.Unlock()
	return order
}

// await blocks until the previous completion of the same entity has finished.
// It can never deadlock: predecessors only wait on strictly earlier links and
// on their own tickets, which the WAL resolves independently.
func (o *completionOrder) await() {
	if o == nil || o.prev == nil {
		return
	}
	<-o.prev
}

// release publishes this completion as finished and drops the entity's chain
// entry when no successor has been linked behind it.
//
// Idempotent on purpose: the link is taken before the pump decides whether it
// can own the work, so on the degraded path the caller holds a release
// obligation it must be able to discharge defensively without risking a double
// close. An unreleased link would block every later completion for that entity
// forever, with no timeout and no metric to show it.
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

func (p *completionPump) start() {
	p.pool.Start()
	go p.run()
}

func (p *completionPump) run() {
	defer close(p.done)
	for entry := range p.queue {
		// Tickets resolve in LSN order (prefix durability), so a FIFO wait
		// adds at most one group-commit batch of latency for entries pushed
		// slightly out of order across entities.
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
			// Pool rejected (saturated or stopping): run inline rather than
			// drop — a completion is a durability promise that must be
			// delivered. complete waits for its entity predecessor, so
			// ordering holds here too; the cost is pump head-of-line delay
			// while the pool is saturated.
			//
			// Inside the same recovery boundary the worker gives it: this
			// goroutine is the pump itself, and a panic that escaped here
			// took the process down (RR-20260911-06, 09-12 实测).
			goroutine.SafeFunc(task.run)
		}
	}
}

// submit hands one completion to the pump. It is called with entity locks
// held and therefore never blocks: a full queue returns false and the caller
// falls back to the Phase 1 in-worker wait.
func (p *completionPump) submit(entry pipelinedCompletion) bool {
	if p == nil {
		return false
	}
	// The read lock makes the closed check and the send one atomic step
	// against stop, which closes the queue only after taking the write lock.
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
	// Linked while the entity lock is still held: chain order == commit order.
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
			// Same verdict as the in-worker path: indeterminate never rolls
			// back — successors may already build on this state and WAL
			// replay owns the final history.
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
	// The caller replies through complete(), so the dispatch path must not
	// also send RetChan.
	msg.deferredCompletion = true
	return handoff
}

// completionEntityID picks the chain/pool key: the first non-nil (primary)
// entity. Ordering is therefore guaranteed per PRIMARY entity — two
// multi-entity transactions that share only a secondary entity are not
// ordered against each other, matching how dispatch itself hashes work.
func completionEntityID(es []entity.IThreadSafeEntity, msg *Msg) int64 {
	for _, e := range es {
		if e != nil {
			return e.GUId()
		}
	}
	return msg.Key()
}

// stop drains every accepted completion and stops the pool. Called after the
// dispatcher has drained, so no new submissions can race the close.
func (p *completionPump) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var err error
	p.once.Do(func() {
		// Wait out every in-flight submit before closing the queue.
		p.closeMu.Lock()
		p.closed.Store(true)
		p.closeMu.Unlock()
		close(p.queue)
		select {
		case <-p.done:
			err = p.pool.StopWithContext(ctx)
		case <-ctx.Done():
			// Still initiate the pool stop: the pump keeps draining in the
			// background and a rejected Dispatch falls back to inline
			// delivery, so stopping here bounds the leak instead of leaving
			// the pool goroutines alive forever after a timed-out shutdown.
			err = errors.Join(ctx.Err(), p.pool.StopWithContext(ctx))
		}
	})
	return err
}
