package redis

import (
	"context"
	"time"
)

// IDistLock is a Redis-based distributed lock for mutual exclusion with
// best-effort semantics: single-instance SetNX, no fencing token, and no
// automatic lease extension (callers own Extend). The lease can expire while
// work is still in progress, and a stale holder cannot be fenced out of
// downstream writes.
//
// Use it only where a duplicate run is tolerable or the protected operation
// is idempotent (cron-style dedup, cache warmers, best-effort singletons).
// Anything that must prevent a stale holder from writing — entity ownership,
// storage commits — must use IVersionedLock, whose fence token makes writes
// verifiable. cube-kit provides an auto-extending wrapper for long-running
// holders of this interface.
type IDistLock interface {
	// Acquire attempts to acquire the lock. Returns true if acquired.
	Acquire(ctx context.Context) (bool, error)

	// Release releases the lock. Returns ErrLockNotHeld if not held.
	Release(ctx context.Context) error

	// Extend extends the lock TTL (for long-running operations).
	Extend(ctx context.Context, ttl time.Duration) (bool, error)
}

// IDistLockFactory creates distributed locks by key.
type IDistLockFactory interface {
	// NewLock creates a distributed lock for the given key with the specified TTL.
	NewLock(key string, ttl time.Duration) IDistLock
}
