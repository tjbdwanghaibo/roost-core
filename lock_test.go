package redis

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/cube-core/redis"
)

// fakeDistLock counts extensions and can start refusing them, modeling a
// lease that expired or was taken over.
type fakeDistLock struct {
	mu       sync.Mutex
	held     bool
	extends  int
	extendOK bool
}

func (f *fakeDistLock) Acquire(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held {
		return false, nil
	}
	f.held = true
	f.extendOK = true
	return true, nil
}

func (f *fakeDistLock) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.held {
		return fredis.ErrLockNotHeld
	}
	f.held = false
	return nil
}

func (f *fakeDistLock) Extend(context.Context, time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends++
	return f.extendOK, nil
}

func (f *fakeDistLock) extendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extends
}

func (f *fakeDistLock) expireLease() {
	f.mu.Lock()
	f.extendOK = false
	f.held = false
	f.mu.Unlock()
}

func TestAutoExtendLockKeepsLeaseAliveUntilRelease(t *testing.T) {
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, 30*time.Millisecond, 5*time.Millisecond)
	ok, err := lock.Acquire(context.Background())
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for inner.extendCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog extended only %d times", inner.extendCount())
		}
		time.Sleep(time.Millisecond)
	}
	if lock.Err() != nil {
		t.Fatalf("healthy lease reported loss: %v", lock.Err())
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Release stops the watchdog: the extend count settles.
	settled := inner.extendCount()
	time.Sleep(30 * time.Millisecond)
	if got := inner.extendCount(); got != settled {
		t.Fatalf("watchdog kept extending after release: %d -> %d", settled, got)
	}
}

func TestAutoExtendLockSurfacesLostLease(t *testing.T) {
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, 30*time.Millisecond, 5*time.Millisecond)
	if ok, err := lock.Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	inner.expireLease()
	deadline := time.Now().Add(2 * time.Second)
	for lock.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("lost lease was never surfaced")
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(lock.Err(), fredis.ErrLockNotHeld) {
		t.Fatalf("err=%v, want ErrLockNotHeld cause", lock.Err())
	}
	// Release after loss reports the inner state honestly.
	if err := lock.Release(context.Background()); !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("release after loss err=%v", err)
	}
}
