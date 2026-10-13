// 替身索引与 bulk：路径读写、唯一索引发布与数组 / 缺失字段、bulk 预检与部分成功。

package mongotest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// BSON 路径、索引发布、bulk 预检与事务自身回滚。历史：RR-20261004-NC-26～29。
func TestMongoBoundaryPromises(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"read", "set", "unset"} {
		for _, nested := range []bool{false, true} {
			name := op + "_flat_control"
			if nested {
				name = op + "_nested"
			}
			t.Run(name, func(t *testing.T) {
				c := NewClient().Collection("r16", name)
				doc := bson.M{"_id": int64(1), "n": int64(1), "keep": "yes"}
				path := "n"
				if nested {
					doc = bson.M{"_id": int64(1), "meta": bson.M{"n": int64(1), "keep": "yes"}}
					path = "meta.n"
				}
				if err := c.Seed(doc); err != nil {
					t.Fatal(err)
				}
				if op == "read" {
					count, err := c.CountDocuments(ctx, bson.M{path: int64(1)})
					if err != nil || count != 1 {
						t.Fatalf("existing nested field not matched: count=%d err=%v", count, err)
					}
					return
				}
				update := bson.M{"$set": bson.M{path: int64(9)}}
				if op == "unset" {
					update = bson.M{"$unset": bson.M{path: 1}}
				}
				if _, err := c.UpdateOne(ctx, bson.M{"_id": int64(1)}, update); err != nil {
					t.Fatal(err)
				}
				var got struct {
					N    int64  `bson:"n"`
					Keep string `bson:"keep"`
					Meta struct {
						N    int64  `bson:"n"`
						Keep string `bson:"keep"`
					} `bson:"meta"`
				}
				if err := c.FindOne(ctx, bson.M{"_id": int64(1)}, &got); err != nil {
					t.Fatal(err)
				}
				n, keep := got.N, got.Keep
				if nested {
					n, keep = got.Meta.N, got.Meta.Keep
				}
				want := int64(9)
				if op == "unset" {
					want = 0
				}
				if n != want || keep != "yes" {
					t.Fatalf("dotted %s changed wrong fields: n=%d keep=%q", op, n, keep)
				}
			})
		}
	}
	for _, duplicate := range []bool{false, true} {
		name := "unique_distinct_control"
		if duplicate {
			name = "unique_existing_duplicates"
		}
		t.Run(name, func(t *testing.T) {
			c := NewClient().Collection("r16", name)
			for _, id := range []int64{1, 2} {
				value := id
				if duplicate {
					value = 1
				}
				if err := c.Seed(bson.M{"_id": id, "name": value}); err != nil {
					t.Fatal(err)
				}
			}
			err := c.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "name_unique", Keys: bson.D{{Key: "name", Value: 1}}, Unique: true}})
			if duplicate {
				if !errors.Is(err, fmongo.ErrDuplicateKey) {
					t.Fatalf("unique index accepted existing duplicates: err=%v has=%v", err, c.HasIndex("name"))
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, unsupported := range []bool{false, true} {
		name := "bulk_valid_control"
		if unsupported {
			name = "bulk_unsupported_preflight"
		}
		t.Run(name, func(t *testing.T) {
			c := NewClient().Collection("r16", name)
			models := []fmongo.WriteModel{{Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": int64(1)}}}
			if unsupported {
				models = append(models, fmongo.WriteModel{Type: 255})
			}
			_, err := c.BulkWrite(ctx, models)
			count, countErr := c.CountDocuments(ctx, bson.M{})
			if countErr != nil {
				t.Fatal(countErr)
			}
			if unsupported {
				if !errors.Is(err, ErrUnsupported) || count != 0 {
					t.Fatalf("invalid model caused prefix side effect: count=%d err=%v", count, err)
				}
			} else if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v", count, err)
			}
		})
	}
	for _, mode := range []string{"serial_control", "concurrent_existing", "concurrent_new_collection"} {
		t.Run("rollback_"+mode, func(t *testing.T) {
			client := NewClient()
			c := client.Collection("r16", mode)
			if err := c.Seed(bson.M{"_id": int64(1), "n": int64(1)}); err != nil {
				t.Fatal(err)
			}
			session, err := client.StartSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer session.EndSession(ctx)
			abort := errors.New("abort A")
			entered := make(chan struct{})
			resume := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- session.WithTransaction(ctx, func(tx context.Context) error {
					if _, err := c.UpdateOne(tx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"n": int64(9)}}); err != nil {
						return err
					}
					close(entered)
					<-resume
					return abort
				})
			}()
			<-entered
			target := c
			if mode == "concurrent_new_collection" {
				target = client.Collection("r16", "created_by_B")
			}
			if mode != "serial_control" {
				b, err := client.StartSession(ctx)
				if err != nil {
					close(resume)
					<-done
					t.Fatal(err)
				}
				err = b.WithTransaction(ctx, func(tx context.Context) error {
					_, err := target.InsertOne(tx, bson.M{"_id": int64(2), "n": int64(2)})
					return err
				})
				b.EndSession(ctx)
				if err != nil {
					close(resume)
					<-done
					t.Fatal(err)
				}
			}
			close(resume)
			if err := <-done; !errors.Is(err, abort) {
				t.Fatal(err)
			}
			if mode == "serial_control" {
				if got := boundaryStoredNumber(t, c); got != 1 {
					t.Fatalf("serial rollback n=%d", got)
				}
				return
			}
			count, err := target.CountDocuments(ctx, bson.M{"_id": int64(2)})
			if err != nil || count != 1 {
				t.Fatalf("abort A erased committed B: count=%d err=%v", count, err)
			}
		})
	}
}

