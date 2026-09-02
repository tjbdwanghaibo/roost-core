package lock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The promise of reentrancy is not "Lock twice does not deadlock" — the old
// test only proved that, and would have passed with a plain sync.Mutex
// replaced by a no-op. The promise is that the inner Unlock does NOT release
// the lock: recursion has to unwind fully before another goroutine gets in.
func TestReentrantMutexInnerUnlockDoesNotReleaseTheLock(t *testing.T) {
	mu := NewReentrantMutex(1)

	mu.Lock()
	mu.Lock() // reentrant: recursion 2
	mu.Unlock()

	// Still held: another goroutine must not be able to take it.
	if acquiredElsewhere(t, mu) {
		t.Fatal("inner Unlock released the lock; recursion must unwind to zero first")
	}

	mu.Unlock() // recursion 0 -> released
	if !acquiredElsewhere(t, mu) {
		t.Fatal("lock was not released after the outer Unlock")
	}
}

// acquiredElsewhere reports whether a different goroutine can take mu. It uses
// TryLock so the probe is bounded — a blocking Lock would turn a failure into
// a go test timeout with no message.
func acquiredElsewhere(t *testing.T, mu *ReentrantMutex) bool {
	t.Helper()
	result := make(chan bool, 1)
	go func() {
		if mu.TryLock() {
			mu.Unlock()
			result <- true
			return
		}
		result <- false
	}()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("TryLock from another goroutine never returned")
		return false
	}
}

// TryLock must be reentrant for the owner and refuse everyone else, since nest
// uses it to decide between taking a lock and deferring the whole dispatch.
func TestReentrantMutexTryLockIsReentrantForOwnerOnly(t *testing.T) {
	mu := NewReentrantMutex(1)
	mu.Lock()
	defer mu.Unlock()

	if !mu.TryLock() {
		t.Fatal("TryLock refused the goroutine that already owns the lock")
	}
	mu.Unlock() // undo the reentrant acquisition, still held once

	if acquiredElsewhere(t, mu) {
		t.Fatal("TryLock succeeded for a goroutine that does not own the lock")
	}
}

// Unlocking a mutex this goroutine does not hold is a programming error the
// framework deliberately turns into a panic rather than silent corruption of
// the lock-order bookkeeping.
func TestReentrantMutexUnlockWithoutOwnershipPanics(t *testing.T) {
	mu := NewReentrantMutex(1)

	// Never locked at all.
	assertPanics(t, "unlock of a never-locked mutex", func() { mu.Unlock() })

	// Locked, fully unlocked, then unlocked once more.
	mu.Lock()
	mu.Unlock()
	assertPanics(t, "unlock after the lock was already released", func() { mu.Unlock() })

	// Held by another goroutine.
	mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		assertPanics(t, "unlock from a non-owner goroutine", func() { mu.Unlock() })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("non-owner Unlock neither panicked nor returned")
	}
	mu.Unlock()
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
}

func TestReentrantMutex_TryLock(t *testing.T) {
	mu := NewReentrantMutex(2)

	if !mu.TryLock() {
		t.Fatal("TryLock should succeed")
	}
	// Reentrant TryLock
	if !mu.TryLock() {
		t.Fatal("Reentrant TryLock should succeed")
	}
	mu.Unlock()
	mu.Unlock()
}

func TestReentrantMutex_Contention(t *testing.T) {
	mu := NewReentrantMutex(3)
	var counter int64
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			atomic.AddInt64(&counter, 1)
		}()
	}
	wg.Wait()

	if counter != 100 {
		t.Fatalf("expected 100, got %d", counter)
	}
}

func TestReentrantMutex_LockWithTimeout(t *testing.T) {
	mu := NewReentrantMutex(4)
	mu.Lock()

	done := make(chan bool, 1)
	go func() {
		// Should timeout since lock is held by another goroutine
		ok := mu.LockWithTimeout(50 * time.Millisecond)
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("LockWithTimeout should have failed")
		}
	case <-time.After(time.Second):
		t.Fatal("LockWithTimeout did not return in time")
	}

	mu.Unlock()
}

func TestReentrantMutex_LockId(t *testing.T) {
	mu := NewReentrantMutex(42)
	if mu.LockId() != 42 {
		t.Fatalf("expected LockId 42, got %d", mu.LockId())
	}
}

func TestDefaultMutex(t *testing.T) {
	mu := NewDefaultMutex(99)
	if mu.LockId() != 99 {
		t.Fatalf("expected LockId 99, got %d", mu.LockId())
	}
	mu.Lock()
	mu.Unlock()

	if !mu.TryLock() {
		t.Fatal("TryLock should succeed")
	}
	mu.Unlock()
}

func TestLockManager_GetLock(t *testing.T) {
	mgr := NewLockManager(nil)

	mu1 := mgr.GetLock(100)
	if mu1 == nil {
		t.Fatal("GetLock should return non-nil")
	}
	if mu1.LockId() != 100 {
		t.Fatalf("expected LockId 100, got %d", mu1.LockId())
	}

	// Same ID returns same lock
	mu2 := mgr.GetLock(100)
	if mu1 != mu2 {
		t.Fatal("GetLock should return same instance for same ID")
	}

	// Different ID returns different lock
	mu3 := mgr.GetLock(200)
	if mu1 == mu3 {
		t.Fatal("different IDs should return different locks")
	}
}

func TestLockManager_ReleaseLock(t *testing.T) {
	mgr := NewLockManager(nil)

	mu1 := mgr.GetLock(300)
	mgr.ReleaseLock(300)

	// After release, a new lock instance is created
	mu2 := mgr.GetLock(300)
	if mu1 == mu2 {
		t.Fatal("after release, GetLock should return a new instance")
	}
}

func TestReentrantMutex_LockWithTimeout_SucceedsAfterRelease(t *testing.T) {
	mu := NewReentrantMutex(5)
	mu.Lock()

	done := make(chan bool, 1)
	go func() {
		done <- mu.LockWithTimeout(time.Second)
	}()

	time.Sleep(20 * time.Millisecond)
	mu.Unlock()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("LockWithTimeout should succeed once the lock is released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LockWithTimeout did not return in time")
	}
}

func TestReentrantMutex_UnlockByNonOwnerPanics(t *testing.T) {
	mu := NewReentrantMutex(6)
	mu.Lock()
	defer mu.Unlock()

	done := make(chan bool, 1)
	go func() {
		defer func() {
			done <- recover() != nil
		}()
		mu.Unlock()
	}()

	if !<-done {
		t.Fatal("Unlock by non-owner should panic")
	}
}

func TestDefaultMutex_LockWithTimeout(t *testing.T) {
	mu := NewDefaultMutex(7)
	mu.Lock()

	// Held by this goroutine: a bounded wait must fail instead of blocking.
	start := time.Now()
	if mu.LockWithTimeout(50 * time.Millisecond) {
		t.Fatal("LockWithTimeout should time out while the lock is held")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("LockWithTimeout blocked too long: %v", elapsed)
	}

	mu.Unlock()
	if !mu.LockWithTimeout(50 * time.Millisecond) {
		t.Fatal("LockWithTimeout should succeed on a free lock")
	}
	mu.Unlock()
}

func TestLockManager_CustomFactory(t *testing.T) {
	var callCount atomic.Int32
	factory := func(id int64) Mutex {
		callCount.Add(1)
		return NewDefaultMutex(id)
	}

	mgr := NewLockManager(factory)
	mgr.GetLock(1)
	mgr.GetLock(2)

	if callCount.Load() != 2 {
		t.Fatalf("expected factory called 2 times, got %d", callCount.Load())
	}
}
