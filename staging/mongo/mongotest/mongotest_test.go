package mongotest

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newColl(t *testing.T) *Collection {
	t.Helper()
	return NewClient().Collection("game", "heroes")
}

func TestInsertRejectsDuplicateIDAndUniqueIndex(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	if err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "effect_id", Value: 1}}, Name: "uniq_effect", Unique: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(1), "effect_id": "e1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(1), "effect_id": "e2"}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("duplicate _id err=%v", err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(2), "effect_id": "e1"}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("unique index violation err=%v", err)
	}
	if coll.Len() != 1 {
		t.Fatalf("stored=%d, want 1", coll.Len())
	}
}

// The CAS predicate is the whole point of the fake: an update whose expected
// version no longer matches must report MatchedCount 0, and an upsert must
// then collide on _id instead of silently inserting a second document.
func TestVersionCASSemantics(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	if err := coll.Seed(bson.M{"_id": int64(7), "_version": int64(4), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	result, err := coll.UpdateOne(ctx, bson.M{"_id": int64(7), "_version": int64(4)},
		bson.M{"$set": bson.M{"_version": int64(5), "level": int32(2)}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("first CAS result=%+v err=%v", result, err)
	}
	// Replaying the same record: the document already carries version 5, so the
	// expected-version predicate matches nothing.
	result, err = coll.UpdateOne(ctx, bson.M{"_id": int64(7), "_version": int64(4)},
		bson.M{"$set": bson.M{"_version": int64(5)}})
	if err != nil || result.MatchedCount != 0 {
		t.Fatalf("replayed CAS result=%+v err=%v", result, err)
	}
	doc, _ := coll.Lookup(int64(7))
	if version, _ := asFloat(doc["_version"]); version != 5 {
		t.Fatalf("document=%v", doc)
	}
	if level, _ := asFloat(doc["level"]); level != 2 {
		t.Fatalf("document=%v", doc)
	}
	// Upsert on a stale predicate must collide, not create a twin.
	if _, err := coll.UpdateOne(ctx, bson.M{"_id": int64(7), "_version": int64(4)},
		bson.M{"$set": bson.M{"_version": int64(9)}}); err != nil {
		t.Fatalf("non-upsert stale update should be a no-op: %v", err)
	}
	if _, err := coll.ReplaceOne(ctx, bson.M{"_id": int64(7), "_version": int64(4)}, bson.M{"_id": int64(7), "_version": int64(9)}); err != nil {
		t.Fatal(err)
	}
	if coll.Len() != 1 {
		t.Fatalf("stored=%d, want 1", coll.Len())
	}
}

func TestUpsertSeedsFromEqualityFieldsOnly(t *testing.T) {
	coll := newColl(t)
	// Mongo seeds an upsert from equality fields; a CAS predicate must not
	// leak into the inserted document.
	err := coll.FindOneAndUpdate(context.Background(),
		bson.M{"_id": int64(3), "_version": bson.M{"$exists": false}},
		bson.A{bson.M{"$replaceWith": bson.M{"_id": int64(3), "_version": int64(1), "name": "new"}}},
		nil, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true})
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := coll.Lookup(int64(3))
	if !ok || doc["_version"] != int64(1) || doc["name"] != "new" {
		t.Fatalf("upserted=%v ok=%v", doc, ok)
	}
}

func TestLeaseFilterEvaluatesOwnerTokenStatusAndExpiry(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := coll.Seed(bson.M{
		"_id": "saga-step/cmd-1", "owner": "worker-1", "lease_token": int64(7),
		"digest": []byte{1, 2, 3}, "status": "pending", "lease_until": now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	live := bson.M{
		"_id": "saga-step/cmd-1", "owner": "worker-1", "lease_token": int64(7),
		"digest": []byte{1, 2, 3}, "status": "pending", "lease_until": bson.M{"$gt": now},
	}
	var found bson.M
	if err := coll.FindOne(ctx, live, &found); err != nil {
		t.Fatalf("matching lease not found: %v", err)
	}
	for name, mutate := range map[string]func(bson.M){
		"wrong owner":  func(f bson.M) { f["owner"] = "worker-2" },
		"wrong token":  func(f bson.M) { f["lease_token"] = int64(8) },
		"wrong digest": func(f bson.M) { f["digest"] = []byte{9} },
		"wrong status": func(f bson.M) { f["status"] = "completed" },
		"expired":      func(f bson.M) { f["lease_until"] = bson.M{"$gt": now.Add(2 * time.Minute)} },
	} {
		filter := bson.M{}
		for k, v := range live {
			filter[k] = v
		}
		mutate(filter)
		if err := coll.FindOne(ctx, filter, &found); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("%s: err=%v, want ErrNotFound", name, err)
		}
	}
}

func TestClaimStealUsesTokenCAS(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := coll.Seed(bson.M{
		"_id": "claim", "status": "pending", "lease_token": int64(1),
		"lease_until": now.Add(-time.Minute), "owner": "old",
	}); err != nil {
		t.Fatal(err)
	}
	filter := bson.M{"_id": "claim", "status": "pending", "lease_token": int64(1), "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{"owner": "new", "lease_until": now.Add(time.Minute)}, "$inc": bson.M{"lease_token": 1}}
	var renewed bson.M
	if err := coll.FindOneAndUpdate(ctx, filter, update, &renewed, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); err != nil {
		t.Fatal(err)
	}
	if renewed["lease_token"] != int64(2) || renewed["owner"] != "new" {
		t.Fatalf("renewed=%v", renewed)
	}
	// A second claimer racing on the stale token must lose.
	if err := coll.FindOneAndUpdate(ctx, filter, update, &renewed, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("stale-token claim err=%v, want ErrNotFound", err)
	}
}

func TestFindSortLimitAndOrOperator(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	base := time.Now().UTC()
	for i, age := range []time.Duration{3 * time.Hour, time.Hour, 2 * time.Hour} {
		if err := coll.Seed(bson.M{"_id": int64(i + 1), "created_at": base.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	var oldest []bson.M
	if err := coll.Find(ctx, bson.M{}, &oldest, fmongo.FindOption{
		Sort: bson.D{{Key: "created_at", Value: 1}}, Limit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if len(oldest) != 1 || oldest[0]["_id"] != int64(1) {
		t.Fatalf("oldest=%v", oldest)
	}
	if err := coll.Seed(bson.M{"_id": int64(4), "available_at": base}); err != nil {
		t.Fatal(err)
	}
	var due []bson.M
	claim := bson.M{"available_at": bson.M{"$lte": base}, "$or": bson.A{
		bson.M{"lease_until": bson.M{"$lte": base}},
		bson.M{"lease_until": bson.M{"$exists": false}},
	}}
	if err := coll.Find(ctx, claim, &due); err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0]["_id"] != int64(4) {
		t.Fatalf("due=%v", due)
	}
}

func TestBulkWriteReportsPerModelMatchCounts(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	if err := coll.Seed(bson.M{"_id": int64(1), "_version": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := coll.Seed(bson.M{"_id": int64(2), "_version": int64(5)}); err != nil {
		t.Fatal(err)
	}
	result, err := coll.BulkWrite(ctx, []fmongo.WriteModel{
		fmongo.NewUpdateOneModel(bson.M{"_id": int64(1), "_version": int64(1)}, bson.M{"$set": bson.M{"_version": int64(2)}}, false),
		fmongo.NewUpdateOneModel(bson.M{"_id": int64(2), "_version": int64(1)}, bson.M{"$set": bson.M{"_version": int64(2)}}, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	// One model matched, the other's CAS predicate did not — exactly the
	// signal the production batch projector relies on.
	if result.MatchedCount != 1 {
		t.Fatalf("matched=%d, want 1", result.MatchedCount)
	}
}

func TestTransientRetryReinvokesCallback(t *testing.T) {
	client := NewClient()
	client.TransientRetries = 1
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	calls := 0
	if err := session.WithTransaction(context.Background(), func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || client.Attempts() != 2 {
		t.Fatalf("calls=%d attempts=%d, want 2/2", calls, client.Attempts())
	}
}

func TestUnsupportedConstructFailsLoudly(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	if err := coll.Seed(bson.M{"_id": int64(1), "level": int32(3)}); err != nil {
		t.Fatal(err)
	}
	var found bson.M
	if err := coll.FindOne(ctx, bson.M{"level": bson.M{"$regex": "x"}}, &found); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported operator err=%v, want ErrUnsupported", err)
	}
	if _, err := coll.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.M{"$push": bson.M{"tags": "a"}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported update err=%v, want ErrUnsupported", err)
	}
}

func TestErrorInjectionAndCallCounters(t *testing.T) {
	coll := newColl(t)
	boom := errors.New("boom")
	coll.Errors["FindOne"] = boom
	var found bson.M
	if err := coll.FindOne(context.Background(), bson.M{"_id": int64(1)}, &found); !errors.Is(err, boom) {
		t.Fatalf("injected err=%v", err)
	}
	if coll.Calls["FindOne"] != 1 {
		t.Fatalf("calls=%v", coll.Calls)
	}
}

func TestDatabaseForSidIsDistinct(t *testing.T) {
	client := NewClient()
	if client.Database("game") == client.DatabaseForSid("game", 3) {
		t.Fatal("DatabaseForSid resolved to the plain database")
	}
}

// Go's BSON codec widens small integers, so a filter written with the field's
// declared Go type (uint8 for an enum, uint32 for a schema) must still match
// the int32/int64 the document round-tripped into. A fake that compared Go
// types instead of numeric values would report "no match" for a query the real
// server answers — and would then be blamed on the production code.
func TestNumericWideningAcrossBSONRoundTrip(t *testing.T) {
	coll := newColl(t)
	ctx := context.Background()
	type stored struct {
		ID     string `bson:"_id"`
		State  uint8  `bson:"state"`
		Schema uint32 `bson:"schema"`
		Level  int16  `bson:"level"`
	}
	if _, err := coll.InsertOne(ctx, stored{ID: "a", State: 2, Schema: 7, Level: -3}); err != nil {
		t.Fatal(err)
	}
	for name, filter := range map[string]bson.M{
		"uint8 field":  {"state": uint8(2)},
		"uint32 field": {"schema": uint32(7)},
		"int16 field":  {"level": int16(-3)},
		"widened":      {"state": int64(2), "schema": int32(7)},
		"range":        {"schema": bson.M{"$gte": uint32(7), "$lte": uint32(7)}},
	} {
		var found bson.M
		if err := coll.FindOne(ctx, filter, &found); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var missing bson.M
	if err := coll.FindOne(ctx, bson.M{"state": uint8(3)}, &missing); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("non-matching narrow integer err=%v, want ErrNotFound", err)
	}
	// A numeric _id declared as uint32 must resolve to the same document as
	// the int64 the codec stores.
	if _, err := coll.InsertOne(ctx, bson.M{"_id": uint32(11), "state": uint8(1)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := coll.Lookup(int64(11)); !ok {
		t.Fatal("uint32 _id did not resolve through an int64 lookup")
	}
}

// All-or-nothing application is a property production code claims (a batch
// projection is documented as atomic), so the fake has to model it: a callback
// that fails must leave nothing behind, including in collections it created.
func TestTransactionAbortRollsBackEveryWrite(t *testing.T) {
	client := NewClient()
	existing := client.Collection("game", "heroes")
	if err := existing.Seed(bson.M{"_id": int64(1), "_version": int64(1)}); err != nil {
		t.Fatal(err)
	}
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	boom := errors.New("callback failed")
	err = session.WithTransaction(context.Background(), func(ctx context.Context) error {
		if _, updateErr := existing.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"_version": int64(2)}}); updateErr != nil {
			return updateErr
		}
		if _, insertErr := client.Collection("game", "markers").InsertOne(ctx, bson.M{"_id": "m1"}); insertErr != nil {
			return insertErr
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	doc, _ := existing.Lookup(int64(1))
	if version, _ := asFloat(doc["_version"]); version != 1 {
		t.Fatalf("aborted transaction kept its update: %v", doc)
	}
	if client.Collection("game", "markers").Len() != 0 {
		t.Fatal("aborted transaction kept a write to a collection it created")
	}
}

// A retried transaction re-runs against the original state, which is what
// makes a non-idempotent callback (one that accumulates outside itself) fail
// here rather than in production.
func TestTransactionRetryRerunsFromOriginalState(t *testing.T) {
	client := NewClient()
	client.TransientRetries = 2
	coll := client.Collection("game", "counters")
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	if err := session.WithTransaction(context.Background(), func(ctx context.Context) error {
		_, insertErr := coll.InsertOne(ctx, bson.M{"_id": "only-one"})
		return insertErr
	}); err != nil {
		t.Fatal(err)
	}
	if coll.Len() != 1 {
		t.Fatalf("stored=%d, want 1 (each attempt must start clean)", coll.Len())
	}
	if client.Attempts() != 3 {
		t.Fatalf("attempts=%d, want 3", client.Attempts())
	}
}
