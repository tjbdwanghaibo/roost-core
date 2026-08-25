package nest

import (
	"context"
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
	queue  chan pipelinedCompletion
	pool   *worker.Pool[*completionTask]
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once
}

func newCompletionPump(workers, queueCap int) *completionPump {
	if workers <= 0 {
		workers = defaultCompletionWorkers
	}
	if queueCap <= 0 {
		queueCap = defaultCompletionQueueCap
	}
	p := &completionPump{
		queue: make(chan pipelinedCompletion, queueCap),
		done:  make(chan struct{}),
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
			// Pool rejected (stopping or full): run inline rather than drop —
			// a completion is a durability promise that must be delivered.
			complete(err)
		}
	}
}

// submit hands one completion to the pump. It is called with entity locks
// held and therefore never blocks: a full queue returns false and the caller
// falls back to the Phase 1 in-worker wait.
func (p *completionPump) submit(entry pipelinedCompletion) bool {
	if p == nil || p.closed.Load() {
		return false
	}
	select {
	case p.queue <- entry:
		return true
	default:
		return false
	}
}

// deferPipelinedCompletion captures everything the post-durability work needs
// — the msg is pooled and recycled as soon as the dispatch returns, so only
// plain values (the buffered reply channel, the handler result) may escape —
// and submits it to the pump. Returns false when the pump is full or closed;
// the caller then keeps the Phase 1 in-worker wait, so backpressure degrades
// latency, never correctness.
//
// The completion runs on a pool goroutine: it has no request context, no
// guard scope, and holds no entity locks. tx ownership transfers here — the
// dispatch worker must not touch it after a successful submit.
func deferPipelinedCompletion(pump *completionPump, msg *Msg, es []entity.IThreadSafeEntity, tx *RollbackTx, ticket CommitTicket, handler string, ret any) bool {
	retChan := msg.RetChan
	waitStart := time.Now()
	submitted := pump.submit(pipelinedCompletion{
		ticket:   ticket,
		entityID: completionEntityID(es, msg),
		complete: func(ticketErr error) {
			obs.ObserveDuration("nest.pipelined.durable_wait", obs.Labels{"handler": handler}, time.Since(waitStart))
			if ticketErr != nil {
				// Same verdict as the in-worker path: indeterminate never
				// rolls back — successors may already build on this state and
				// WAL replay owns the final history.
				tx.abandon()
				obs.IncCounter("nest.pipelined.async_total", obs.Labels{"result": "indeterminate"}, 1)
				if retChan != nil {
					retChan <- ticketErr
				} else {
					slog.Error("nest pipelined async completion failed", "handler", handler, "err", ticketErr)
				}
				return
			}
			tx.Commit()
			obs.IncCounter("nest.pipelined.async_total", obs.Labels{"result": "ok"}, 1)
			if retChan != nil {
				retChan <- ret
			}
		},
	})
	if submitted {
		msg.deferredCompletion = true
	}
	return submitted
}

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
		p.closed.Store(true)
		close(p.queue)
		select {
		case <-p.done:
		case <-ctx.Done():
			err = ctx.Err()
			return
		}
		err = p.pool.StopWithContext(ctx)
	})
	return err
}
