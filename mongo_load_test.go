package dataengine

import (
	"context"
	"testing"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoStoreLoadCopiesVersionSchemaAndRemoteEnvelope(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	ordinary, err := bson.Marshal(bson.M{"_id": int64(7), "_version": uint64(9), "_schema": uint32(3), "name": "hero"})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := bson.Marshal(bson.M{
		"_id": int64(8), "_ver": uint64(11), "_marker_epoch": uint64(4), "_lock_fence": uint64(5), "_route_epoch": uint64(6),
		"data": ordinary,
	})
	if err != nil {
		t.Fatal(err)
	}
	collection.findRaw = []bson.Raw{ordinary, remote}
	docs, err := store.Load(context.Background(), coredata.LoadSpec{Database: "game", Resource: "heroes", Filter: map[string]any{"zone": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Key.ID != 7 || docs[0].Version != 9 || docs[0].Schema != 3 {
		t.Fatalf("ordinary=%+v", docs)
	}
	if !docs[1].Enveloped || docs[1].Version != 11 || docs[1].MarkerEpoch != 4 || docs[1].LockFence != 5 || docs[1].RouteEpoch != 6 {
		t.Fatalf("remote=%+v", docs[1])
	}
	if client.startSessions != 0 {
		t.Fatalf("load unexpectedly started session")
	}
	filter, ok := collection.lastFilter.(bson.M)
	if !ok || filter["$and"] == nil {
		t.Fatalf("filter does not exclude tombstones: %#v", collection.lastFilter)
	}
}

func TestMongoStoreReadConsistentUsesOneSession(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	called := false
	if err := store.ReadConsistent(context.Background(), func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || client.startSessions != 1 {
		t.Fatalf("called=%v sessions=%d", called, client.startSessions)
	}
}
