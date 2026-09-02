package lock

import (
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/goroutine"
)

var _ Mutex = (*ReentrantMutex)(nil)

// ReentrantMutex is an in-process reentrant mutual exclusion lock.
// It allows the same goroutine to acquire the lock multiple times without deadlock.
//
// Waiters park on a capacity-1 semaphore channel instead of spinning, so a
// holder performing slow work (e.g. a WAL commit at the transaction commit
// point) does not burn CPU on every waiting goroutine. Channel receive order
// also gives waiters FIFO-ish fairness, so a hot entity lock cannot starve
// an individual waiter the way an unfair spin loop can.
//
// Invariants:
//   - The token is in sem exactly when the lock is free.
//   - owner is non-zero exactly while some goroutine holds the lock, and is
//     only ever compared against the caller's own goroutine ID, so a stale
//     read can never satisfy the reentrancy fast path for a non-holder.
//   - recursion is touched only by the holder; the sem handoff provides the
//     happens-before edge between successive holders.
type ReentrantMutex struct {
	sem       chan struct{} // capacity 1; holds the token while the lock is free
	owner     atomic.Int64  // goroutine ID of lock holder, 0 when free
	recursion int32         // reentry count, owned by the holder
	id        int64
}

// NewReentrantMutex creates a new reentrant mutex.
func NewReentrantMutex(ids ...int64) *ReentrantMutex {
	var id int64
	if len(ids) > 0 {
		id = ids[0]
	}
	rm := &ReentrantMutex{
		sem: make(chan struct{}, 1),
		id:  id,
	}
	rm.sem <- struct{}{}
	return rm
}

func (rm *ReentrantMutex) LockId() int64 {
	return rm.id
}

// Lock acquires the lock. The same goroutine can call this multiple times.
func (rm *ReentrantMutex) Lock() {
	gid := goroutine.GoID()
	if rm.owner.Load() == gid {
		rm.recursion++
		return
	}

	<-rm.sem
	rm.owner.Store(gid)
	rm.recursion = 1
}

// Unlock releases the lock.
func (rm *ReentrantMutex) Unlock() {
	gid := goroutine.GoID()
	if rm.owner.Load() != gid {
		panic("unlock of unowned mutex")
	}

	rm.recursion--
	if rm.recursion < 0 {
		panic("unlock of unlocked mutex")
	}

	if rm.recursion == 0 {
		rm.owner.Store(0)
		rm.sem <- struct{}{}
	}
}

// TryLock attempts to acquire the lock without blocking.
func (rm *ReentrantMutex) TryLock() bool {
	gid := goroutine.GoID()
	if rm.owner.Load() == gid {
		rm.recursion++
		return true
	}

	select {
	case <-rm.sem:
		rm.owner.Store(gid)
		rm.recursion = 1
		return true
	default:
		return false
	}
}

// LockWithTimeout acquires the lock with a timeout. Returns false if timeout expires.
func (rm *ReentrantMutex) LockWithTimeout(timeout time.Duration) bool {
	if timeout <= 0 {
		return rm.TryLock()
	}

	gid := goroutine.GoID()
	if rm.owner.Load() == gid {
		rm.recursion++
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-rm.sem:
		rm.owner.Store(gid)
		rm.recursion = 1
		return true
	case <-timer.C:
		return false
	}
}
