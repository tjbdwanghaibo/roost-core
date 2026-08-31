package dataengine

import (
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
)

type successfulOutboxPublisher struct{ ids []string }

func (publisher *successfulOutboxPublisher) Publish(_ context.Context, item OutboxItem) error {
	publisher.ids = append(publisher.ids, item.Effect.ID)
	return nil
}

func TestOutboxWorkerAcknowledgesSuccessfulPublishByEffectID(t *testing.T) {
	store := newProjectorOutboxFake()
	store.pending["effect-1"] = OutboxItem{TransactionID: "tx-1", Effect: coredata.Effect{ID: "effect-1", Topic: "hero.changed"}}
	publisher := &successfulOutboxPublisher{}
	worker, err := NewOutboxWorker(store, publisher, OutboxWorkerOptions{Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || len(publisher.ids) != 1 || publisher.ids[0] != "effect-1" {
		t.Fatalf("processed=%d ids=%v", processed, publisher.ids)
	}
	store.mu.Lock()
	pending := len(store.pending)
	store.mu.Unlock()
	if pending != 0 || worker.Stats().Published != 1 {
		t.Fatalf("pending=%d stats=%+v", pending, worker.Stats())
	}
}

func TestOutboxWorkerHardLimitFencesWithoutTreatingPublishFailureAsProjectionFailure(t *testing.T) {
	store := newProjectorOutboxFake()
	createdAt := time.Now().Add(-time.Hour)
	for _, id := range []string{"effect-1", "effect-2"} {
		store.pending[id] = OutboxItem{TransactionID: "tx-1", Effect: coredata.Effect{ID: id}, CreatedAt: createdAt}
	}
	fenced := make(chan error, 1)
	worker, err := NewOutboxWorker(store, failingOutboxPublisher{err: context.DeadlineExceeded}, OutboxWorkerOptions{
		Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond,
		MaxPending: 1, MaxOldestAge: time.Minute, OnHardLimit: func(err error) { fenced <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fenced:
		if !errors.Is(err, ErrOutboxHardLimit) {
			t.Fatalf("fence err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbox hard limit did not fence")
	}
	stats := worker.Stats()
	if stats.Pending != 2 || stats.OldestAge < time.Minute || stats.PublishFailures != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}
