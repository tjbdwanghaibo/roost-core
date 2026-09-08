package redis

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"

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

// Contract boundary: the locks in this file implement fredis.IDistLock —
// value-guarded mutual exclusion WITHOUT a fencing token. A holder whose TTL
// expires (GC pause, network stall) keeps executing without knowing the lock
// is gone, so a second holder can run concurrently for that window. Use them
// only where a double-execution is tolerable (cache warm-up, dedup-able
// jobs, optimization-grade exclusivity). For correctness-grade exclusivity —
// entity ownership, anything a store must be able to reject stale writers
// for — use remote_entity's versionedLock (fence counter outlives the TTL,
// stores compare fences) or etcd's IFencedElection.
func newDistLockFactory(rdb goredis.UniversalClient) *distLockFactory {
	return &distLockFactory{rdb: rdb}
}

func (f *distLockFactory) NewLock(key string, ttl time.Duration) fredis.IDistLock {
	if f == nil {
		return &distLock{key: key, ttl: ttl}
	}
	return &distLock{rdb: f.rdb, key: key, ttl: ttl}
}

var _ fredis.IDistLockFactory = (*distLockFactory)(nil)

// distLock implements fredis.IDistLock.
type distLock struct {
	rdb goredis.UniversalClient
	key string
	ttl time.Duration

	mu    sync.Mutex
	value string // unique per-acquisition owner identity
	state distLockState
}

type distLockState uint8

const (
	distLockIdle distLockState = iota
	distLockAcquired
	distLockUncertain
)

var (
	// ErrDistLockConfig reports a nil client, empty key or invalid TTL.
	ErrDistLockConfig = errors.New("redis: invalid distributed lock configuration")
	// ErrDistLockAlreadyActive reports a repeated Acquire on one active object.
	ErrDistLockAlreadyActive = errors.New("redis: distributed lock already active")
	// ErrDistLockStateUncertain requires a value-guarded Release before reuse.
	ErrDistLockStateUncertain = errors.New("redis: distributed lock ownership is uncertain; call Release to reconcile")
)

func (l *distLock) Acquire(ctx context.Context) (bool, error) {
	if l == nil || l.rdb == nil || l.key == "" || l.ttl < time.Millisecond {
		return false, ErrDistLockConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case distLockAcquired:
		return false, ErrDistLockAlreadyActive
	case distLockUncertain:
		return false, ErrDistLockStateUncertain
	}
	value := generateLockValue()
	ok, err := l.rdb.SetNX(ctx, l.key, value, l.ttl).Result()
	if err != nil {
		// SetNX may have reached Redis even when the reply is lost. Preserve the
		// token so Release can reconcile with a value-guarded delete.
		l.value = value
		l.state = distLockUncertain
		return false, err
	}
	if !ok {
		return false, nil
	}
	l.value = value
	l.state = distLockAcquired
	return true, nil
}

func (l *distLock) Release(ctx context.Context) error {
	if l == nil || l.rdb == nil {
		return ErrDistLockConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == distLockIdle || l.value == "" {
		return fredis.ErrLockNotHeld
	}
	result, err := l.rdb.Eval(ctx, releaseLockScript, []string{l.key}, l.value).Int64()
	if err != nil {
		l.state = distLockUncertain
		return err
	}
	l.state = distLockIdle
	l.value = ""
	if result == 0 {
		return fredis.ErrLockNotHeld
	}
	return nil
}

func (l *distLock) Extend(ctx context.Context, ttl time.Duration) (bool, error) {
	if l == nil || l.rdb == nil || ttl < time.Millisecond {
		return false, ErrDistLockConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == distLockIdle || l.value == "" {
		return false, fredis.ErrLockNotHeld
	}
	result, err := l.rdb.Eval(ctx, extendLockScript, []string{l.key}, l.value, ttl.Milliseconds()).Int64()
	if err != nil {
		l.state = distLockUncertain
		return false, err
	}
	if result != 1 {
		l.state = distLockIdle
		l.value = ""
		return false, nil
	}
	l.state = distLockAcquired
	return true, nil
}

var _ fredis.IDistLock = (*distLock)(nil)

func generateLockValue() string {
	return rand.Text()
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
	opMu     sync.Mutex

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	lostErr error
	initErr error
}

// NewAutoExtendLock wraps lock, which must have been created with the given
// ttl. extendInterval <= 0 defaults to ttl/3.
func NewAutoExtendLock(lock fredis.IDistLock, ttl time.Duration, extendInterval time.Duration) *AutoExtendLock {
	var initErr error
	if lock == nil {
		initErr = errors.Join(initErr, fmt.Errorf("redis: auto extend lock is nil"))
	}
	if ttl < time.Millisecond {
		initErr = errors.Join(initErr, fmt.Errorf("redis: auto extend TTL must be at least 1ms"))
	}
	if extendInterval <= 0 {
		extendInterval = ttl / 3
	}
	if extendInterval <= 0 {
		extendInterval = time.Second
	}
	if ttl >= time.Millisecond && extendInterval >= ttl {
		initErr = errors.Join(initErr, fmt.Errorf("redis: auto extend interval must be shorter than TTL"))
	}
	return &AutoExtendLock{inner: lock, ttl: ttl, interval: extendInterval, initErr: initErr}
}

func (l *AutoExtendLock) Acquire(ctx context.Context) (bool, error) {
	if l == nil || l.initErr != nil {
		if l == nil {
			return false, fmt.Errorf("redis: auto extend lock is nil")
		}
		return false, l.initErr
	}
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.mu.Lock()
	if l.cancel != nil {
		l.mu.Unlock()
		return false, ErrDistLockAlreadyActive
	}
	l.mu.Unlock()
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
	lastRenewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Bound every extension attempt: an unbounded call against a hung
		// Redis would freeze the watchdog while the lease silently expired,
		// leaving Err() reporting a healthy lock.
		extendCtx, cancel := context.WithTimeout(ctx, l.interval)
		extended, err := l.inner.Extend(extendCtx, l.ttl)
		cancel()
		if err == nil && extended {
			lastRenewed = time.Now()
			continue
		}
		if err == nil {
			// Authoritative server answer: the lease is not held anymore.
			// Stop extending and make the loss observable instead of
			// fighting the new holder.
			l.recordLost(fredis.ErrLockNotHeld)
			return
		}
		// Transient failure (network, timeout): the last successful renewal
		// may still be covering the lease, so keep retrying on the ticker.
		// Only when the renewal gap exceeds the TTL has the lease provably
		// expired.
		if time.Since(lastRenewed) >= l.ttl {
			l.recordLost(err)
			return
		}
	}
}

func (l *AutoExtendLock) recordLost(cause error) {
	l.mu.Lock()
	l.lostErr = fmt.Errorf("redis: auto extend lost lock: %w", cause)
	l.mu.Unlock()
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
	if l == nil || l.initErr != nil {
		if l == nil {
			return fmt.Errorf("redis: auto extend lock is nil")
		}
		return l.initErr
	}
	l.opMu.Lock()
	defer l.opMu.Unlock()
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
	if l == nil || l.initErr != nil {
		if l == nil {
			return false, fmt.Errorf("redis: auto extend lock is nil")
		}
		return false, l.initErr
	}
	l.opMu.Lock()
	defer l.opMu.Unlock()
	return l.inner.Extend(ctx, ttl)
}

var _ fredis.IDistLock = (*AutoExtendLock)(nil)
