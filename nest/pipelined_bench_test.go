package nest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// delayCommitter models a WAL whose fsync takes fsyncDelay. Strict commits
// block inside Commit; pipelined commits get a ticket resolved after the same
// delay. committing signals the moment the handler reached the commit path,
// which is when the benchmark starts measuring lock unavailability.
type delayCommitter struct {
	fsyncDelay time.Duration
	committing chan struct{}
	nextLSN    atomic.Uint64
	durable    atomic.Uint64
}

func (c *delayCommitter) signalCommitting() {
	select {
	case c.committing <- struct{}{}:
	default:
	}
}

func (c *delayCommitter) Commit(context.Context, CommitRecord) error {
	c.signalCommitting()
	time.Sleep(c.fsyncDelay)
	return nil
}

func (c *delayCommitter) Enqueue(context.Context, CommitRecord) (CommitTicket, error) {
	lsn := c.nextLSN.Add(1)
	ticket := &fakeCommitTicket{lsn: lsn, done: make(chan struct{})}
	c.signalCommitting()
	time.AfterFunc(c.fsyncDelay, func() {
		c.durable.Store(lsn)
		close(ticket.done)
	})
	return ticket, nil
}

func (c *delayCommitter) DurableLSN() uint64 { return c.durable.Load() }

var lockHoldBenchOnce sync.Once

// BenchmarkCommitLockHold measures how long the entity lock stays unavailable
// to other goroutines once a handler reaches the commit path, with a 5ms
// simulated fsync. Strict holds the lock across the fsync (lock-wait-ms ~= 5);
// pipelined releases it after enqueue (lock-wait-ms ~= 0). Reply latency is
// the same in both modes — the metric isolates what pipelining is for.
func BenchmarkCommitLockHold(b *testing.B) {
	const fsyncDelay = 5 * time.Millisecond
	committer := &delayCommitter{fsyncDelay: fsyncDelay, committing: make(chan struct{}, 1)}
	getter := newMockGetter()
	buildEntity := func(raw int64) (int64, *rollbackTestEntity) {
		entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
		id, err := entity.BuildEntityID(raw, nestLocalKind)
		if err != nil {
			b.Fatal(err)
		}
		ent := &rollbackTestEntity{
			EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
			dao:        &rollbackTestDao{id: id, Value: 1},
		}
		getter.Add(ent)
		return id, ent
	}
	strictID, strictEnt := buildEntity(330)
	pipelinedID, pipelinedEnt := buildEntity(331)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(2, 2, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	b.Cleanup(StopNest)

	handler := func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value++
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return nil, nil
	}
	lockHoldBenchOnce.Do(func() {
		MustRegisterHandlerWithMeta(NewHandlerName("bench_lock_hold_strict"), handler,
			HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
		MustRegisterHandlerWithMeta(NewHandlerName("bench_lock_hold_pipelined"), handler,
			HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
	})

	run := func(b *testing.B, name string, id int64, ent *rollbackTestEntity) {
		var totalWait time.Duration
		for i := 0; i < b.N; i++ {
			done := make(chan error, 1)
			go func() {
				_, err := Nest.Request(context.Background(), NewHandlerName(name), id, nil)
				done <- err
			}()
			<-committer.committing
			start := time.Now()
			mu := ent.GetMutex()
			for !mu.TryLock() {
				time.Sleep(20 * time.Microsecond)
			}
			totalWait += time.Since(start)
			mu.Unlock()
			if err := <-done; err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(totalWait.Microseconds())/float64(b.N)/1000, "lock-wait-ms")
	}
	b.Run("strict", func(b *testing.B) { run(b, "bench_lock_hold_strict", strictID, strictEnt) })
	b.Run("pipelined", func(b *testing.B) { run(b, "bench_lock_hold_pipelined", pipelinedID, pipelinedEnt) })
}
