package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	fredis "github.com/tjbdwanghaibo/cube-core/redis"

	goredis "github.com/redis/go-redis/v9"
)

// Lua scripts for atomic lock operations
const (
	releaseLockScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end`

	extendLockScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
else
    return 0
end`
)

// distLockFactory implements fredis.IDistLockFactory.
type distLockFactory struct {
	rdb goredis.UniversalClient
}

func newDistLockFactory(rdb goredis.UniversalClient) *distLockFactory {
	return &distLockFactory{rdb: rdb}
}

func (f *distLockFactory) NewLock(key string, ttl time.Duration) fredis.IDistLock {
	return &distLock{
		rdb:   f.rdb,
		key:   key,
		ttl:   ttl,
		value: generateLockValue(),
	}
}

var _ fredis.IDistLockFactory = (*distLockFactory)(nil)

// distLock implements fredis.IDistLock.
type distLock struct {
	rdb   goredis.UniversalClient
	key   string
	ttl   time.Duration
	value string // unique value to identify lock owner
}

func (l *distLock) Acquire(ctx context.Context) (bool, error) {
	ok, err := l.rdb.SetNX(ctx, l.key, l.value, l.ttl).Result()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return true, nil
}

func (l *distLock) Release(ctx context.Context) error {
	result, err := l.rdb.Eval(ctx, releaseLockScript, []string{l.key}, l.value).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return fredis.ErrLockNotHeld
	}
	return nil
}

func (l *distLock) Extend(ctx context.Context, ttl time.Duration) (bool, error) {
	result, err := l.rdb.Eval(ctx, extendLockScript, []string{l.key}, l.value, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

var _ fredis.IDistLock = (*distLock)(nil)

func generateLockValue() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AutoExtendLock wraps an IDistLock with a keep-alive watchdog: after a
// successful Acquire it re-extends the lease to the full TTL on a fraction of
// the TTL, so the lock cannot silently expire under a long-running holder. A
// crashed holder still releases naturally within one TTL because the watchdog
// dies with the process.
//
// This does NOT add fencing: a holder that loses the lease (reported by Err)
// can still race a new holder's downstream writes. Operations that must fence
// stale holders belong on IVersionedLock instead.
type AutoExtendLock struct {
	inner    fredis.IDistLock
	ttl      time.Duration
	interval time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	lostErr error
}

// NewAutoExtendLock wraps lock, which must have been created with the given
// ttl. extendInterval <= 0 defaults to ttl/3.
func NewAutoExtendLock(lock fredis.IDistLock, ttl time.Duration, extendInterval time.Duration) *AutoExtendLock {
	if extendInterval <= 0 {
		extendInterval = ttl / 3
	}
	if extendInterval <= 0 {
		extendInterval = time.Second
	}
	return &AutoExtendLock{inner: lock, ttl: ttl, interval: extendInterval}
}

func (l *AutoExtendLock) Acquire(ctx context.Context) (bool, error) {
	ok, err := l.inner.Acquire(ctx)
	if err != nil || !ok {
		return ok, err
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l.mu.Lock()
	l.cancel = cancel
	l.done = done
	l.lostErr = nil
	l.mu.Unlock()
	go l.watch(watchCtx, done)
	return true, nil
}

func (l *AutoExtendLock) watch(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		extended, err := l.inner.Extend(ctx, l.ttl)
		if err == nil && extended {
			continue
		}
		if err == nil {
			err = fredis.ErrLockNotHeld
		}
		// The lease is gone (expired or taken over): stop extending and make
		// the loss observable instead of fighting the new holder.
		l.mu.Lock()
		l.lostErr = fmt.Errorf("redis: auto extend lost lock: %w", err)
		l.mu.Unlock()
		return
	}
}

// Err reports whether the watchdog lost the lease since the last Acquire.
// Holders of long critical sections should check it before externalizing
// results that assume exclusivity.
func (l *AutoExtendLock) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lostErr
}

func (l *AutoExtendLock) Release(ctx context.Context) error {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.cancel, l.done = nil, nil
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return l.inner.Release(ctx)
}

func (l *AutoExtendLock) Extend(ctx context.Context, ttl time.Duration) (bool, error) {
	return l.inner.Extend(ctx, ttl)
}

var _ fredis.IDistLock = (*AutoExtendLock)(nil)
