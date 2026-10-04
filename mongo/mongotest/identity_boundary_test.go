// RR-20261004-NC-22～25：通过公开集合 API 验证数据隔离、唯一候选、精确数值和返回身份边界。
package mongotest

import (
	"context"
	"errors"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"math"
	"math/big"
	"reflect"
	"testing"
)

func TestMongoIdentityBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("duplicate_members_count_and_update", func(t *testing.T) {
		c := NewClient().Collection("identity", "members")
		for _, id := range []int64{1, 2} {
			if err := c.Seed(bson.M{"_id": id, "n": int64(0)}); err != nil {
				t.Fatal(err)
			}
		}
		filter := bson.M{"_id": bson.M{"$in": bson.A{int32(1), int64(1), int64(99), int64(2), int32(2)}}}
		count, err := c.CountDocuments(ctx, filter)
		if err != nil || count != 2 {
			t.Fatalf("count=%d err=%v", count, err)
		}
		result, err := c.UpdateMany(ctx, filter, bson.M{"$inc": bson.M{"n": int64(1)}})
		if err != nil || result.MatchedCount != 2 || result.ModifiedCount != 2 {
			t.Fatalf("update=%+v err=%v", result, err)
		}
		for _, doc := range c.Documents() {
			if doc["n"] != int64(1) {
				t.Fatalf("document updated twice: %v", doc)
			}
		}
	})
	for _, kind := range []string{"array", "binary", "scope", "lookup", "documents", "failed_update"} {
		t.Run("copy_"+kind, func(t *testing.T) {
			c := NewClient().Collection("identity", kind)
			seed := bson.M{"_id": int64(1), "array": bson.A{bson.M{"n": int64(1)}}, "binary": bson.Binary{Subtype: 0x80, Data: []byte{1, 2}}, "scope": bson.CodeWithScope{Code: "x", Scope: bson.M{"n": int64(1)}}}
			if err := c.Seed(seed); err != nil {
				t.Fatal(err)
			}
			var before bson.Raw
			if err := c.FindOne(ctx, bson.M{"_id": int64(1)}, &before); err != nil {
				t.Fatal(err)
			}
			var rows []bson.M
			if err := c.Find(ctx, bson.M{}, &rows); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "array":
				rows[0]["array"].(bson.A)[0].(bson.D)[0].Value = int64(9)
			case "binary":
				rows[0]["binary"].(bson.Binary).Data[0] = 9
			case "scope":
				rows[0]["scope"].(bson.CodeWithScope).Scope.(bson.D)[0].Value = int64(9)
			case "lookup":
				doc, ok := c.Lookup(int64(1))
				if !ok {
					t.Fatal("missing")
				}
				doc["binary"].(bson.Binary).Data[0] = 9
			case "documents":
				c.Documents()[0]["array"].(bson.A)[0].(bson.D)[0].Value = int64(9)
			case "failed_update":
				// First put a BSON.M into storage through the supported dotted update.
				if _, err := c.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"meta.n": int64(1)}}); err != nil {
					t.Fatal(err)
				}
				// 修改工作副本后因身份变化被拒绝，嵌套改动也不得泄漏。
				_, err := c.UpdateOne(ctx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"meta.n": int64(9), "_id": int64(2)}})
				if !errors.Is(err, ErrUnsupported) {
					t.Fatal(err)
				}
				if got := identityStoredNumber(t, c, true); got != 1 {
					t.Fatalf("failed update leaked n=%d", got)
				}
				return
			}
			var after bson.M
			if err := c.FindOne(ctx, bson.M{"_id": int64(1)}, &after); err != nil {
				t.Fatal(err)
			}
			var expected bson.M
			if err := bson.Unmarshal(before, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(expected, after) {
				t.Fatalf("read alias changed stored data: %v", after)
			}
		})
	}
	for _, n := range []int64{1 << 53, math.MaxInt64 - 1, math.MinInt64} {
		t.Run("precise_"+stringID(n), func(t *testing.T) {
			c := NewClient().Collection("identity", "numbers")
			if err := c.Seed(bson.M{"_id": int64(1), "version": n}); err != nil {
				t.Fatal(err)
			}
			for _, filter := range []bson.M{{"version": n + 1}, {"version": bson.M{"$gt": n}}, {"version": bson.M{"$in": bson.A{n + 1}}}} {
				count, err := c.CountDocuments(ctx, filter)
				if err != nil || count != 0 {
					t.Fatalf("query %v count=%d err=%v", filter, count, err)
				}
			}
			count, err := c.CountDocuments(ctx, bson.M{"version": bson.M{"$gte": n}})
			if err != nil || count != 1 {
				t.Fatalf("gte count=%d err=%v", count, err)
			}
		})
	}
	t.Run("sort_adjacent_large_integers", func(t *testing.T) {
		c := NewClient().Collection("identity", "sort")
		for _, doc := range []bson.M{{"_id": int64(1), "version": int64(1<<53) + 1}, {"_id": int64(2), "version": int64(1 << 53)}} {
			if err := c.Seed(doc); err != nil {
				t.Fatal(err)
			}
		}
		var rows []bson.M
		if err := c.Find(ctx, bson.M{}, &rows, fmongo.FindOption{Sort: bson.D{{Key: "version", Value: 1}}}); err != nil {
			t.Fatal(err)
		}
		if rows[0]["_id"] != int64(2) {
			t.Fatal(rows)
		}
	})
	for _, tc := range []struct {
		name        string
		left, right any
		cmp         int
	}{
		{"signed_width", int32(7), int64(7), 0}, {"unsigned_extreme", uint64(math.MaxUint64), int64(math.MaxInt64), 1},
		{"negative_unsigned", int64(-1), uint64(0), -1}, {"float_exact", int64(1 << 53), float64(1 << 53), 0},
		{"float_rounding", int64(1<<53) + 1, float64(1 << 53), 1}, {"fraction", int64(-1), float64(-1.5), 1},
	} {
		t.Run("mixed_"+tc.name, func(t *testing.T) {
			cmp, err := compareValues(tc.left, tc.right)
			if err != nil || cmp != tc.cmp {
				t.Fatalf("cmp=%d err=%v", cmp, err)
			}
			equal, err := valuesEqual(tc.left, tc.right)
			if err != nil || equal != (tc.cmp == 0) {
				t.Fatalf("equal=%v err=%v", equal, err)
			}
			reverse, err := compareValues(tc.right, tc.left)
			if err != nil || reverse != -tc.cmp {
				t.Fatalf("reverse=%d err=%v", reverse, err)
			}
		})
	}
	t.Run("nonfinite_rejected", func(t *testing.T) {
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			if _, err := compareValues(v, int64(1)); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("%v err=%v", v, err)
			}
		}
	})
	for _, mode := range []string{"before", "after", "upsert_after", "upsert_before", "nil_result", "failure"} {
		t.Run("image_"+mode, func(t *testing.T) {
			c := NewClient().Collection("identity", mode)
			upsert := mode == "upsert_after" || mode == "upsert_before"
			if !upsert {
				for _, id := range []int64{1, 2} {
					if err := c.Seed(bson.M{"_id": id, "version": int64(1)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			filter := bson.M{"version": int64(1)}
			if upsert {
				filter["_id"] = int64(3)
			}
			update := bson.M{"$set": bson.M{"version": int64(2)}}
			if mode == "failure" {
				update = bson.M{"$bad": bson.M{"version": int64(2)}}
			}
			var got bson.M
			var result any = &got
			if mode == "nil_result" {
				result = nil
			}
			err := c.FindOneAndUpdate(ctx, filter, update, result, fmongo.FindOneAndUpdateOption{ReturnAfter: mode != "before" && mode != "upsert_before", Upsert: upsert})
			if mode == "failure" {
				if !errors.Is(err, ErrUnsupported) || got != nil {
					t.Fatalf("%v %v", err, got)
				}
				return
			}
			if mode == "upsert_before" {
				if !errors.Is(err, fmongo.ErrNotFound) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "nil_result" {
				return
			}
			wantID, wantVersion := int64(1), int64(2)
			if upsert {
				wantID = 3
			}
			if mode == "before" {
				wantVersion = 1
			}
			if got["_id"] != wantID || got["version"] != wantVersion {
				t.Fatalf("wrong image: %v", got)
			}
		})
	}
}

func stringID(n int64) string { return new(big.Int).SetInt64(n).String() }
