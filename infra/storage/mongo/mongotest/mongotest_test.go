package mongotest

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
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

// The fake stands in for Mongo in most kit and service unit tests, so its
// refusals ARE the contract those tests exercise. Each is pinned: a miss on
// the find-and-modify family is ErrNotFound (unless upserting), an update may
// not move a document to another _id, and an insert colliding on _id is a
// duplicate key.
func TestFakeCollectionRefusalsMatchTheDriverContract(t *testing.T) {
	ctx := context.Background()
	coll := NewClient().Database("game").Collection("heroes").(*Collection)
	if err := coll.Seed(bson.M{"_id": int64(1), "name": "a", "version": int64(1)}); err != nil {
		t.Fatal(err)
	}
	var out bson.M
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("FindOneAndUpdate miss without upsert = %v", err)
	}
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true}); err != nil {
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

// 替身的拒绝就是被替身的测试所依赖的契约：找不到文档报 ErrNotFound（包括 upsert 时要求 before 镜像而没有的情形），插入 / upsert / 替换
// 撞上已有 _id 报 ErrDuplicateKey 或拒绝改 _id，nil 文档与无 _id 的文档被拒，过滤器里不认识的形状要大声失败而不是悄悄不匹配。

func newGuardCollection(t *testing.T) *Collection {
	t.Helper()
	coll := NewClient().Database("game").Collection("guards").(*Collection)
	for _, doc := range []bson.M{
		{"_id": int64(1), "name": "a", "score": int64(10)},
		{"_id": int64(2), "name": "b", "score": int64(20)},
	} {
		if err := coll.Seed(doc); err != nil {
			t.Fatal(err)
		}
	}
	return coll
}

func TestFindAndModifyMissesAreNotFoundEvenWhenAskingForTheAfterImage(t *testing.T) {
	ctx := context.Background()
	coll := newGuardCollection(t)
	var out bson.M
	// 无匹配、不 upsert、要求 after 镜像：仍是 ErrNotFound（不能"成功"返回空）。
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("miss + ReturnAfter = %v, want ErrNotFound", err)
	}
	if coll.Len() != 2 {
		t.Fatalf("a refused find-and-update inserted: len=%d", coll.Len())
	}
	// upsert 但要求 before 镜像：文档被插入，调用仍报 ErrNotFound（真驱动的 ErrNoDocuments）。
	if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(9)}, bson.M{"$set": bson.M{"name": "x"}}, &out, fmongo.FindOneAndUpdateOption{Upsert: true}); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("upsert + before image = %v, want ErrNotFound", err)
	}
	if _, found := coll.Lookup(int64(9)); !found {
		t.Fatal("upsert asking for the before image did not insert")
	}
}

func TestWritesRefuseIDCollisionsAndIDChanges(t *testing.T) {
	ctx := context.Background()
	coll := newGuardCollection(t)
	// upsert 的种子 + $set 把 _id 改成已有的：重复键，不覆盖（FindOneAndUpdate 的 Upsert 走 updateLocked）。
	var out bson.M
	if err := coll.FindOneAndUpdate(ctx, bson.M{"name": "zzz"}, bson.M{"$set": bson.M{"_id": int64(1), "name": "hijack"}}, &out, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("upsert landing on an existing _id = %v, want ErrDuplicateKey", err)
	}
	if doc, _ := coll.Lookup(int64(1)); doc["name"] != "a" {
		t.Fatalf("refused upsert overwrote document 1: %v", doc)
	}
	// 替换 upsert 撞上已有 _id（只有 BulkWrite 的 ReplaceOne 模型带 Upsert）：重复键。
	if _, err := coll.BulkWrite(ctx, []fmongo.WriteModel{{Type: fmongo.WriteModelReplaceOne, Filter: bson.M{"name": "zzz"}, Document: bson.M{"_id": int64(2), "name": "hijack"}, Upsert: true}}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("replace-upsert landing on an existing _id = %v, want ErrDuplicateKey", err)
	}
	if doc, _ := coll.Lookup(int64(2)); doc["name"] != "b" {
		t.Fatalf("refused replace-upsert overwrote document 2: %v", doc)
	}
	// 替换匹配到的文档但换了 _id：拒绝。
	if _, err := coll.ReplaceOne(ctx, bson.M{"_id": int64(1)}, bson.M{"_id": int64(3), "name": "moved"}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "replacement changed _id") {
		t.Fatalf("replacement moving _id = %v", err)
	}
	if _, found := coll.Lookup(int64(3)); found {
		t.Fatal("a refused replacement created a document under the new _id")
	}
	// nil 文档、无 _id 的文档。
	if _, err := coll.InsertOne(ctx, nil); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "nil document") {
		t.Fatalf("insert nil = %v", err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"name": "anonymous"}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "without _id") {
		t.Fatalf("insert without _id = %v", err)
	}
	if coll.Len() != 2 {
		t.Fatalf("refused writes changed the collection: len=%d", coll.Len())
	}
}

