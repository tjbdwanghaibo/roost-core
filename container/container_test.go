package container

import "testing"

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
