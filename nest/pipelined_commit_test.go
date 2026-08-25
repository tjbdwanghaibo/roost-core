package nest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
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
		e.dao.Tracker.MarkPersist(1)
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
		e.dao.Tracker.MarkPersist(1)
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
		e.dao.Tracker.MarkPersist(1)
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

func TestParseDurabilityPolicyPipelined(t *testing.T) {
	policy, err := ParseDurabilityPolicy("pipelined")
	if err != nil || policy != DurabilityPipelined {
		t.Fatalf("policy=%v err=%v", policy, err)
	}
	if policy.String() != "pipelined" {
		t.Fatalf("String()=%q", policy.String())
	}
}
