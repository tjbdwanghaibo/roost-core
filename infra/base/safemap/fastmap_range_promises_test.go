package safemap

import "testing"

// RR-20261005-NC-182：FastMap.Range 的回调可以改这个 map，遍历仍然只交出
// 真实存在的键，且每个未被删除的原有键恰好一次。
//
// 生成 DAO 的 RangeX 对 map=fast 字段直接转发 FastMap.Range，所以
// `RangeItems(func(k, v) { SetItems(k, v*10) })` 这类写法就是这里的场景。
//
// 旧行为：Set 在查找键之前先检查装载率，改一个已有键也会扩容重排；Range 按
// 旧 states 的下标去读新的 keys / values，交出零值键 0（回调再 Set(0, 0) 就把
// 一个不存在的键写进 map、写进持久化）并漏掉一部分原有键。回调里 Clear 直接
// 下标越界 panic。
func TestFastMapRangeToleratesWritesFromTheCallback(t *testing.T) {
	fill := func() *FastMap[int64, int64] {
		m := NewInt64FastMap[int64](0)
		for i := int64(1); i <= 6; i++ { // 6 of 8 slots: the next growth check trips
			m.Set(i, i)
		}
		return m
	}
	exactlyOnce := func(t *testing.T, seen map[int64]int, keys ...int64) {
		t.Helper()
		if len(seen) != len(keys) {
			t.Fatalf("Range delivered %v, want each of %v exactly once", seen, keys)
		}
		for _, k := range keys {
			if seen[k] != 1 {
				t.Fatalf("Range delivered %v, want each of %v exactly once", seen, keys)
			}
		}
	}

	t.Run("update every existing key", func(t *testing.T) {
		m := fill()
		seen := map[int64]int{}
		m.Range(func(k, v int64) bool {
			seen[k]++
			m.Set(k, v*10)
			return true
		})
		exactlyOnce(t, seen, 1, 2, 3, 4, 5, 6)
		if m.Len() != 6 {
			t.Fatalf("Len = %d after updating six existing keys, want 6", m.Len())
		}
		for i := int64(1); i <= 6; i++ {
			if v, ok := m.Get(i); !ok || v != i*10 {
				t.Fatalf("key %d = %d,%v after the update pass, want %d", i, v, ok, i*10)
			}
		}
		if _, ok := m.Get(0); ok {
			t.Fatal("the update pass created key 0, which was never in the map")
		}
	})

	t.Run("insert keys until the table grows", func(t *testing.T) {
		m := fill()
		seen := map[int64]int{}
		m.Range(func(k, v int64) bool {
			if k <= 6 {
				seen[k]++
			}
			if k == 1 {
				for n := int64(100); n < 120; n++ {
					m.Set(n, n)
				}
			}
			if k == 0 {
				t.Fatal("Range delivered key 0, which was never in the map")
			}
			return true
		})
		exactlyOnce(t, seen, 1, 2, 3, 4, 5, 6)
		if m.Len() != 26 {
			t.Fatalf("Len = %d, want 26", m.Len())
		}
	})

	t.Run("delete a key not yet reached after growth", func(t *testing.T) {
		m := fill()
		var first int64
		deleted := map[int64]bool{}
		m.Range(func(k, v int64) bool {
			if deleted[k] {
				t.Fatalf("Range delivered key %d after the callback deleted it", k)
			}
			if first == 0 {
				first = k
				for n := int64(100); n < 120; n++ {
					m.Set(n, n)
				}
				for i := int64(1); i <= 6; i++ {
					if i != k {
						m.Delete(i)
						deleted[i] = true
					}
				}
			}
			return true
		})
	})

	t.Run("clear from the callback", func(t *testing.T) {
		m := fill()
		calls := 0
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Clear inside Range panicked: %v", r)
				}
			}()
			m.Range(func(int64, int64) bool {
				calls++
				m.Clear()
				return true
			})
		}()
		if calls != 1 {
			t.Fatalf("Range delivered %d entries, want 1: the map was cleared after the first", calls)
		}
		if m.Len() != 0 {
			t.Fatalf("Len = %d after Clear, want 0", m.Len())
		}
	})
}
