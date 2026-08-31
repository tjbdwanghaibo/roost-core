package dataengine

import (
	"context"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
)

func TestMongoOutboxStoreClaimAckAndNackUseLeaseFence(t *testing.T) {
	mongoStore, _, _ := newMongoStoreTest(t)
	store, err := NewMongoOutboxStore(mongoStore)
	if err != nil {
		t.Fatal(err)
	}
	collection := store.collection().(*mongoStoreFakeCollection)
	now := time.Now().UTC()
	candidate := outboxDocument{ID: "effect-1", EffectID: "effect-1", TransactionID: "tx-1", Topic: "hero.changed", AvailableAt: now.Add(-time.Second), LeaseToken: 4, CreatedAt: now.Add(-time.Minute)}
	claimed := candidate
	claimed.LeaseOwner = "worker-1"
	claimed.LeaseToken = 5
	claimed.LeaseUntil = now.Add(time.Minute)
	collection.findDocs = []outboxDocument{candidate}
	collection.findOneAndUpdateDoc = &claimed

	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Lease != (OutboxLease{Owner: "worker-1", Token: 5}) || items[0].Effect.ID != "effect-1" {
		t.Fatalf("claimed=%+v", items)
	}
	collection.deleteCount = 1
	if err := store.Ack(context.Background(), "effect-1", items[0].Lease); err != nil {
		t.Fatal(err)
	}
	collection.updateResult = &fmongo.UpdateResult{MatchedCount: 1}
	if err := store.Nack(context.Background(), "effect-1", items[0].Lease, now.Add(time.Second), "publish failed"); err != nil {
		t.Fatal(err)
	}
	collection.count = 1
	collection.findDocs = []outboxDocument{{ID: "effect-old", CreatedAt: now.Add(-time.Hour)}}
	backlog, err := store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 1 || backlog.OldestAge != time.Hour {
		t.Fatalf("backlog=%+v", backlog)
	}
}
