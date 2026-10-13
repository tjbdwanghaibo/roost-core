package container

import (
	"slices"
	"testing"
)

func TestBucketHolder(t *testing.T) {
	bh := NewBucketHolder[int64, int](4, func(k int64) int { return int(k * 10) }, true)

	// Get auto-creates via builder
	v := bh.Get(5)
	if v != 50 {
		t.Fatalf("Expected 50, got %d", v)
	}

	bh.Add(5, 99)
	v = bh.Get(5)
	if v != 99 {
		t.Fatalf("Expected 99 after Add, got %d", v)
	}

	bh.Del(5)
	// After deletion, re-get should rebuild
	v = bh.Get(5)
	if v != 50 {
		t.Fatalf("Expected 50 after Del+rebuild, got %d", v)
	}

	if bh.Count() != 1 {
		t.Fatalf("Expected count 1, got %d", bh.Count())
	}
}

func TestKeyMap(t *testing.T) {
	km := NewKeyMap[int64, string](8)

	km.Set(1, "one")
	km.Set(2, "two")
	km.Set(3, "three")

	if v, ok := km.Get(1); !ok || v != "one" {
		t.Fatalf("Expected 'one', got %v %v", v, ok)
	}

	km.Remove(2)
	if _, ok := km.Get(2); ok {
		t.Fatal("Expected key 2 to be removed")
	}

	if km.Len() != 2 {
		t.Fatalf("Expected len 2, got %d", km.Len())
	}
}

func TestTopologicalSort(t *testing.T) {
	ts := NewTopologicalSortCache[string]()
	ts.RegisterCompDependency("C", "A", "B")
	ts.RegisterCompDependency("B", "A")
	ts.RegisterCompDependency("A")

	sorted := ts.GetTopologicalSortedComponents()
	if len(sorted) != 3 {
		t.Fatalf("Expected 3 sorted components, got %d", len(sorted))
	}
	// A should be first (most depended upon)
	if sorted[0] != "A" {
		t.Fatalf("Expected 'A' first, got %s", sorted[0])
	}
}

// ObjectPool has one surprising property that the previous test left as an
// empty if-body and a comment saying the author was unsure: an object taken
// back off the freelist is returned AS-IS, and resetFunc runs only when the
// object comes from the backing sync.Pool. That asymmetry is the whole point
// of the freelist (a function-scoped reuse with no reset cost), so it is what
// has to be pinned down.
func TestObjectPoolReusesFreelistObjectsWithoutReset(t *testing.T) {
	resets := 0
	pool := NewObjectPool(
		func() *int { v := 0; return &v },
		func(v *int) *int { *v = 0; resets++; return v },
	)

	first := pool.Get()
	*first = 42
	pool.Put(first)

	// Straight off the freelist: same object, value untouched, no reset.
	second := pool.Get()
	if second != first {
		t.Fatal("Get did not reuse the freelist object")
	}
	if *second != 42 {
		t.Fatalf("freelist object was reset to %d; the freelist path must not reset", *second)
	}

	// Release hands everything back to sync.Pool, so the next Get takes the
	// sync.Pool path — and that path does reset.
	pool.Put(second)
	pool.Release()
	resetsBeforeRelease := resets
	third := pool.Get()
	if *third != 0 {
		t.Fatalf("object from sync.Pool was not reset: value %d", *third)
	}
	if resets != resetsBeforeRelease+1 {
		t.Fatalf("expected exactly one reset on the sync.Pool path, got %d", resets-resetsBeforeRelease)
	}
}

