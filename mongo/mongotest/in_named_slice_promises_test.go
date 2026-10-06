// RR-20261006-08（A14，O-S5-6）：$in 的操作数是具名切片（如 saga 的 []Status）时，真实驱动按元素的底层类型编码，
// 替身之前只认 bson.A / []any / []string，报 ErrUnsupported，MongoStore.ClaimDue 在替身上用不了，saga 用例只好绕开。
// 现在按 reflect 展开任意切片 / 数组，元素按底层类型（整型、无符号、浮点、字符串、布尔）比较，与驱动一致。
package mongotest

import (
	"context"
	"errors"
	"sort"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// namedStatus 与 saga.Status 一样以 uint8 为底层类型：驱动只把元素恰好是 byte 的切片编成二进制，[]namedStatus 是数组。
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
