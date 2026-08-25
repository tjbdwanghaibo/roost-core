package nest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
	"github.com/tjbdwanghaibo/cube-core/obs"
	"github.com/tjbdwanghaibo/cube-core/worker"
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
	ticket   CommitTicket
	entityID int64
	complete func(error)
}

type completionTask struct {
	run func()
}

func (t *completionTask) OnRelease() {}

type completionPump struct {
	queue chan pipelinedCompletion
	pool  *worker.Pool[*completionTask]
	done  chan struct{}
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
	pump     *completionPump
	entityID int64
	prev     chan struct{}
	mine     chan struct{}
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
func (o *completionOrder) release() {
	if o == nil {
		return
	}
	close(o.mine)
	o.pump.chainMu.Lock()
	if o.pump.chains[o.entityID] == o.mine {
		delete(o.pump.chains, o.entityID)
	}
	o.pump.chainMu.Unlock()
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
		<-entry.ticket.Done()
		err := entry.ticket.Err()
		complete := entry.complete
		task := &completionTask{run: func() { complete(err) }}
		if p.pool.Dispatch(entry.entityID, task) != nil {
			// Pool rejected (saturated or stopping): run inline rather than
			// drop — a completion is a durability promise that must be
			// delivered. complete waits for its entity predecessor, so
			// ordering holds here too; the cost is pump head-of-line delay
			// while the pool is saturated.
			complete(err)
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

// prepareCompletion captures everything the post-durability work needs — the
// msg is pooled and recycled as soon as the dispatch returns, so only plain
// values (the buffered reply channel, the handler result) may escape —
// reserves this transaction's place in its entity's completion order, and
// tries to hand the work to the pump.
//
// Returns (deferred, runInline):
//
//	deferred == true  the pump owns the work; tx ownership transferred, and
//	                  the dispatch path must not reply or touch tx.
//	deferred == false the pump is full or closed; the caller keeps the Phase 1
//	                  in-worker wait and must call runInline once its ticket
//	                  resolves. runInline honors the same completion order, so
//	                  backpressure degrades latency — never ordering, never
//	                  correctness.
//
// A deferred completion runs on a pool goroutine: no request context, no
// guard scope, no entity locks held.
func prepareCompletion(pump *completionPump, msg *Msg, es []entity.IThreadSafeEntity, tx *RollbackTx, ticket CommitTicket, handler string, ret any) (bool, func(error)) {
	retChan := msg.RetChan
	waitStart := time.Now()
	entityID := completionEntityID(es, msg)
	// Linked while the entity lock is still held: chain order == commit order.
	order := pump.link(entityID)
	complete := func(ticketErr error) {
		order.await()
		defer order.release()
		obs.ObserveDuration("nest.pipelined.durable_wait", obs.Labels{"handler": handler}, time.Since(waitStart))
		if ticketErr != nil {
			// Same verdict as the in-worker path: indeterminate never rolls
			// back — successors may already build on this state and WAL
			// replay owns the final history.
			tx.abandon()
			obs.IncCounter("nest.pipelined.async_total", obs.Labels{"result": "indeterminate"}, 1)
			if retChan != nil {
				retChan <- ticketErr
			} else {
				slog.Error("nest pipelined completion failed", "handler", handler, "err", ticketErr)
			}
			return
		}
		tx.Commit()
		obs.IncCounter("nest.pipelined.async_total", obs.Labels{"result": "ok"}, 1)
		if retChan != nil {
			retChan <- ret
		}
	}
	if pump.submit(pipelinedCompletion{ticket: ticket, entityID: entityID, complete: complete}) {
		msg.deferredCompletion = true
		return true, nil
	}
	obs.IncCounter("nest.pipelined.async_total", obs.Labels{"result": "degraded"}, 1)
	// The caller replies through complete(), so the dispatch path must not
	// also send RetChan.
	msg.deferredCompletion = true
	return false, complete
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
