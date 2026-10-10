package mongotest

import (
	"context"
	"errors"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
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

type uniqueNullStep struct {
	doc bson.M
	dup bool
}

func TestUniqueIndexMissingFieldPromises(t *testing.T) {
	ctx := context.Background()
	single := bson.D{{Key: "name", Value: 1}}
	compound := bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}}
	dotted := bson.D{{Key: "m.n", Value: 1}}
	cases := []struct {
		name     string
		keys     bson.D
		sparse   bool
		existing []bson.M
		buildDup bool
		inserts  []uniqueNullStep
	}{
		// 探针 S1 / S4 / S2 / S3。
		{name: "build_two_missing", keys: single, existing: []bson.M{{"_id": 1}, {"_id": 2}}, buildDup: true},
		{name: "build_missing_and_null", keys: single, existing: []bson.M{{"_id": 1}, {"_id": 2, "name": nil}}, buildDup: true},
		{name: "insert_two_missing", keys: single, inserts: []uniqueNullStep{{doc: bson.M{"_id": 1}}, {doc: bson.M{"_id": 2}, dup: true}}},
		{name: "insert_missing_then_null", keys: single, inserts: []uniqueNullStep{{doc: bson.M{"_id": 1}}, {doc: bson.M{"_id": 2, "name": nil}, dup: true}}},
		// 探针 S5：sparse 跳过缺字段，但显式 null 会进索引。
		{name: "sparse_build_two_missing", keys: single, sparse: true, existing: []bson.M{{"_id": 1}, {"_id": 2}}, inserts: []uniqueNullStep{
			{doc: bson.M{"_id": 3}},
			{doc: bson.M{"_id": 4, "name": nil}},
			{doc: bson.M{"_id": 5, "name": nil}, dup: true},
		}},
		// 探针 C1：复合非 sparse，缺的字段按 null。
		{name: "compound_missing_is_null", keys: compound, inserts: []uniqueNullStep{
			{doc: bson.M{"_id": 1, "a": 1}},
			{doc: bson.M{"_id": 2, "a": 1, "b": nil}, dup: true},
			{doc: bson.M{"_id": 3}},
			{doc: bson.M{"_id": 4}, dup: true},
		}},
		// 探针 C2 / C3 / C4：复合 sparse 只在全部字段都缺时跳过。
		{name: "compound_sparse_partial", keys: compound, sparse: true, inserts: []uniqueNullStep{
			{doc: bson.M{"_id": 1}},
			{doc: bson.M{"_id": 2}},
			{doc: bson.M{"_id": 3, "a": 1}},
			{doc: bson.M{"_id": 4, "a": 1}, dup: true},
			{doc: bson.M{"_id": 5, "a": 1, "b": nil}, dup: true},
			{doc: bson.M{"_id": 6, "b": 2}},
			{doc: bson.M{"_id": 7, "a": nil, "b": 2}, dup: true},
		}},
		{name: "compound_sparse_build_partial", keys: compound, sparse: true, existing: []bson.M{{"_id": 1, "a": 1}, {"_id": 2, "a": 1}}, buildDup: true},
		{name: "compound_sparse_build_all_missing", keys: compound, sparse: true, existing: []bson.M{{"_id": 1}, {"_id": 2}}},
		// 探针 P1 / P2：点路径穿过标量父字段等同缺字段。
		{name: "dotted_scalar_parent_is_null", keys: dotted, inserts: []uniqueNullStep{{doc: bson.M{"_id": 1, "m": 5}}, {doc: bson.M{"_id": 2}, dup: true}}},
		{name: "dotted_sparse_scalar_parent_skipped", keys: dotted, sparse: true, inserts: []uniqueNullStep{
			{doc: bson.M{"_id": 1, "m": 5}},
			{doc: bson.M{"_id": 2, "m": 6}},
			{doc: bson.M{"_id": 3, "m": bson.M{}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient().Collection("rr05", tc.name)
			for _, doc := range tc.existing {
				if err := c.Seed(doc); err != nil {
					t.Fatal(err)
				}
			}
			fields := make([]string, 0, len(tc.keys))
			for _, key := range tc.keys {
				fields = append(fields, key.Key)
			}
			err := c.EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: tc.keys, Unique: true, Sparse: tc.sparse}})
			if tc.buildDup {
				if !errors.Is(err, fmongo.ErrDuplicateKey) || c.HasIndex(fields...) {
					t.Fatalf("real Mongo fails the build with 11000 and publishes nothing: err=%v published=%v", err, c.HasIndex(fields...))
				}
				return
			}
			if err != nil {
				t.Fatalf("real Mongo builds this index: err=%v", err)
			}
			for i, step := range tc.inserts {
				_, err := c.InsertOne(ctx, step.doc)
				if step.dup {
					if !errors.Is(err, fmongo.ErrDuplicateKey) || hasErrorLabel(err, TransientTransactionError) {
						t.Fatalf("insert #%d %v: real Mongo reports 11000, fake err=%v", i, step.doc, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("insert #%d %v: real Mongo accepts it, fake err=%v", i, step.doc, err)
				}
			}
		})
	}
	// 探针 U1：$unset 让文档缺唯一字段，与已有的缺字段文档撞 null 键。
	t.Run("unset_collides_with_missing", func(t *testing.T) {
		c := NewClient().Collection("rr05", "unset")
		if err := c.EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: single, Unique: true}}); err != nil {
			t.Fatal(err)
		}
		for _, doc := range []bson.M{{"_id": 1}, {"_id": 2, "name": "x"}} {
			if _, err := c.InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.UpdateOne(ctx, bson.M{"_id": 2}, bson.M{"$unset": bson.M{"name": ""}}); !errors.Is(err, fmongo.ErrDuplicateKey) {
			t.Fatalf("real Mongo rejects the $unset with 11000, fake err=%v", err)
		}
		if doc, _ := c.Lookup(2); doc["name"] != "x" {
			t.Fatalf("rejected update changed the document: %v", doc)
		}
	})
}
