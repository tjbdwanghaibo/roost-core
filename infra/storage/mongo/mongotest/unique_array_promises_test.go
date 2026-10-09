// RR-20261005-NC-102：唯一索引键落在数组上（multikey）时，替身不能静默给出与真实 Mongo 不同的
// 判定。真实 Mongo 按数组的每个元素建键：{tags:[1,2]} 与 {tags:[2,3]} 撞键，{tags:[1,2]} 与
// {tags:2} 撞键；路径穿过文档数组时取各元素的子字段，{m:[{n:1}]} 与 {m:[{n:2}]} 不撞键。
// 旧替身把整个数组当一个值比较、穿过数组的路径当缺字段（null），前两例放过真实 Mongo 会拒绝的
// 重复，第三例又误报重复（三例均在隔离副本集 8.0.28 上实测，见 docs/bug/RR-20261005-NC-102.md）。
// 替身不模拟 multikey，所以明确拒绝：索引路径上出现数组时返回 ErrUnsupported，标量照旧比较。
package mongotest

import (
	"context"
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestUniqueIndexOverAnArrayIsRefusedNotGuessed(t *testing.T) {
	ctx := context.Background()
	unique := func(t *testing.T, field string) fmongo.ICollection {
		t.Helper()
		coll := NewClient().Database("d").Collection("c")
		if err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "u", Keys: bson.D{{Key: field, Value: 1}}, Unique: true}}); err != nil {
			t.Fatal(err)
		}
		return coll
	}
	// 实测分歧的三例：每例都必须在碰到数组的那次写入上明确拒绝，而不是给出某个判定。
	for _, c := range []struct {
		name  string
		field string
		docs  []bson.M
	}{
		{"array_shared_element", "tags", []bson.M{{"_id": 1, "tags": bson.A{1, 2}}, {"_id": 2, "tags": bson.A{2, 3}}}},
		{"array_vs_scalar", "tags", []bson.M{{"_id": 1, "tags": 2}, {"_id": 2, "tags": bson.A{1, 2}}}},
		{"array_of_docs_path", "m.n", []bson.M{{"_id": 1, "m": bson.A{bson.M{"n": 1}}}, {"_id": 2, "m": bson.A{bson.M{"n": 2}}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			coll := unique(t, c.field)
			var last error
			for _, doc := range c.docs {
				if _, last = coll.InsertOne(ctx, doc); last != nil {
					break
				}
			}
			if !errors.Is(last, ErrUnsupported) {
				t.Fatalf("writes %v under unique %q ended with %v; want ErrUnsupported (real Mongo indexes each array element)", c.docs, c.field, last)
			}
		})
	}
	t.Run("index_build_over_stored_array", func(t *testing.T) {
		coll := NewClient().Database("d").Collection("c")
		if _, err := coll.InsertOne(ctx, bson.M{"_id": 1, "tags": bson.A{1, 2}}); err != nil {
			t.Fatal(err)
		}
		err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "u", Keys: bson.D{{Key: "tags", Value: 1}}, Unique: true}})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unique index build over a stored array = %v, want ErrUnsupported", err)
		}
	})
	t.Run("update_into_array", func(t *testing.T) {
		coll := unique(t, "tags")
		if _, err := coll.InsertOne(ctx, bson.M{"_id": 1, "tags": 1}); err != nil {
			t.Fatal(err)
		}
		_, err := coll.UpdateOne(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"tags": bson.A{1, 2}}})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("update moving a unique key onto an array = %v, want ErrUnsupported", err)
		}
	})
	// 控制：标量唯一键、未进索引的数组字段、compound 索引里的标量照旧。
	t.Run("controls", func(t *testing.T) {
		coll := unique(t, "name")
		if _, err := coll.InsertOne(ctx, bson.M{"_id": 1, "name": "a", "tags": bson.A{1, 2}}); err != nil {
			t.Fatalf("array outside the unique index refused: %v", err)
		}
		if _, err := coll.InsertOne(ctx, bson.M{"_id": 2, "name": "a", "tags": bson.A{1, 2}}); !errors.Is(err, fmongo.ErrDuplicateKey) {
			t.Fatalf("scalar duplicate = %v, want ErrDuplicateKey", err)
		}
		if _, err := coll.InsertOne(ctx, bson.M{"_id": 3, "name": "b"}); err != nil {
			t.Fatal(err)
		}
		compound := NewClient().Database("d").Collection("c")
		if err := compound.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "u", Keys: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}}, Unique: true}}); err != nil {
			t.Fatal(err)
		}
		if _, err := compound.InsertOne(ctx, bson.M{"_id": 1, "a": 1, "b": bson.A{1}}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("compound unique key with an array member = %v, want ErrUnsupported", err)
		}
	})
}
