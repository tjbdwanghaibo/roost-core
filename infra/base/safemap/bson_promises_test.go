package safemap

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// U-0265 · C2 · RR-20260920-07：这个类型说它自己会编码 BSON，而它并不会。
//
// 驱动的 ValueMarshaler 是 `MarshalBSONValue() (byte, []byte, error)`；这里写的是
// `(bson.Type, []byte, error)`，而 `bson.Type` 是 `type Type byte`——**定义类型，不是别名**。
// 签名不匹配时 Go 不报错，接口只是没有被实现，方法被静默忽略。类型于是退回默认结构体
// 编码器，而它的字段全是未导出的，结果是**空文档**：内容一声不响地没了。
//
// 承诺：一个 SmallSafeMap 写进 BSON 再读回来，还是它自己。
func TestSmallSafeMapSurvivesABSONRoundTrip(t *testing.T) {
	for name, src := range map[string]map[string]int64{
		"empty":   {},
		"one":     {"a": 1},
		"several": {"a": 1, "b": 2, "c": 3},
	} {
		t.Run(name, func(t *testing.T) {
			value := &SmallSafeMap[string, int64]{}
			value.SetRawMap(src)

			raw, err := bson.Marshal(bson.M{"m": value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back struct {
				M *SmallSafeMap[string, int64] `bson:"m"`
			}
			if err := bson.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.M == nil {
				t.Fatal("the map came back nil")
			}
			got := back.M.RawMap()
			if len(got) != len(src) {
				t.Fatalf("round trip produced %d entries, want %d (%v)", len(got), len(src), got)
			}
			for key, want := range src {
				if got[key] != want {
					t.Fatalf("round trip lost %q: got %d want %d", key, got[key], want)
				}
			}
		})
	}
}

// 写出去的必须是一个**文档**，不是默认结构体编码器产出的空文档。这条和上面那条分开，
// 是因为一个同样空的 map 会让上面那条在损坏的实现下也通过。
func TestSmallSafeMapEncodesItsContentsNotItsFields(t *testing.T) {
	value := &SmallSafeMap[string, int64]{}
	value.SetRawMap(map[string]int64{"a": 7})
	raw, err := bson.Marshal(bson.M{"m": value})
	if err != nil {
		t.Fatal(err)
	}
	var back bson.M
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	document, ok := back["m"].(bson.D)
	if !ok {
		t.Fatalf("encoded as %T, want a document", back["m"])
	}
	if len(document) != 1 || document[0].Key != "a" {
		t.Fatalf("encoded as %v, want the map's own entries", document)
	}
}

// 让下一次写错签名在**编译期**就红，而不是等到某个文档变成 {}。
var (
	_ bson.ValueMarshaler   = (*SmallSafeMap[string, int64])(nil)
	_ bson.ValueUnmarshaler = (*SmallSafeMap[string, int64])(nil)
)
