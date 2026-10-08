package lock

import (
	"context"
	"time"
)

// LockContext 在原有Mutex契约上增加可取消等待，不为等待者创建goroutine。
// 内建锁直接等待信号量；外部旧实现沿用其有界LockWithTimeout契约。
// nil表示已取得一层锁，调用者负责Unlock；取消与取得同时发生时归还这一层。
func LockContext(ctx context.Context, mu Mutex) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if native, ok := mu.(interface{ LockContext(context.Context) error }); ok {
		if err := native.LockContext(ctx); err != nil {
			return err
		}
	} else {
		for !mu.TryLock() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if mu.LockWithTimeout(10 * time.Millisecond) {
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		mu.Unlock()
		return err
	}
	return nil
}

func takeToken(ctx context.Context, sem chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-sem:
		if err := ctx.Err(); err != nil {
			sem <- struct{}{}
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (d *defaultMutex) LockContext(ctx context.Context) error {
	return takeToken(ctx, d.sem)
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