// Every object must be tracked by exactly one list. If Put appended to the
// freelist without removing the worklist entry, Release would hand the same
// pointer to sync.Pool twice and two later callers could hold one object.
// That invariant is internal state, so an internal test asserts it directly —
// going through sync.Pool instead would depend on its per-P caching and would
// be flaky rather than rigorous.
func TestObjectPoolPutMovesObjectBetweenLists(t *testing.T) {
	pool := NewObjectPool(func() *int { v := 0; return &v }, nil)

	a, b := pool.Get(), pool.Get()
	if len(pool.workList) != 2 || len(pool.freeList) != 0 {
		t.Fatalf("after 2 Gets: work=%d free=%d, want 2/0", len(pool.workList), len(pool.freeList))
	}

	pool.Put(a)
	if len(pool.workList) != 1 || len(pool.freeList) != 1 {
		t.Fatalf("after Put: work=%d free=%d, want 1/1 — Put must move, not copy",
			len(pool.workList), len(pool.freeList))
	}
	if pool.workList[0] != b {
		t.Fatal("Put removed the wrong worklist entry")
	}

	pool.Put(b)
	if len(pool.workList) != 0 || len(pool.freeList) != 2 {
		t.Fatalf("after both Puts: work=%d free=%d, want 0/2", len(pool.workList), len(pool.freeList))
	}

	// Both come back distinct, which is the property callers actually rely on.
	seen := map[*int]int{pool.Get(): 1}
	seen[pool.Get()]++
	if len(seen) != 2 {
		t.Fatal("a 2-object freelist handed out the same object twice")
	}
}

// Clear drops both lists without returning anything to sync.Pool; Release
// returns them. The distinction matters because Clear is the path used when
// the objects are known to be unsafe to reuse.
func TestObjectPoolClearDropsWithoutReturningToSyncPool(t *testing.T) {
	created := 0
	pool := NewObjectPool(func() *int { created++; v := 0; return &v }, nil)

	dropped := pool.Get()
	pool.Clear()

	// Nothing is tracked any more, so the next Get must mint a fresh object
	// rather than resurrect the cleared one.
	fresh := pool.Get()
	if fresh == dropped {
		t.Fatal("Clear left the object reachable; it must be dropped, not recycled")
	}
}

func TestNewObjectPoolRejectsNilConstructor(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewObjectPool accepted a nil newFunc")
		}
	}()
	NewObjectPool[*int](nil, nil)
}

// 依赖里出现未单独注册的节点时，拓扑排序既不误报环（A→B、B 未注册），也不把真正的环藏起来（A↔B 成环时不能静默丢掉 A、B）。
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

// KeyMap.Range 的回调删除当前键时，其余键各出现一次，不出现不存在的键（Remove 把桶末尾的元素换到当前位置，Range 不能跳过它，
// 也不能把清零的末尾槽以零值键交给回调）。
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

// 同一个对象 Put 两次不能让它在空闲表里出现两份，否则之后两次 Get 交出同一个对象、两个使用者共享一份可变状态。
func TestObjectPoolIgnoresASecondPutOfTheSameObject(t *testing.T) {
	type item struct{ n int }
	pool := NewObjectPool(func() *item { return &item{} }, nil)
	a := pool.Get()
	pool.Put(a)
	pool.Put(a) // 重复归还

	x := pool.Get()
	y := pool.Get()
	if x == y {
		t.Fatalf("two Gets after a double Put returned the same object %p: both callers now share it", x)
	}
	// 邻近分支：从未经 Get 的外来对象仍可放入，且只放一份。
	foreign := &item{n: 7}
	pool.Put(foreign)
	pool.Put(foreign)
	if got := pool.Get(); got != foreign {
		t.Fatalf("Get after Put(foreign) = %p, want the foreign object %p", got, foreign)
	}
	if got := pool.Get(); got == foreign {
		t.Fatal("a foreign object put twice was handed out twice")
	}
}

// 依赖表与调用方互不共享切片：登记时复制调用方的变参切片，GetCompDependencies 返回副本；任一方改动都不影响登记的依赖与排序缓存。
func TestTopologicalSortCacheDoesNotShareSlicesWithCallers(t *testing.T) {
	cache := NewTopologicalSortCache[string]()
	deps := []string{"B", "C"}
	cache.RegisterCompDependency("A", deps...)
	deps[0] = "X" // 调用方复用自己的切片

	if got := cache.GetCompDependencies("A"); !slices.Equal(got, []string{"B", "C"}) {
		t.Errorf("dependencies of A after the caller reused its slice = %v, want [B C]", got)
	}

	got := cache.GetCompDependencies("A")
	got[1] = "Y" // 调用方改返回值
	if again := cache.GetCompDependencies("A"); !slices.Equal(again, []string{"B", "C"}) {
		t.Fatalf("dependencies of A after the caller edited the returned slice = %v, want [B C]", again)
	}
}
