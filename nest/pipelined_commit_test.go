package nest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
)

type fakeCommitTicket struct {
	lsn  uint64
	done chan struct{}
	err  error // written before done is closed
}

func (t *fakeCommitTicket) LSN() uint64           { return t.lsn }
func (t *fakeCommitTicket) Done() <-chan struct{} { return t.done }
func (t *fakeCommitTicket) Err() error {
	select {
	case <-t.done:
		return t.err
	default:
		return nil
	}
}

// pipelinedTestCommitter is a controllable PipelinedTransactionCommitter.
// When autoResolve is false the test resolves tickets by hand, which lets it
// observe the world between "enqueued" and "durable".
type pipelinedTestCommitter struct {
	mu          sync.Mutex
	records     []CommitRecord
	tickets     []*fakeCommitTicket
	nextLSN     uint64
	durable     atomic.Uint64
	enqueueErr  error
	autoResolve bool
	enqueued    chan struct{}
	released    TransactionID
	commits     int
}

func newPipelinedTestCommitter(autoResolve bool) *pipelinedTestCommitter {
	return &pipelinedTestCommitter{autoResolve: autoResolve, enqueued: make(chan struct{}, 16)}
}

func (c *pipelinedTestCommitter) Commit(_ context.Context, record CommitRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits++
	c.records = append(c.records, record)
	return nil
}

func (c *pipelinedTestCommitter) Enqueue(_ context.Context, record CommitRecord) (CommitTicket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enqueueErr != nil {
		return nil, c.enqueueErr
	}
	c.nextLSN++
	ticket := &fakeCommitTicket{lsn: c.nextLSN, done: make(chan struct{})}
	c.records = append(c.records, record)
	c.tickets = append(c.tickets, ticket)
	if c.autoResolve {
		c.durable.Store(ticket.lsn)
		close(ticket.done)
	}
	select {
	case c.enqueued <- struct{}{}:
	default:
	}
	return ticket, nil
}

func (c *pipelinedTestCommitter) DurableLSN() uint64 { return c.durable.Load() }

func (c *pipelinedTestCommitter) TransactionReleased(id TransactionID) {
	c.mu.Lock()
	c.released = id
	c.mu.Unlock()
}

func (c *pipelinedTestCommitter) resolveAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ticket := range c.tickets {
		select {
		case <-ticket.done:
		default:
			ticket.err = err
			if err == nil {
				c.durable.Store(ticket.lsn)
			}
			close(ticket.done)
		}
	}
}

func TestPipelinedCommitReleasesLocksBeforeDurable(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 320, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	ent := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	}
	getter.Add(ent)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_early_release"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value = 20
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return "durable", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	// The lock must become acquirable while the ticket is still unresolved:
	// that is the whole point of the pipelined mode. Only after observing the
	// released lock does the probe resolve the ticket.
	lockObserved := make(chan struct{})
	go func() {
		<-committer.enqueued
		mu := ent.GetMutex()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if mu.TryLock() {
				mu.Unlock()
				close(lockObserved)
				committer.resolveAll(nil)
				return
			}
			time.Sleep(time.Millisecond)
		}
		// Locks were never released before durability: fail the request path
		// by resolving so the test can report instead of hanging.
		committer.resolveAll(errors.New("entity lock was held across the durability wait"))
	}()

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_early_release"), id, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if ret != "durable" || dao.Value != 20 {
		t.Fatalf("ret=%v value=%d", ret, dao.Value)
	}
	select {
	case <-lockObserved:
	default:
		t.Fatal("entity lock was not observed released before ticket resolution")
	}
	if got := ent.Base().LastCommitLSN(); got != 1 {
		t.Fatalf("entity LSN=%d, want 1", got)
	}
	committer.mu.Lock()
	defer committer.mu.Unlock()
	if committer.commits != 0 {
		t.Fatalf("blocking Commit was called %d times on the pipelined path", committer.commits)
	}
	if len(committer.records) != 1 || len(committer.records[0].Mutations) != 1 {
		t.Fatalf("records=%+v", committer.records)
	}
	if committer.released.IsZero() {
		t.Fatal("TransactionReleased was not called")
	}
}

