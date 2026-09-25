package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

type delayedUnlockReply struct {
	*unlockEvalStub
	calls            atomic.Int32
	entered, release chan struct{}
	beforeApply      bool
}

func (s *delayedUnlockReply) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedUnlockLua || s.calls.Add(1) != 1 {
		return s.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
	if s.beforeApply {
		close(s.entered)
		<-s.release
		return s.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
	result, err := s.unlockEvalStub.Eval(ctx, script, keys, args...)
	close(s.entered)
	<-s.release
	return result, err
}
func TestDelayedUnlockCannotClearReacquiredGeneration(t *testing.T) {
	for _, before := range []bool{false, true} {
		name := "successful old reply"
		if before {
			name = "rejected old reply"
		}
		t.Run(name, func(t *testing.T) {
			stub := &delayedUnlockReply{unlockEvalStub: newUnlockEvalStub(), entered: make(chan struct{}), release: make(chan struct{}), beforeApply: before}
			lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
			if err := lock.TryLock(context.Background()); err != nil {
				t.Fatal(err)
			}
			old := make(chan error, 1)
			go func() { old <- lock.Unlock(context.Background(), 7, time.Second) }()
			awaitChan(t, stub.entered, "first unlock")
			err := lock.Unlock(context.Background(), 9, time.Second)
			if before && err != nil || !before && !errors.Is(err, ErrVersionedLockNotOwned) {
				t.Fatalf("second unlock=%v", err)
			}
			if err := lock.TryLock(context.Background()); err != nil {
				t.Fatal(err)
			}
			version, fence := lock.Version(), lock.Fence()
			close(stub.release)
			err = awaitChan(t, old, "old reply")
			if before && !errors.Is(err, ErrVersionedLockNotOwned) || !before && err != nil {
				t.Fatalf("old unlock=%v", err)
			}
			if !lock.IsAcquired() || lock.Version() != version || lock.Fence() != fence {
				t.Fatalf("old reply changed new generation: held=%v version=%d fence=%d", lock.IsAcquired(), lock.Version(), lock.Fence())
			}
			if err := lock.Unlock(context.Background(), 10, time.Second); err != nil {
				t.Fatalf("new generation unlock=%v", err)
			}
		})
	}
}
