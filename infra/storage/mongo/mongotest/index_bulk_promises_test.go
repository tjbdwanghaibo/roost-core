// RR-20261004-NC-26～28：多层D同级字段、逐个索引发布、非法模型预检与合法ordered部分成功。
package mongotest

import (
	"context"
	"errors"
	"fmt"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

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