func TestPipelinedEnqueueRejectionRollsBack(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 321, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := newPipelinedTestCommitter(true)
	committer.enqueueErr = errors.New("wal buffer full")

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_reject"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value = 20
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return "not-committed", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_reject"), id, nil)
	if !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("err=%v, want %v", err, ErrCommitRejected)
	}
	if ret != nil || dao.Value != 10 || dao.Tracker.Dirty() {
		t.Fatalf("ret=%v value=%d dirty=%v, want rollback", ret, dao.Value, dao.Tracker.Dirty())
	}
}

func TestPipelinedIndeterminateAbandonsWithoutRollback(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 322, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_indeterminate"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value = 20
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return "unknown", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	go func() {
		<-committer.enqueued
		committer.resolveAll(ErrCommitIndeterminate)
	}()

	_, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_indeterminate"), id, nil)
	if !errors.Is(err, ErrCommitIndeterminate) {
		t.Fatalf("err=%v, want %v", err, ErrCommitIndeterminate)
	}
	// The in-memory state must NOT be rolled back: successors may already
	// have enqueued on top of it and WAL replay owns the final verdict.
	if dao.Value != 20 {
		t.Fatalf("value=%d, indeterminate outcome must not roll back", dao.Value)
	}
}

func TestPipelinedRequiresCapableCommitter(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 323, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := &recordingCommitter{} // no PipelinedTransactionCommitter capability

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	var handlerRan atomic.Bool
	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_incapable"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		handlerRan.Store(true)
		return nil, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})

	_, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_incapable"), id, nil)
	if !errors.Is(err, ErrPipelinedCommitterRequired) {
		t.Fatalf("err=%v, want %v", err, ErrPipelinedCommitterRequired)
	}
	if handlerRan.Load() {
		t.Fatal("handler must not run when the committer lacks pipelined capability")
	}
}

func TestPipelinedAllowlistGatesHandlers(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 324, entity.EntityCategory(1), nestLocalKind)
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: id, Value: 10},
	})
	committer := newPipelinedTestCommitter(true)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithPipelinedAllowlist("test_pipelined_allowed"),
		NestOptionWithWorkerNumAndMsgCap(1, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

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
		return "ok", nil
	}
	meta := HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}
	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_allowed"), handler, meta)
	MustRegisterHandlerWithMeta(NewHandlerName("test_pipelined_unlisted"), handler, meta)

	if ret, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_allowed"), id, nil); err != nil || ret != "ok" {
		t.Fatalf("allowlisted handler: ret=%v err=%v", ret, err)
	}
	if _, err := Nest.Request(context.Background(), NewHandlerName("test_pipelined_unlisted"), id, nil); !errors.Is(err, ErrPipelinedNotAllowed) {
		t.Fatalf("unlisted handler err=%v, want %v", err, ErrPipelinedNotAllowed)
	}
}

