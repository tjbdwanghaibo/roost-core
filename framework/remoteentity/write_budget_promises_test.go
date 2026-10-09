package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// U-0266 · C8 · RR-20260920-08：OpTimeout 是一次远端写的**总**预算，排队也算在里面。
//
// 旧行为：`beginWrite` 先在每实体一格的写闸上 select，只受调用方 ctx 约束，拿到闸之后
// 才 `context.WithTimeout(ctx, OpTimeout)`。于是配置成 3 秒的 op_timeout 和一次 79 秒的
// dispatch 可以同时为真——同一个实体上排队的 N 个写者，最后一个要等前面 N-1 个各自跑完。
//
// 同一个包里的另一条路径（ownership.go 的加锁路径）**已经**是先设 deadline 再排队，
// 所以这不是两种合理设计的取舍，而是同一个判据在两条路径上不一致。
func TestAQueuedWriterIsBoundedByTheOperationBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OpTimeout = 150 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4242, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}

	// Somebody else holds the entity's write gate and is not letting go: a
	// slow commit, a distributed lock still being acquired, anything.
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	// The caller has no deadline of its own, which is the case the budget is
	// supposed to cover. Run it off the test goroutine: without the budget
	// this call never returns, and a hanging test says less than a failing
	// one.
	type result struct {
		err    error
		waited time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		_, err := wrapper.beginWrite(context.Background())
		done <- result{err: err, waited: time.Since(started)}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a writer got in while the gate was held")
		}
		if got.waited > 4*cfg.OpTimeout {
			t.Fatalf("a queued writer waited %s with an operation budget of %s; the budget does not cover the wait", got.waited, cfg.OpTimeout)
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("a writer that ran out of budget reported %v, want a deadline", got.err)
		}
	case <-time.After(4 * cfg.OpTimeout):
		t.Fatalf("a queued writer did not return within %s of an operation budget of %s; nothing bounds the wait", 4*cfg.OpTimeout, cfg.OpTimeout)
	}
}

// 排队本身不能把总耗时做成 N 倍：N 个写者各自都要在预算内回来。
func TestConcurrentWritersEachReturnWithinTheBudget(t *testing.T) {
	const writers = 8
	cfg := DefaultConfig()
	cfg.OpTimeout = 150 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4343, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	var wg sync.WaitGroup
	worst := make([]time.Duration, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			started := time.Now()
			_, _ = wrapper.beginWrite(context.Background())
			worst[slot] = time.Since(started)
		}(i)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(6 * cfg.OpTimeout):
		t.Fatalf("%d queued writers did not all return within %s of a %s budget", writers, 6*cfg.OpTimeout, cfg.OpTimeout)
	}
	for slot, waited := range worst {
		if waited > 4*cfg.OpTimeout {
			t.Fatalf("writer %d waited %s with a budget of %s; the wait grows with the queue", slot, waited, cfg.OpTimeout)
		}
	}
}

// 调用方自带的更短 deadline 仍然优先——预算是上界，不是下界。
func TestACallerDeadlineShorterThanTheBudgetStillWins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OpTimeout = 2 * time.Second
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4444, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := wrapper.beginWrite(ctx); err == nil {
		t.Fatal("a writer got in while the gate was held")
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the caller's 100ms deadline was ignored: waited %s", waited)
	}
}
