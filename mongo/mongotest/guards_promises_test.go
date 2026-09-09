package mongotest

import (
	"context"
	"errors"
	"strings"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// U-0112 · C2（空洞测试）· nightly gap map `mongo/mongotest` 13/20。
//
// 替身的拒绝就是被替身的测试所依赖的契约：找不到文档要报 ErrNotFound（包括
// upsert 时要求 before 镜像而没有的情形），插入 / upsert / 替换撞上已有 _id 要报
// ErrDuplicateKey 或拒绝改 _id，nil 文档与无 _id 的文档要被拒，过滤器里不认识
// 的形状（$exists 非布尔、聚合管道非 $replaceWith、数字与字符串比大小）要大声
// 失败而不是悄悄不匹配——否则被替身的代码在真 Mongo 上才第一次见到这些错误。

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
