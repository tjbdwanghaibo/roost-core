package dataengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-kit/internal/mongofake"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newOutboxStoreTest(t *testing.T) (*MongoOutboxStore, *mongofake.Collection) {
	t.Helper()
	mongoStore, client, _ := newMongoStoreTest(t)
	store, err := NewMongoOutboxStore(mongoStore)
	if err != nil {
		t.Fatal(err)
	}
	return store, client.Collection(testDatabase, outboxCollection)
}

func seedOutboxEffect(t *testing.T, outbox *mongofake.Collection, id string, availableAt, createdAt time.Time, token int64) {
	t.Helper()
	if err := outbox.Seed(bson.M{
		"_id": id, "effect_id": id, "transaction_id": "tx-1", "topic": "hero.changed",
		"available_at": availableAt, "created_at": createdAt, "lease_token": token,
	}); err != nil {
		t.Fatal(err)
	}
}

// The claim path is a lease CAS: it must move the token forward and stamp the
// owner on the stored document, not merely return something.
func TestMongoOutboxStoreClaimTakesLeaseByTokenCAS(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now.Add(-time.Minute), 4)

	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Effect.ID != "effect-1" {
		t.Fatalf("claimed=%+v", items)
	}
	if items[0].Lease != (OutboxLease{Owner: "worker-1", Token: 5}) {
		t.Fatalf("lease=%+v, want owner worker-1 token 5", items[0].Lease)
	}
	// A second worker arriving while the lease is live must claim nothing.
	again, err := store.Claim(context.Background(), "worker-2", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("live lease was stolen: %+v", again)
	}
	// After expiry it becomes claimable again, with the token moving forward.
	later := now.Add(2 * time.Minute)
	stolen, err := store.Claim(context.Background(), "worker-2", later, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stolen) != 1 || stolen[0].Lease != (OutboxLease{Owner: "worker-2", Token: 6}) {
		t.Fatalf("expired-lease claim=%+v", stolen)
	}
}

func TestMongoOutboxStoreSkipsEffectsNotYetAvailable(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "later", now.Add(time.Minute), now, 1)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("claimed a delayed effect: %+v", items)
	}
}

func TestMongoOutboxStoreAckRequiresMatchingLease(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now, 4)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim items=%+v err=%v", items, err)
	}
	// A stale lease must not be able to remove another worker's claim.
	stale := OutboxLease{Owner: "worker-1", Token: items[0].Lease.Token - 1}
	if err := store.Ack(context.Background(), "effect-1", stale); !errors.Is(err, ErrOutboxLeaseConflict) {
		t.Fatalf("stale ack err=%v, want ErrOutboxLeaseConflict", err)
	}
	if err := store.Ack(context.Background(), "effect-1", items[0].Lease); err != nil {
		t.Fatal(err)
	}
	backlog, err := store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 0 {
		t.Fatalf("acked effect still pending: %+v", backlog)
	}
}

func TestMongoOutboxStoreNackReschedulesAndCountsAttempt(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now, 4)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim items=%+v err=%v", items, err)
	}
	next := now.Add(30 * time.Second)
	if err := store.Nack(context.Background(), "effect-1", items[0].Lease, next, "publish failed"); err != nil {
		t.Fatal(err)
	}
	// Nack must release the lease so the effect is claimable at `next`, and it
	// must not be claimable before then.
	early, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("nacked effect claimable before its retry time: %+v", early)
	}
	retried, err := store.Claim(context.Background(), "worker-1", next, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried) != 1 || retried[0].Attempt != 1 || retried[0].LastError != "publish failed" {
		t.Fatalf("retried=%+v, want attempt 1 with the recorded error", retried)
	}
	// A stale lease must not be able to reschedule someone else's claim.
	stale := OutboxLease{Owner: "worker-1", Token: retried[0].Lease.Token - 1}
	if err := store.Nack(context.Background(), "effect-1", stale, next, "stale"); !errors.Is(err, ErrOutboxLeaseConflict) {
		t.Fatalf("stale nack err=%v, want ErrOutboxLeaseConflict", err)
	}
}

func TestMongoOutboxStoreBacklogReportsPendingAndOldestAge(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-old", now.Add(-time.Second), now.Add(-time.Hour), 1)
	seedOutboxEffect(t, outbox, "effect-new", now.Add(-time.Second), now.Add(-time.Minute), 1)

	backlog, err := store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 2 {
		t.Fatalf("pending=%d, want 2", backlog.Pending)
	}
	// BSON dates carry millisecond precision, exactly like real Mongo, so the
	// age is asserted within a tolerance rather than to the nanosecond.
	if drift := backlog.OldestAge - time.Hour; drift < -time.Second || drift > time.Second {
		t.Fatalf("oldest age=%s, want ~1h", backlog.OldestAge)
	}
	// An empty outbox reports a zero backlog rather than a stale age.
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if err := store.Ack(context.Background(), item.Effect.ID, item.Lease); err != nil {
			t.Fatal(err)
		}
	}
	backlog, err = store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 0 || backlog.OldestAge != 0 {
		t.Fatalf("drained backlog=%+v", backlog)
	}
}
