package dataengine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type projectorOutboxFake struct {
	mu         sync.Mutex
	records    map[coredata.TransactionID]coredata.CommitRecord
	pending    map[string]OutboxItem
	projectErr error
}

func newProjectorOutboxFake() *projectorOutboxFake {
	return &projectorOutboxFake{records: make(map[coredata.TransactionID]coredata.CommitRecord), pending: make(map[string]OutboxItem)}
}

func (store *projectorOutboxFake) Project(_ context.Context, record coredata.CommitRecord) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.projectErr != nil {
		return store.projectErr
	}
	if _, exists := store.records[record.ID]; exists {
		return nil
	}
	store.records[record.ID] = coredata.CloneCommitRecord(record)
	for _, effect := range record.Effects {
		store.pending[effect.ID] = OutboxItem{TransactionID: record.ID.String(), Effect: coredata.CloneEffect(effect)}
	}
	return nil
}

type projectorBatchStore struct {
	projectCalls int
	batchCalls   int
	batchRecords int
}

type recordingSegmentStore struct {
	events []string
	failID coredata.TransactionID
}

func (store *recordingSegmentStore) Project(_ context.Context, record coredata.CommitRecord) error {
	store.events = append(store.events, "project:"+record.ID.String())
	if record.ID == store.failID {
		return context.DeadlineExceeded
	}
	return nil
}

func (store *recordingSegmentStore) ProjectBatch(_ context.Context, records []coredata.CommitRecord) error {
	store.events = append(store.events, fmt.Sprintf("batch:%d", len(records)))
	return nil
}

func (store *projectorBatchStore) Project(context.Context, coredata.CommitRecord) error {
	store.projectCalls++
	return nil
}

func (store *projectorBatchStore) ProjectBatch(_ context.Context, records []coredata.CommitRecord) error {
	store.batchCalls++
	store.batchRecords += len(records)
	return nil
}

func (store *projectorOutboxFake) Claim(_ context.Context, owner string, _ time.Time, limit int, _ time.Duration) ([]OutboxItem, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	items := make([]OutboxItem, 0, limit)
	for id, item := range store.pending {
		if item.Lease.Owner != "" {
			continue
		}
		item.Lease = OutboxLease{Owner: owner, Token: item.Lease.Token + 1}
		store.pending[id] = item
		items = append(items, item)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func (store *projectorOutboxFake) Ack(_ context.Context, effectID string, lease OutboxLease) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	item, ok := store.pending[effectID]
	if !ok || item.Lease != lease {
		return ErrOutboxLeaseConflict
	}
	delete(store.pending, effectID)
	return nil
}

func (store *projectorOutboxFake) Nack(_ context.Context, effectID string, lease OutboxLease, _ time.Time, lastError string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	item, ok := store.pending[effectID]
	if !ok || item.Lease != lease {
		return ErrOutboxLeaseConflict
	}
	item.Lease.Owner = ""
	item.Attempt++
	item.LastError = lastError
	store.pending[effectID] = item
	return nil
}

func (store *projectorOutboxFake) Backlog(_ context.Context, now time.Time) (OutboxBacklog, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	backlog := OutboxBacklog{Pending: int64(len(store.pending))}
	for _, item := range store.pending {
		age := now.Sub(item.CreatedAt)
		if age > backlog.OldestAge {
			backlog.OldestAge = age
		}
	}
	return backlog, nil
}

func projectorRecord(sequence byte, withEffect bool) coredata.CommitRecord {
	var id coredata.TransactionID
	id[15] = sequence
	payload, _ := bson.Marshal(bson.M{"_id": int64(sequence), "value": int32(sequence)})
	record := coredata.CommitRecord{
		ID: id, Durability: corenest.DurabilityStrict,
		Mutations: []coredata.Mutation{{
			Key:  coredata.DocumentKey{Database: "game", Resource: "heroes", ID: int64(sequence)},
			Kind: coredata.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: payload,
		}},
	}
	if withEffect {
		record.Effects = []coredata.Effect{{ID: "effect-a", Topic: "hero.changed", Payload: []byte{1}}}
	}
	return record
}

type failingOutboxPublisher struct{ err error }

func (publisher failingOutboxPublisher) Publish(context.Context, OutboxItem) error {
	return publisher.err
}

func TestProjectorAckNotBlockedByPublisherFailure(t *testing.T) {
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	opts.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: false, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())

	for _, record := range []coredata.CommitRecord{projectorRecord(1, true), projectorRecord(2, false)} {
		if err := projector.Commit(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		if got := projector.Stats().WALUnacked; got == 0 {
			t.Fatal("newly admitted transaction was not reported as unacknowledged")
		}
		projector.TransactionReleased(record.ID)
	}
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	worker, err := NewOutboxWorker(store, failingOutboxPublisher{err: errors.New("nats unavailable")}, OutboxWorkerOptions{Owner: "worker-1", BatchSize: 8, RetryMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	recordCount, pending := len(store.records), len(store.pending)
	store.mu.Unlock()
	if recordCount != 2 || pending != 1 {
		t.Fatalf("projected=%d pending effects=%d", recordCount, pending)
	}
	if stats := worker.Stats(); stats.PublishFailures != 1 {
		t.Fatalf("worker stats=%+v", stats)
	}
	replayed := 0
	if err := wal.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error {
		replayed++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if replayed != 0 {
		t.Fatalf("WAL replayed %d records after projector flush", replayed)
	}
	if got := projector.Stats().WALUnacked; got != 0 {
		t.Fatalf("wal_unacked=%d after flush", got)
	}
}

func TestProjectorFatalConflictInvokesFence(t *testing.T) {
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	store.projectErr = ErrProjectionConflict
	fenced := make(chan error, 1)
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: false, IdlePoll: time.Hour, OnFatal: func(err error) { fenced <- err }})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())
	record := projectorRecord(3, false)
	if err := projector.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(record.ID)
	if err := projector.Flush(context.Background()); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("flush err=%v", err)
	}
	select {
	case err := <-fenced:
		if !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("fence err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal projection conflict did not fence")
	}
}