func TestPipelinedCascadedReadGatesBothRepliesInOrder(t *testing.T) {
	// Design doc §8.4: T1 enqueues and releases its locks before durability;
	// T2 (a multi-entity handler on another worker) reads the state T1 wrote
	// and enqueues on top of it. Neither reply may escape before its own
	// ticket resolves, LSN order matches observation order, and resolving T1
	// alone must not release T2.
	getter := newMockGetter()
	idA := mustBuildCastID(t, 325, entity.EntityCategory(1), nestLocalKind)
	entA := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(idA, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: idA, Value: 10},
	}
	idB := mustBuildCastID(t, 326, entity.EntityCategory(1), nestLocalKind)
	entB := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(idB, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: idB, Value: 0},
	}
	getter.Add(entA)
	getter.Add(entB)
	committer := newPipelinedTestCommitter(false)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerNumAndMsgCap(4, 1, 64),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	meta := HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}
	MustRegisterHandlerWithMeta(NewHandlerName("test_cascade_t1"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value = 20
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return nil, nil
	}, meta)
	// T2 locks both entities and copies A's value into B: a cross-entity read
	// of state that is not durable yet.
	MustRegisterHandlerWithMeta(NewHandlerName("test_cascade_t2"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		b := es[0].(*rollbackTestEntity)
		a := es[1].(*rollbackTestEntity)
		old := b.dao.Value
		if !RecordUndo(b.dao, 1, func() error { b.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		b.dao.Value = a.dao.Value
		if err := MarkPersist(b.dao, 1); err != nil {
			return nil, err
		}
		return b.dao.Value, nil
	}, meta)

	// Dispatch functions are called directly (they are synchronous and fully
	// exercise locking and commit): in Phase 1 a worker goroutine parks on
	// its own ticket, so routing T2 through the worker pool would deadlock
	// the test whenever both requests hash to the same worker — exactly the
	// serialization Phase 2 would remove.
	// Direct dispatch calls need the same goroutine-scoped environment that
	// NestDispatch sets up: a request context (RollbackTx travels in it) and
	// a guard scope.
	withDispatchEnv := func(name string, run func() (any, error)) (any, error) {
		c, releaseCtx := fctx.NewContext()
		c.MergeMeta(fctx.RequestMeta{Source: "nest", Handler: name})
		defer releaseCtx()
		_, releaseScope := entity.NewGuardScope("test:" + name)
		defer releaseScope()
		return run()
	}

	t1Done := make(chan error, 1)
	go func() {
		_, err := withDispatchEnv("test_cascade_t1", func() (any, error) {
			return Nest.singleDispatch("test_cascade_t1", idA, nil)
		})
		t1Done <- err
	}()
	select {
	case err := <-t1Done:
		t.Fatalf("T1 finished before enqueue: %v", err)
	case <-committer.enqueued: // T1 enqueued (LSN 1), locks released, reply gated
	}

	t2Done := make(chan any, 1)
	go func() {
		ret, err := withDispatchEnv("test_cascade_t2", func() (any, error) {
			return Nest.multiDispatch("test_cascade_t2", []int64{idB, idA}, nil)
		})
		if err != nil {
			t2Done <- err
			return
		}
		t2Done <- ret
	}()
	select {
	case ret := <-t2Done:
		t.Fatalf("T2 finished before enqueue: %v", ret)
	case <-committer.enqueued: // T2 enqueued (LSN 2): it acquired A's lock and read pre-durable state
	}

	select {
	case <-t1Done:
		t.Fatal("T1 replied before its commit was durable")
	case <-t2Done:
		t.Fatal("T2 replied before its commit was durable")
	case <-time.After(30 * time.Millisecond):
	}

	committer.mu.Lock()
	if len(committer.tickets) != 2 || committer.tickets[0].lsn >= committer.tickets[1].lsn {
		t.Fatalf("LSN order does not match observation order: %+v", committer.tickets)
	}
	t1Ticket, t2Ticket := committer.tickets[0], committer.tickets[1]
	committer.mu.Unlock()

	// Prefix durability: resolving T1 releases only T1.
	committer.durable.Store(t1Ticket.lsn)
	close(t1Ticket.done)
	if err := <-t1Done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-t2Done:
		t.Fatal("T2 replied on T1's durability")
	case <-time.After(30 * time.Millisecond):
	}

	committer.durable.Store(t2Ticket.lsn)
	close(t2Ticket.done)
	if ret := <-t2Done; ret != 20 {
		t.Fatalf("T2 result=%v, want the value written by T1", ret)
	}
	if entB.dao.Value != 20 || entB.Base().LastCommitLSN() != t2Ticket.lsn {
		t.Fatalf("cascade state: value=%d lsn=%d", entB.dao.Value, entB.Base().LastCommitLSN())
	}
}

func TestParseDurabilityPolicyPipelined(t *testing.T) {
	policy, err := ParseDurabilityPolicy("pipelined")
	if err != nil || policy != DurabilityPipelined {
		t.Fatalf("policy=%v err=%v", policy, err)
	}
	if policy.String() != "pipelined" {
		t.Fatalf("String()=%q", policy.String())
	}
}
