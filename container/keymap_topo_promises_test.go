package container

import (
	"slices"
	"testing"
)

// RR-20261005-NC-184：依赖里出现未单独注册的节点时，拓扑排序既不误报环，
// 也不把真正的环藏起来。
//
// 旧行为：结果长度与“已注册的键”比较，而排序里还包含只作为依赖出现的节点。
// A→B（B 未注册）被当成环返回 nil；A↔B 成环、另有 C→X,Y 时两边长度恰好相等，
// 返回 [Y X C]，A、B 被静默丢掉、环没有报告。
func TestTopologicalSortHandlesUnregisteredDependencies(t *testing.T) {
	leaf := NewTopologicalSortCache[string]()
	leaf.RegisterCompDependency("A", "B")
	if got := leaf.GetTopologicalSortedComponents(); !slices.Equal(got, []string{"B", "A"}) {
		t.Fatalf("A depends on unregistered B: got %v, want [B A]", got)
	}

	masked := NewTopologicalSortCache[string]()
	masked.RegisterCompDependency("A", "B")
	masked.RegisterCompDependency("B", "A")
	masked.RegisterCompDependency("C", "X", "Y")
	if got := masked.GetTopologicalSortedComponents(); got != nil {
		t.Fatalf("A and B form a cycle: got %v, want nil", got)
	}
}

// RR-20261005-NC-185：KeyMap.Range 的回调删除当前键时，其余键各出现一次，
// 不出现不存在的键。
//
// 旧行为：Remove 把桶末尾的元素换到当前位置并清零末尾，Range 按旧长度继续
// 走——换过来的那个键被跳过，末尾清零的槽以零值键 0 被交给回调。
func TestKeyMapRangeWithRemoveOfTheCurrentKey(t *testing.T) {
	m := NewKeyMap[int64, int64](1) // one bucket: every key in the same slice
	for i := int64(1); i <= 4; i++ {
		m.Set(i, i*10)
	}
	seen := map[int64]int{}
	m.Range(func(k, v int64) bool {
		seen[k]++
		if v != k*10 {
			t.Errorf("key %d delivered with value %d, want %d", k, v, k*10)
		}
		if k == 1 {
			m.Remove(k)
		}
		return true
	})
	want := map[int64]int{1: 1, 2: 1, 3: 1, 4: 1}
	if len(seen) != len(want) {
		t.Fatalf("Range delivered %v, want each of 1..4 exactly once", seen)
	}
	for k, n := range want {
		if seen[k] != n {
			t.Fatalf("Range delivered %v, want each of 1..4 exactly once", seen)
		}
	}
	if m.Len() != 3 {
		t.Fatalf("Len = %d after removing one of four, want 3", m.Len())
	}
}
