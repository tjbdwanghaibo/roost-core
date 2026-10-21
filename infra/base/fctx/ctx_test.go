package fctx

import (
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
)

func TestCurrentContext_NilWithoutStore(t *testing.T) {
	// Ensure no leftover from other tests
	DeleteContext()

	got := CurrentContext()
	if got != nil {
		t.Fatal("CurrentContext should return nil without store")
	}
}

func TestCurrentContext_Release(t *testing.T) {
	_, release := NewContext()
	release()

	got := CurrentContext()
	if got != nil {
		t.Fatal("CurrentContext should return nil after release")
	}
}

func TestCurrentContext_NestedReleaseRestoresParent(t *testing.T) {
	parent, releaseParent := NewContext(WithSource("parent"))
	defer releaseParent()

	child, releaseChild := NewContext(WithSource("child"))
	if got := CurrentContext(); got != child {
		t.Fatal("CurrentContext should point at child before child release")
	}
	releaseChild()

	if got := CurrentContext(); got != parent {
		t.Fatal("CurrentContext should restore parent after child release")
	}
}

func TestCurrentContext_PerGoroutine(t *testing.T) {
	c1, release1 := NewContext()
	defer release1()

	var c2 *Context
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		innerC, release := NewContext()
		defer release()
		c2 = innerC

		// Should not see c1
		got := CurrentContext()
		if got != innerC {
			t.Error("goroutine should see its own context")
		}
	}()
	wg.Wait()

	// Main goroutine still sees c1
	got := CurrentContext()
	if got != c1 {
		t.Fatal("main goroutine should still see c1")
	}
	_ = c2
}

func TestContext_KV(t *testing.T) {
	c, release := NewContext()
	defer release()

	c.Set("key1", "val1")
	v, ok := c.Get("key1")
	if !ok || v != "val1" {
		t.Fatal("Set/Get should work")
	}

	_, ok = c.Get("nonexist")
	if ok {
		t.Fatal("Get missing key should return false")
	}
}

func TestContextTraceSnapshotClonesTraceMeta(t *testing.T) {
	trace := TraceMeta{
		TraceID: "trace-test",
		Enabled: true,
		Reason:  "player_protocol",
		Sampled: true,
		Tags: map[string]string{
			"player_id": "1001",
		},
	}
	c, release := NewContext(WithTrace(trace))
	defer release()

	if !c.Trace.Active() {
		t.Fatal("trace should be active on current context")
	}
	snap := CaptureSnapshot()
	c.Trace.Tags["player_id"] = "changed"

	child, releaseChild := NewContext(WithSnapshot(snap))
	defer releaseChild()

	if child.Trace.TraceID != "trace-test" || child.Trace.Reason != "player_protocol" {
		t.Fatalf("trace meta = %+v", child.Trace)
	}
	if got := child.Trace.Tags["player_id"]; got != "1001" {
		t.Fatalf("snapshot tag player_id = %q, want 1001", got)
	}
	child.Trace.Tags["player_id"] = "child"
	if got := snap.Trace.Tags["player_id"]; got != "1001" {
		t.Fatalf("snapshot tag mutated through child context: %q", got)
	}
}

func TestGoID(t *testing.T) {
	id := GoID()
	if id <= 0 {
		t.Fatalf("goID should be positive, got %d", id)
	}

	// Different goroutines should have different IDs
	var otherId int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		otherId = GoID()
	}()
	wg.Wait()

	if otherId == id {
		t.Fatal("different goroutines should have different IDs")
	}
}

// Frozen 在一次执行里只算一次；新的执行（新的请求上下文、交接后的执行）重新冻结；没有请求上下文时不缓存。
func TestFrozenHoldsOneValuePerExecution(t *testing.T) {
	DeleteContext()
	type key struct{}
	calls := 0
	compute := func() any { calls++; return calls }

	if Frozen(key{}, compute) != 1 || Frozen(key{}, compute) != 2 {
		t.Fatal("without a request context every read must compute afresh")
	}

	c, release := NewContext()
	first := Frozen(key{}, compute)
	if again := Frozen(key{}, compute); again != first {
		t.Fatalf("second read in one execution = %v, want the frozen %v", again, first)
	}
	snapshot := c.Snapshot()
	release()

	_, release = NewContext(WithSnapshot(snapshot))
	defer release()
	if next := Frozen(key{}, compute); next == first {
		t.Fatalf("a new execution from a snapshot read the previous execution's frozen value %v", next)
	}
}

// 同一次执行里 Now 冻结：真实时间来源每读一次走一步，执行里的 Now / NowMilli 仍是建立上下文时的那一刻。
func TestNowIsFrozenPerExecution(t *testing.T) {
	DeleteContext()
	var mu sync.Mutex
	tick := time.Unix(1_800_000_000, 0)
	clock.SetSource(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Millisecond)
		return tick
	})
	t.Cleanup(func() { clock.SetSource(nil) })

	c, release := NewContext()
	defer release()
	if a, b := Now(), Now(); !a.Equal(b) || !a.Equal(c.Now) {
		t.Fatalf("Now inside one execution moved: %v then %v (frozen %v)", a, b, c.Now)
	}
	if NowMilli() != c.Now.UnixMilli() {
		t.Fatalf("NowMilli = %d, want the frozen %d", NowMilli(), c.Now.UnixMilli())
	}
}

// Refreeze 在同一个请求上下文里开始新的一次执行：Now / NowMilli 换成此刻的业务时钟，Frozen 的派生值重新计算；
// 之后在这次执行里照样冻结。
// 变异核对：Refreeze 不清 Frozen（去掉 clearFrozen）时第二次执行读到上一次的派生值，本用例变红。
func TestRefreezeStartsANewExecutionInTheSameContext(t *testing.T) {
	DeleteContext()
	var mu sync.Mutex
	tick := time.Unix(1_800_000_000, 0)
	clock.SetSource(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Millisecond)
		return tick
	})
	t.Cleanup(func() { clock.SetSource(nil) })
	type key struct{}
	calls := 0
	compute := func() any { calls++; return calls }

	c, release := NewContext()
	defer release()
	before, frozen := Now(), Frozen(key{}, compute)
	c.Refreeze()
	after := Now()
	if !after.After(before) || NowMilli() != after.UnixMilli() {
		t.Fatalf("Refreeze kept Now %v -> %v (NowMilli %d)", before, after, NowMilli())
	}
	if again := Now(); !again.Equal(after) {
		t.Fatalf("Now moved inside the new execution: %v then %v", after, again)
	}
	if next := Frozen(key{}, compute); next == frozen {
		t.Fatalf("Frozen after Refreeze = %v, the previous execution's value", next)
	}
}