func TestFiltersFailLoudlyOnShapesTheFakeDoesNotModel(t *testing.T) {
	ctx := context.Background()
	coll := newGuardCollection(t)
	var docs []bson.M

	// $and：一个分支为假 → 整体不匹配且无错误；一个分支出错 → 错误上抛。
	if err := coll.Find(ctx, bson.M{"$and": bson.A{bson.M{"name": "a"}, bson.M{"score": int64(999)}}}, &docs); err != nil || len(docs) != 0 {
		t.Fatalf("$and with a false branch: err=%v docs=%d", err, len(docs))
	}
	err := coll.Find(ctx, bson.M{"$and": bson.A{bson.M{"name": "a"}, bson.M{"score": bson.M{"$exists": "yes"}}}}, &docs)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "$exists operand") {
		t.Fatalf("$and with an erroring branch = %v", err)
	}
	// $exists 非布尔操作数。
	if err := coll.Find(ctx, bson.M{"score": bson.M{"$exists": 1}}, &docs); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "$exists operand") {
		t.Fatalf("$exists with a non-bool operand = %v", err)
	}
	// $eq 不等：不匹配。
	if err := coll.Find(ctx, bson.M{"score": bson.M{"$eq": int64(11)}}, &docs); err != nil || len(docs) != 0 {
		t.Fatalf("$eq mismatch: err=%v docs=%d", err, len(docs))
	}
	// 数字与字符串比大小：拒绝而不是按字符串排。
	if err := coll.Find(ctx, bson.M{"score": bson.M{"$gt": "5"}}, &docs); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "compare") {
		t.Fatalf("numeric $gt against a string = %v", err)
	}
	if err := coll.Seed(bson.M{"_id": int64(3), "name": "c", "score": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := coll.Find(ctx, bson.M{}, &docs, fmongo.FindOption{Sort: bson.D{{Key: "score", Value: 1}}}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "compare") {
		t.Fatalf("sort across number and string = %v", err)
	}
	// 聚合管道只认 $replaceWith。
	if _, err := coll.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.A{bson.M{"$set": bson.M{"name": "p"}}}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "aggregation stage") {
		t.Fatalf("pipeline update with a non-$replaceWith stage = %v", err)
	}
}

// $in 的操作数是具名切片（如 saga 的 []Status）时按元素的底层类型比较，与驱动一致（驱动只把元素恰好是 byte 的切片编成二进制，
// []namedStatus 是数组）。历史：RR-20261006-08。
type namedStatus uint8

func (s namedStatus) String() string { return [...]string{"pending", "waiting", "done", "closed"}[s] }

type namedLabel string

func TestInAcceptsNamedSlices(t *testing.T) {
	ctx := context.Background()
	c := NewClient().Collection("in_named", "records")
	for id, doc := range []bson.M{
		{"status": int32(0), "label": "a", "n": int64(10)},
		{"status": int32(1), "label": "b", "n": int64(20)},
		{"status": int32(2), "label": "c", "n": int64(30)},
	} {
		doc["_id"] = int64(id + 1)
		if err := c.Seed(doc); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		filter bson.M
		want   []int64
	}{
		"named_int_slice":    {bson.M{"status": bson.M{"$in": []namedStatus{0, 2}}}, []int64{1, 3}},
		"named_string_slice": {bson.M{"label": bson.M{"$in": []namedLabel{"b"}}}, []int64{2}},
		"plain_int64_slice":  {bson.M{"n": bson.M{"$in": []int64{20, 30}}}, []int64{2, 3}},
		"array":              {bson.M{"status": bson.M{"$in": [2]namedStatus{1, 1}}}, []int64{2}},
		"id_named_slice":     {bson.M{"_id": bson.M{"$in": []namedStatus{1, 3}}}, []int64{1, 3}},
	} {
		var rows []bson.M
		if err := c.Find(ctx, tc.filter, &rows); err != nil {
			t.Fatalf("%s: Find: %v", name, err)
		}
		got := make([]int64, 0, len(rows))
		for _, row := range rows {
			got = append(got, row["_id"].(int64))
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if len(got) != len(tc.want) {
			t.Fatalf("%s: matched %v, want %v", name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: matched %v, want %v", name, got, tc.want)
			}
		}
	}
	// []byte（含以它为底层的具名类型）是二进制标量，不是数组：真实服务端对非数组的 $in 报错，替身同样拒绝。
	type blob []byte
	for _, operand := range []any{[]byte("a"), blob("a"), [1]byte{'a'}} {
		var rows []bson.M
		if err := c.Find(ctx, bson.M{"label": bson.M{"$in": operand}}, &rows); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("$in %T: err=%v rows=%v, want ErrUnsupported", operand, err, rows)
		}
	}
}

// 页边界、排序后分页与 int64 大值在 Find / Stream 两入口一致。历史：RR-20261004-NC-20。

func TestPaginationBoundaryPromises(t *testing.T) {
	cases := []struct {
		name        string
		skip, limit int64
		want        []int64
	}{
		{"all", 0, 0, []int64{1, 2, 3}},
		{"middle", 1, 1, []int64{2}},
		{"last", 2, 2, []int64{3}},
		{"outside", 3, 1, nil},
		{"huge_skip", math.MaxInt64, 1, nil},
		{"huge_limit", 1, math.MaxInt64, []int64{2, 3}},
	}
	for _, method := range []string{"find", "stream"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				coll := NewClient().Collection("pagination", "boundary")
				for _, id := range []int64{3, 1, 2} {
					if err := coll.Seed(bson.M{"_id": id}); err != nil {
						t.Fatal(err)
					}
				}
				option := fmongo.FindOption{Sort: bson.D{{Key: "_id", Value: 1}}, Skip: tc.skip, Limit: tc.limit}
				var got []int64
				if method == "find" {
					var rows []bson.M
					if err := coll.Find(context.Background(), bson.M{}, &rows, option); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						got = append(got, row["_id"].(int64))
					}
				} else {
					if err := coll.StreamFind(context.Background(), bson.M{}, func(raw []byte) error {
						var row bson.M
						if err := bson.Unmarshal(raw, &row); err != nil {
							return err
						}
						got = append(got, row["_id"].(int64))
						return nil
					}, option); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("%s skip=%d limit=%d got=%v want=%v", method, tc.skip, tc.limit, got, tc.want)
				}
			})
		}
	}
}
