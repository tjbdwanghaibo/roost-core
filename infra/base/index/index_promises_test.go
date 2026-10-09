package index

import (
	"math"
	"testing"
)

// RR-20261005-NC-144：值不等于自身（float NaN，或装着 NaN 的接口 / 结构体）也是合法的 comparable 值。
// 旧 Upsert 先 make 反查桶、再用 i.reverse[value] 取回它写入——NaN 作 map 键取不回来，第一次插入就
// panic（assignment to entry in nil map）。承诺：插入不 panic，主表照常可读、可删；反查表不留取不回的桶。
func TestUpsertAcceptsAValueThatIsNotEqualToItself(t *testing.T) {
	idx := NewIndex[int, float64]()
	idx.Upsert(1, math.NaN())
	idx.Upsert(1, math.NaN())
	idx.Upsert(2, 3)
	if got, ok := idx.Get(1); !ok || !math.IsNaN(got) {
		t.Fatalf("Get(1) = %v, %v; want the NaN that was stored", got, ok)
	}
	if idx.Len() != 2 {
		t.Fatalf("Len = %d, want 2", idx.Len())
	}
	idx.Upsert(1, 4)
	if got := idx.Query(4); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Query(4) after moving key 1 off NaN = %v", got)
	}
	idx.Upsert(1, math.NaN())
	idx.Delete(1)
	if idx.Len() != 1 || len(idx.reverse) != 1 {
		t.Fatalf("after Delete: Len = %d, reverse buckets = %d; want only key 2's bucket", idx.Len(), len(idx.reverse))
	}
	if got := idx.Query(4); len(got) != 0 {
		t.Fatalf("Query(4) after key 1 moved back to NaN and was deleted = %v", got)
	}
}

// RR-20261005-NC-145：K 为接口类型（如 any）时，同一个值下的键可以是不同动态类型或 nil。旧 defaultLess 只看
// 左操作数的 Kind，就对右操作数调用 Int()/Uint()/Float()，遇到 string、uint 或 nil 时 panic；是否触发取决于
// sort 比较的先后顺序。承诺：默认排序对任意 comparable 键都给出确定的全序，不 panic。
func TestDefaultOrderHandlesKeysOfDifferentDynamicTypes(t *testing.T) {
	keys := []any{int64(10), "x", nil, uint(3), int32(2), 1.5, "a", int64(-1), true}
	for rotation := range keys {
		idx := NewOrderedIndex[any, string](nil)
		for i := range keys {
			idx.Upsert(keys[(i+rotation)%len(keys)], "g")
		}
		var got []any
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("rotation %d: Query panicked: %v", rotation, r)
				}
			}()
			got = idx.Query("g")
		}()
		if len(got) != len(keys) {
			t.Fatalf("rotation %d: Query = %v", rotation, got)
		}
		if rotation == 0 {
			keys = append([]any(nil), got...)
			continue
		}
		for i := range got {
			if got[i] != keys[i] {
				t.Fatalf("rotation %d: order %v differs from %v; the default order is not a total order", rotation, got, keys)
			}
		}
	}
	// Same-kind keys keep their natural order (the earlier numeric fix).
	idx := NewOrderedIndex[any, string](nil)
	for _, key := range []any{int64(10), int64(9), int64(100)} {
		idx.Upsert(key, "n")
	}
	if got := idx.Query("n"); got[0] != int64(9) || got[1] != int64(10) || got[2] != int64(100) {
		t.Fatalf("same-kind numeric order = %v", got)
	}
}

// RR-20261005-NC-146：Index 的零值可直接使用（ensureLocked），OrderedIndex 的零值却把 Upsert 静默丢掉
// （内部指针为 nil，转发到 nil 接收者直接返回），nil *OrderedIndex 的 Upsert/Get 则 panic。
// 承诺：零值 OrderedIndex 与零值 Index 一样可用；nil 指针与 *Index 一样是空索引。
func TestZeroOrderedIndexIsUsable(t *testing.T) {
	var idx OrderedIndex[int64, string]
	idx.Upsert(2, "g")
	idx.Upsert(1, "g")
	if got, ok := idx.Get(2); !ok || got != "g" {
		t.Fatalf("Get after Upsert on a zero OrderedIndex = %q, %v", got, ok)
	}
	if got := idx.Query("g"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Query = %v", got)
	}
	if idx.Len() != 2 {
		t.Fatalf("Len = %d", idx.Len())
	}
	idx.Delete(1)
	idx.Clear()
	if idx.Len() != 0 {
		t.Fatalf("Len after Clear = %d", idx.Len())
	}

	var none *OrderedIndex[int64, string]
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a nil *OrderedIndex panicked: %v", r)
			}
		}()
		none.Upsert(1, "g")
		none.Delete(1)
		none.Clear()
		if _, ok := none.Get(1); ok || none.Len() != 0 || none.Query("g") != nil {
			t.Fatal("a nil *OrderedIndex is not empty")
		}
	}()
}
