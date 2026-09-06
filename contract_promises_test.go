package mongotest

import (
	"context"
	"errors"
	"strings"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The fake stands in for Mongo in most kit and service unit tests, so its
// refusals ARE the contract those tests exercise. Each is pinned: a miss on
// the find-and-modify family is ErrNotFound (unless upserting), an update may
// not move a document to another _id, and an insert colliding on _id is a
// duplicate key.
func TestFakeCollectionRefusalsMatchTheDriverContract(t *testing.T) {
	ctx := context.Background()
	coll := NewClient().Database("game").Collection("heroes")
	if err := coll.Seed(bson.M{"_id": int64(1), "name": "a", "version": int64(1)}); err != nil {
		t.Fatal(err)
	}
	var out bson.M
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("FindOneAndUpdate miss without upsert = %v", err)
	}
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out, fmongo.FindOneAndUpdateOption{Upsert: true}); err != nil {
		t.Fatalf("FindOneAndUpdate miss with upsert = %v", err)
	}
	if _, found := coll.Lookup(int64(9)); !found {
		t.Fatal("upsert did not insert")
	}
	if err := coll.FindOneAndDelete(ctx, bson.M{"_id": int64(404)}, &out); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("FindOneAndDelete miss = %v", err)
	}
	if err := coll.FindOneAndReplace(ctx, bson.M{"_id": int64(404)}, bson.M{"_id": int64(404)}, &out); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("FindOneAndReplace miss = %v", err)
	}
	if err := coll.FindOne(ctx, bson.M{"_id": int64(404)}, &out); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("FindOne miss = %v", err)
	}
	_, err := coll.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"_id": int64(2)}})
	if err == nil || !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "update changed _id") {
		t.Fatalf("update that moves _id = %v", err)
	}
	if doc, found := coll.Lookup(int64(1)); !found || doc["name"] != "a" {
		t.Fatalf("a refused update changed the document: %v found=%v", doc, found)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(1), "name": "dup"}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("insert on an existing _id = %v", err)
	}
	if coll.Len() != 2 {
		t.Fatalf("documents = %d, want 2 (seed + upsert)", coll.Len())
	}
}
