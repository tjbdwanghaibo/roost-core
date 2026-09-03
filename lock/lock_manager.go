package lock

import (
	"github.com/tjbdwanghaibo/roost-core/container"
)

const defaultBucketCnt = 64

// MutexFactory creates a Mutex for the given lock ID.
// Application layer can override to provide custom lock implementations (e.g., distributed locks).
type MutexFactory func(id int64) Mutex

// LockManager manages a sharded pool of Mutex instances by entity ID.
type LockManager struct {
	locks   *container.BucketHolder[int64, Mutex]
	factory MutexFactory
}

// NewLockManager creates a lock manager with the given factory.
// If factory is nil, defaults to NewReentrantMutex.
func NewLockManager(factory MutexFactory) *LockManager {
	if factory == nil {
		factory = func(id int64) Mutex {
			return NewReentrantMutex(id)
		}
	}
	mgr := &LockManager{
		factory: factory,
	}
	mgr.locks = container.NewBucketHolder[int64, Mutex](defaultBucketCnt, mgr.factory, true)
	return mgr
}

// GetLock returns the Mutex for the given entity ID, creating one if necessary.
//
// A Mutex obtained here may be released from the manager concurrently (see
// ReleaseLock), so acquiring it proves nothing by itself: callers must
// revalidate the guarded state after Lock (e.g. entity IsRemoved/IsClear and
// index membership) and retreat if the state is gone. That revalidation is
// what makes holding a stale, released instance harmless.
func (m *LockManager) GetLock(id int64) Mutex {
	return m.locks.Get(id)
}

// ReleaseLock removes the lock for the given ID from the manager.
//
// After removal a subsequent GetLock returns a fresh instance while late
// waiters may still acquire the stale one, so two goroutines can each hold "a
// lock for id" at once. This is only safe under the GetLock contract above:
// the caller must first make the guarded state unreachable (removed from the
// index, marked removed) while holding the lock, so any stale-lock winner
// revalidates, observes the removal, and retreats. Do not call this for ids
// whose readers do not revalidate.
func (m *LockManager) ReleaseLock(id int64) {
	m.locks.Del(id)
}
