package nestwal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

func waitTicket(t *testing.T, ticket corenest.CommitTicket) error {
	t.Helper()
	select {
	case <-ticket.Done():
		return ticket.Err()
	case <-time.After(2 * time.Second):
		t.Fatal("commit ticket did not resolve")
		return nil
	}
}

func TestWALEnqueueTicketResolvesDurableAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	tickets := make([]corenest.CommitTicket, 0, 3)
	for i := byte(1); i <= 3; i++ {
		record := testRecord(i, corenest.DurabilityPipelined)
		ticket, err := w.Enqueue(context.Background(), record)
		if err != nil {
			t.Fatal(err)
		}
		if ticket.LSN() != uint64(i) {
			t.Fatalf("lsn=%d, want %d", ticket.LSN(), i)
		}
		tickets = append(tickets, ticket)
	}
	for _, ticket := range tickets {
		if err := waitTicket(t, ticket); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.DurableLSN(); got != 3 {
		t.Fatalf("durable watermark=%d, want 3", got)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The tickets promised durability: a reopened WAL must replay every
	// enqueued record in LSN order.
	w, err = Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	var replayed []byte
	if err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		replayed = append(replayed, record.ID[15])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(replayed) != string([]byte{1, 2, 3}) {
		t.Fatalf("replay=%v", replayed)
	}
}

func TestWALEnqueueRejectsSynchronously(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.MaxDiskBytes = 1
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	// Capacity is checked inside Enqueue: the caller still holds entity locks
	// and can roll back, so rejection must never be deferred to the writer.
	if _, err := w.Enqueue(context.Background(), testRecord(1, corenest.DurabilityPipelined)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("err=%v, want ErrCapacity", err)
	}
	// A failed admission must release its reservation.
	if got := w.reservedBytes.Load(); got != 0 {
		t.Fatalf("reservation leaked: %d bytes", got)
	}

	oversized := testRecord(2, corenest.DurabilityPipelined)
	oversized.Mutations[0].Data = make([]byte, opts.MaxRecordBytes+1)
	if _, err := w.Enqueue(context.Background(), oversized); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("err=%v, want ErrRecordTooLarge", err)
	}
}

func TestWALTerminalFailsPendingAndLateTickets(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	// A pending ticket must observe the terminal verdict instead of blocking
	// forever; the write outcome of its record is unknown once the WAL is
	// fenced, so indeterminate is the only honest answer.
	pending := &walTicket{lsn: 1000, done: make(chan struct{})}
	w.ticketMu.Lock()
	w.tickets = append(w.tickets, pending)
	w.ticketMu.Unlock()

	cause := errors.Join(corenest.ErrCommitIndeterminate, errors.New("physical write outcome unknown"))
	w.setTerminal(cause)
	if err := waitTicket(t, pending); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("pending ticket err=%v", err)
	}

	// A ticket registered concurrently with the terminal transition may miss
	// the sweep; its record reaches processBatch, whose terminal branch must
	// fail it too.
	late := &walTicket{lsn: 1001, done: make(chan struct{})}
	w.ticketMu.Lock()
	w.tickets = append(w.tickets, late)
	w.ticketMu.Unlock()
	w.processBatch([]appendRequest{{
		record: testRecord(7, corenest.DurabilityPipelined),
		frame:  encodeFrame([]byte{1}),
		done:   make(chan appendResult, 1),
	}})
	if err := waitTicket(t, late); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("late ticket err=%v", err)
	}

	// After the fence every new Enqueue is a synchronous rejection.
	if _, err := w.Enqueue(context.Background(), testRecord(8, corenest.DurabilityPipelined)); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("post-terminal enqueue err=%v", err)
	}
}

func TestCommitterEnqueueHoldsReplayUntilReleased(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var applies atomic.Int32
	applier := MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error {
		applies.Add(1)
		return nil
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error { return nil })
	opts := DefaultCommitterOptions()
	opts.RetryMin = time.Millisecond
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())

	record := testRecord(11, corenest.DurabilityPipelined)
	ticket, err := committer.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTicket(t, ticket); err != nil {
		t.Fatal(err)
	}
	if got := committer.DurableLSN(); got != ticket.LSN() {
		t.Fatalf("committer watermark=%d, want %d", got, ticket.LSN())
	}
	// Durable but not yet released: the replay loop must not apply mutations
	// while core Nest still considers the transaction in flight.
	time.Sleep(20 * time.Millisecond)
	if applies.Load() != 0 {
		t.Fatalf("mutation applied %d times before release", applies.Load())
	}
	committer.TransactionReleased(record.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := committer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if applies.Load() != 1 {
		t.Fatalf("mutation apply calls=%d, want 1", applies.Load())
	}
}

func TestCommitterEnqueueRejectionReleasesHold(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.MaxDiskBytes = 1
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	committer, err := NewCommitter(w, MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error { return nil }), nil, DefaultCommitterOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())

	record := testRecord(12, corenest.DurabilityPipelined)
	if _, err := committer.Enqueue(context.Background(), record); !errors.Is(err, ErrCapacity) {
		t.Fatalf("err=%v, want ErrCapacity", err)
	}
	if committer.isHeld(record.ID) {
		t.Fatal("rejected transaction is still held and would stall replay")
	}
}

// Two replay passes must not both apply the same durable record. A pass is
// "read from the ack fence → apply → ack"; the WAL serializes only the read, so
// a second pass that starts after the first one's read returned but before its
// ack landed re-reads the same records from the old fence and applies them
// again. That is what made TestCommitterEnqueueHoldsReplayUntilReleased report
// "apply calls=2" on a slow runner: the run loop and an active Flush overlapped
// in exactly that window. The applier contract tolerates it (at-least-once,
// idempotent), but two passes over one region is pure waste and the window is
// a race the committer can simply close. This pins it deterministically with a
// seam that parks the first pass between its read and its ack.
func TestConcurrentReplayPassesApplyEachRecordOnce(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var applies atomic.Int32
	applier := MutationApplyFunc(func(context.Context, corenest.TransactionID, corenest.EntityMutation) error {
		applies.Add(1)
		return nil
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error { return nil })
	opts := DefaultCommitterOptions()
	// Park the background loop so the two passes under test are the only ones.
	opts.RetryMin, opts.RetryMax, opts.IdlePoll = time.Hour, time.Hour, time.Hour
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	opts.testBetweenReplayAndAck = func() {
		once.Do(func() {
			close(entered)
			<-gate
		})
	}
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(context.Background())

	record := testRecord(31, corenest.DurabilityPipelined)
	ticket, err := committer.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTicket(t, ticket); err != nil {
		t.Fatal(err)
	}
	// Release without signalling, so the parked loop stays parked.
	committer.release(record.ID)

	ctx := context.Background()
	first := make(chan error, 1)
	go func() { _, err := committer.replayPass(ctx); first <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first pass never reached the seam")
	}
	// The second pass starts while the first has applied but not yet acked.
	second := make(chan error, 1)
	go func() { _, err := committer.replayPass(ctx); second <- err }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s pass: %v", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s pass did not finish", name)
		}
	}
	if got := applies.Load(); got != 1 {
		t.Fatalf("mutation apply calls=%d, want 1: a second pass re-read records the first had applied but not yet acknowledged", got)
	}
}