func boundaryStoredNumber(t *testing.T, c *Collection) int64 {
	t.Helper()
	var doc struct {
		N int64 `bson:"n"`
	}
	if err := c.FindOne(context.Background(), bson.M{"_id": int64(1)}, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.N
}

// 多层 D 同级字段、逐个索引发布、非法模型预检与合法 ordered 部分成功。历史：RR-20261004-NC-26～28。
func TestIndexAndBulkBoundaryPromises(t *testing.T) {
	ctx := context.Background()
	t.Run("deep_D_siblings_and_unvisited_array", func(t *testing.T) {
		c := NewClient().Collection("tx", "deep")
		if err := c.Seed(bson.M{"_id": 1, "meta": bson.D{{Key: "inner", Value: bson.D{{Key: "n", Value: 1}, {Key: "keep", Value: "yes"}}}, {Key: "array", Value: bson.A{bson.D{{Key: "x", Value: 1}}}}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.UpdateOne(ctx, bson.M{"meta.inner.n": 1}, bson.M{"$set": bson.M{"meta.inner.n": 9}}); err != nil {
			t.Fatal(err)
		}
		n, err := c.CountDocuments(ctx, bson.M{"meta.inner.n": 9, "meta.inner.keep": "yes"})
		if err != nil || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		doc, _ := c.Lookup(1)
		value, ok := lookupPath(doc, "meta.array")
		if !ok || len(value.(bson.A)) != 1 {
			t.Fatal(doc)
		}
	})
	t.Run("failed_unique_is_not_published", func(t *testing.T) {
		c := NewClient().Collection("tx", "unique")
		for _, id := range []int{1, 2} {
			if err := c.Seed(bson.M{"_id": id, "a": 1, "b": 2}); err != nil {
				t.Fatal(err)
			}
		}
		err := c.EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}}, Unique: true}})
		if !errors.Is(err, fmongo.ErrDuplicateKey) || c.HasIndex("a", "b") {
			t.Fatalf("err=%v indexes=%v", err, c.Indexes)
		}
		if _, err := c.InsertOne(ctx, bson.M{"_id": 3, "a": 1, "b": 2}); err != nil {
			t.Fatalf("failed index enforced: %v", err)
		}
	})
	t.Run("index_batch_keeps_prior_success", func(t *testing.T) {
		c := NewClient().Collection("tx", "partial")
		for _, id := range []int{1, 2} {
			_ = c.Seed(bson.M{"_id": id, "a": 1, "b": id})
		}
		err := c.EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "b", Value: 1}}, Unique: true}, {Keys: bson.D{{Key: "a", Value: 1}}, Unique: true}})
		if !errors.Is(err, fmongo.ErrDuplicateKey) || !c.HasIndex("b") || c.HasIndex("a") {
			t.Fatalf("err=%v indexes=%v", err, c.Indexes)
		}
		if _, err := c.InsertOne(ctx, bson.M{"_id": 3, "b": 1}); !errors.Is(err, fmongo.ErrDuplicateKey) {
			t.Fatal(err)
		}
	})
	for pos := 0; pos < 3; pos++ {
		t.Run(fmt.Sprintf("invalid_model_%d", pos), func(t *testing.T) {
			c := NewClient().Collection("tx", "bulk")
			models := []fmongo.WriteModel{{Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 1}}, {Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 2}}, {Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 3}}}
			models[pos].Type = 255
			_, err := c.BulkWrite(ctx, models)
			n, _ := c.CountDocuments(ctx, bson.M{})
			if !errors.Is(err, ErrUnsupported) || n != 0 {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
	t.Run("valid_ordered_conflict_keeps_prefix", func(t *testing.T) {
		c := NewClient().Collection("tx", "ordered")
		result, err := c.BulkWrite(ctx, []fmongo.WriteModel{{Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 1}}, {Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 1}}, {Type: fmongo.WriteModelInsertOne, Document: bson.M{"_id": 3}}})
		n, _ := c.CountDocuments(ctx, bson.M{})
		if !errors.Is(err, fmongo.ErrDuplicateKey) || n != 1 || result.InsertedCount != 1 {
			t.Fatalf("n=%d result=%v err=%v", n, result, err)
		}
	})
}

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