func TestProjectorCommitSystemTicketCompletesAfterProjection(t *testing.T) {
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: false, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())
	ticket, err := projector.CommitSystem(context.Background(), projectorRecord(9, false))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("system projection ticket did not complete")
	}
	store.mu.Lock()
	_, projected := store.records[projectorRecord(9, false).ID]
	store.mu.Unlock()
	if !projected {
		t.Fatal("ticket completed before projection")
	}
}

func TestProjectorUsesAtomicBatchStoreForBacklog(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	w, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := byte(1); sequence <= 2; sequence++ {
		if _, err := w.Append(context.Background(), projectorRecord(sequence, false)); err != nil {
			t.Fatal(err)
		}
	}
	store := &projectorBatchStore{}
	projector, err := NewProjector(w, store, ProjectorOptions{ReplayBatchRecords: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.batchCalls != 1 || store.batchRecords != 2 || store.projectCalls != 0 {
		t.Fatalf("batch_calls=%d batch_records=%d project_calls=%d", store.batchCalls, store.batchRecords, store.projectCalls)
	}
}

func TestProjectorAcknowledgesSuccessfulPrefixBeforeLaterSegmentFailure(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close(context.Background())
	records := []coredata.CommitRecord{
		projectorRecord(1, false), projectorRecord(2, false),
		projectorRecord(3, true), projectorRecord(4, false),
	}
	store := &recordingSegmentStore{failID: records[2].ID}
	projector, err := NewProjector(wal, store, ProjectorOptions{
		ReplayBatchRecords: 16, ReplayBatchBytes: 4 << 20, CloseWAL: false, IdlePoll: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	projector.cancel()
	<-projector.done
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	for i := range records {
		if _, err := wal.Append(context.Background(), records[i]); err != nil {
			t.Fatal(err)
		}
	}

	processed, err := projector.replayPass(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || processed != 2 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	var remaining []coredata.TransactionID
	if err := wal.Replay(context.Background(), func(_ corenest.CommitFence, record coredata.CommitRecord) error {
		remaining = append(remaining, record.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []coredata.TransactionID{records[2].ID, records[3].ID}
	if !slices.Equal(remaining, want) {
		t.Fatalf("remaining=%v want=%v", remaining, want)
	}
	if !slices.Equal(store.events, []string{"batch:2", "project:" + records[2].ID.String()}) {
		t.Fatalf("events=%v", store.events)
	}
}

func stoppedProjectorWithRecords(t *testing.T, store ProjectionStore, records []coredata.CommitRecord, batchBytes int) (*Projector, *nestwal.WAL) {
	t.Helper()
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector, err := NewProjector(wal, store, ProjectorOptions{
		ReplayBatchRecords: 16, ReplayBatchBytes: batchBytes, CloseWAL: false, IdlePoll: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	projector.cancel()
	<-projector.done
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	for i := range records {
		if _, err := wal.Append(context.Background(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	return projector, wal
}

func assertWALReplayCount(t *testing.T, wal *nestwal.WAL, want int) {
	t.Helper()
	got := 0
	if err := wal.Replay(context.Background(), func(corenest.CommitFence, coredata.CommitRecord) error {
		got++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("replay records=%d want=%d", got, want)
	}
}

func TestProjectorKeepsSingleOrdinarySegmentsOnProjectFastPath(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, true), projectorRecord(3, false)}
	store := &recordingSegmentStore{}
	projector, wal := stoppedProjectorWithRecords(t, store, records, 4<<20)
	processed, err := projector.replayPass(context.Background())
	if err != nil || processed != 3 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	want := []string{
		"project:" + records[0].ID.String(), "project:" + records[1].ID.String(), "project:" + records[2].ID.String(),
	}
	if !slices.Equal(store.events, want) {
		t.Fatalf("events=%v want=%v", store.events, want)
	}
	assertWALReplayCount(t, wal, 0)
}

func TestProjectorSplitsOrdinaryBatchAtLogicalByteLimit(t *testing.T) {
	records := []coredata.CommitRecord{
		projectorRecord(1, false), projectorRecord(2, false), projectorRecord(3, false), projectorRecord(4, false),
	}
	store := &recordingSegmentStore{}
	limit := projectionRecordLogicalBytes(records[0]) + projectionRecordLogicalBytes(records[1])
	projector, wal := stoppedProjectorWithRecords(t, store, records, limit)
	processed, err := projector.replayPass(context.Background())
	if err != nil || processed != 4 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	if !slices.Equal(store.events, []string{"batch:2", "batch:2"}) {
		t.Fatalf("events=%v", store.events)
	}
	assertWALReplayCount(t, wal, 0)
}

func TestProjectorStopsAfterSegmentAckFailure(t *testing.T) {
	records := []coredata.CommitRecord{projectorRecord(1, false), projectorRecord(2, false), projectorRecord(3, true)}
	store := &recordingSegmentStore{}
	projector, wal := stoppedProjectorWithRecords(t, store, nil, 4<<20)
	tickets := make([]coredata.ProjectionTicket, 0, len(records))
	for i := range records {
		ticket, err := projector.CommitSystem(context.Background(), records[i])
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, ticket)
	}
	ackErr := errors.New("checkpoint unavailable")
	projector.ack = func(context.Context, corenest.CommitFence) error { return ackErr }
	processed, err := projector.replayPass(context.Background())
	if !errors.Is(err, ackErr) || processed != 2 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	if !slices.Equal(store.events, []string{"batch:2"}) {
		t.Fatalf("events=%v", store.events)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-tickets[i].Done():
			if err := tickets[i].Err(); err != nil {
				t.Fatalf("ticket %d err=%v", i, err)
			}
		default:
			t.Fatalf("ticket %d did not complete after Mongo success", i)
		}
	}
	select {
	case <-tickets[2].Done():
		t.Fatal("later segment ticket completed after checkpoint failure")
	default:
	}
	if stats := projector.Stats(); stats.Projected != 2 || stats.WALUnacked != 3 {
		t.Fatalf("stats after checkpoint failure=%+v", stats)
	}
	assertWALReplayCount(t, wal, 3)
}
