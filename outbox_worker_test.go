package dataengine

import (
	"context"
	"errors"
	"sync/atomic"
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

// countingBacklogStore records how often the backlog was probed.
type countingBacklogStore struct {
	OutboxStore
	probes atomic.Int32
}

func (store *countingBacklogStore) Backlog(ctx context.Context, now time.Time) (OutboxBacklog, error) {
	store.probes.Add(1)
	return store.OutboxStore.Backlog(ctx, now)
}

// The probe costs a collection count plus an oldest-document lookup, so its
// cost grows with the very backlog it measures. Running it after every claim
// (once per worker per PollInterval) multiplied that against the same Mongo
// the projector writes to, so it is rate-limited — while a health check still
// gets a current reading on demand.
func TestOutboxWorkerRateLimitsBacklogProbe(t *testing.T) {
	inner, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now.Add(-time.Minute), 1)
	store := &countingBacklogStore{OutboxStore: inner}
	worker, err := NewOutboxWorker(store, &successfulOutboxPublisher{}, OutboxWorkerOptions{
		Owner: "worker-1", BatchSize: 8, BacklogInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	for range 5 {
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if probes := store.probes.Load(); probes != 1 {
		t.Fatalf("backlog probes=%d across 5 claim rounds, want 1", probes)
	}

	// A health check must not read a stale gauge.
	if err := worker.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probes := store.probes.Load(); probes != 2 {
		t.Fatalf("on-demand refresh did not sample: probes=%d", probes)
	}

	// Once the interval elapses the claim loop samples again.
	now = now.Add(2 * time.Hour)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probes := store.probes.Load(); probes != 3 {
		t.Fatalf("probe did not resume after the interval: probes=%d", probes)
	}
}

// The oldest-document lookup sorts on created_at; without that index the sort
// scans the whole collection, which is exactly the wrong scaling for a probe
// that measures backlog size.
func TestOutboxInfrastructureIndexesCoverClaimAndBacklogQueries(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	if err := store.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	outbox := client.Collection(testDatabase, outboxCollection)
	for _, fields := range [][]string{
		{"available_at", "lease_until"},
		{"effect_id"},
		{"created_at"},
	} {
		if !outbox.HasIndex(fields...) {
			t.Fatalf("missing outbox index on %v: %+v", fields, outbox.Indexes)
		}
	}
}
