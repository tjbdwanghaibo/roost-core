package dataengine

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoStoreLoadCopiesVersionSchemaAndRemoteEnvelope(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	ordinary, err := bson.Marshal(bson.M{"_id": int64(7), "_version": uint64(9), "_schema": uint32(3), "name": "hero"})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{"_id": int64(7), "_version": uint64(9), "_schema": uint32(3), "name": "hero", "zone": int32(3)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Seed(bson.M{
		"_id": int64(8), "_ver": uint64(11), "_marker_epoch": uint64(4), "_lock_fence": uint64(5), "_route_epoch": uint64(6),
		"data": ordinary, "zone": int32(3),
	}); err != nil {
		t.Fatal(err)
	}
	docs, err := store.Load(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes", Filter: map[string]any{"zone": int32(3)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Key.ID != 7 || docs[0].Version != 9 || docs[0].Schema != 3 {
		t.Fatalf("ordinary=%+v", docs)
	}
	if !docs[1].Enveloped || docs[1].Version != 11 || docs[1].MarkerEpoch != 4 || docs[1].LockFence != 5 || docs[1].RouteEpoch != 6 {
		t.Fatalf("remote=%+v", docs[1])
	}
	if client.Sessions() != 0 {
		t.Fatalf("load unexpectedly started session")
	}
}

// The tombstone exclusion is a load-path invariant, so assert it by storage
// behaviour: a deleted document must not come back, and a caller filter must
// still be honoured alongside it.
func TestMongoStoreLoadExcludesTombstonesAndHonoursCallerFilter(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	for _, doc := range []bson.M{
		{"_id": int64(1), "_version": uint64(1), "zone": int32(3)},
		{"_id": int64(2), "_version": uint64(2), "zone": int32(3), "_deleted": true},
		{"_id": int64(3), "_version": uint64(1), "zone": int32(9)},
	} {
		if err := collection.Seed(doc); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := store.Load(context.Background(), coredata.LoadSpec{
		Database: testDatabase, Resource: "heroes", Filter: map[string]any{"zone": int32(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Key.ID != 1 {
		t.Fatalf("loaded=%+v, want only the live zone-3 document", docs)
	}
	// Without a caller filter the tombstone must still be excluded.
	docs, err = store.Load(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("loaded=%d documents, want 2 live ones", len(docs))
	}
	for _, doc := range docs {
		if doc.Deleted || doc.Key.ID == 2 {
			t.Fatalf("tombstone leaked into load: %+v", doc)
		}
	}
}

func TestMongoStoreStreamLoadUsesCursorAndStopsOnConsumerError(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	for id := int64(1); id <= 3; id++ {
		if err := collection.Seed(bson.M{"_id": id, "_version": uint64(1)}); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("consumer stop")
	seen := 0
	err := store.StreamLoad(context.Background(), coredata.LoadSpec{Database: testDatabase, Resource: "heroes"},
		func(coredata.RawDocument) error {
			seen++
			if seen == 2 {
				return boom
			}
			return nil
		})
	if !errors.Is(err, boom) || seen != 2 {
		t.Fatalf("err=%v seen=%d, want the consumer error to stop the stream", err, seen)
	}
	if collection.Calls["StreamFind"] == 0 {
		t.Fatal("StreamLoad did not use the streaming cursor path")
	}
}

func TestMongoStoreReadConsistentUsesOneSession(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	called := false
	if err := store.ReadConsistent(context.Background(), func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || client.Sessions() != 1 {
		t.Fatalf("called=%v sessions=%d", called, client.Sessions())
	}
}

// ISession.WithTransaction is documented as retrying automatically, so every
// ReadConsistent callback must be idempotent across invocations.
func TestMongoStoreReadConsistentRetriesCallback(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	client.TransientRetries = 1
	calls := 0
	if err := store.ReadConsistent(context.Background(), func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("callback invocations=%d, want 2 (one transient retry)", calls)
	}
}
