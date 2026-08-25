package lock

import (
	"time"
)

// Mutex is the lock interface used by entity system.
// Implementations include ReentrantMutex (local) and distributed locks (app-layer).
type Mutex interface {
	TryLock() bool
	Lock()
	LockWithTimeout(timeout time.Duration) bool
	Unlock()
	LockIdGetter
}

// LockIdGetter provides lock identity.
type LockIdGetter interface {
	LockId() int64
}

// defaultMutex is a simple non-reentrant mutex built on a capacity-1
// semaphore channel so LockWithTimeout can honor its contract: waiters park
// on the channel and a timer bounds the wait. The token is in sem exactly
// when the lock is free.
var _ Mutex = (*defaultMutex)(nil)

type defaultMutex struct {
	sem chan struct{}
	id  int64
}

func (d *defaultMutex) Lock() {
	<-d.sem
}

func (d *defaultMutex) Unlock() {
	select {
	case d.sem <- struct{}{}:
	default:
		panic("unlock of unlocked mutex")
	}
}

func (d *defaultMutex) TryLock() bool {
	select {
	case <-d.sem:
		return true
	default:
		return false
	}
}

func (d *defaultMutex) LockWithTimeout(timeout time.Duration) bool {
	if timeout <= 0 {
		return d.TryLock()
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-d.sem:
		return true
	case <-timer.C:
		return false
	}
}

func (d *defaultMutex) LockId() int64 {
	return d.id
}

// NewDefaultMutex creates a simple non-reentrant mutex satisfying the Mutex interface.
func NewDefaultMutex(id int64) Mutex {
	d := &defaultMutex{
		sem: make(chan struct{}, 1),
		id:  id,
	}
	d.sem <- struct{}{}
	return d
}
