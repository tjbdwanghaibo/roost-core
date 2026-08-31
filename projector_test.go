package dataengine

import (
	"context"
	"errors"
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
