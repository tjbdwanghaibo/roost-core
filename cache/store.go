package cache

import (
	"context"
	"errors"
)

var (
	ErrKeyFuncNil = errors.New("cache: key func is nil")
	ErrInvalidKey = errors.New("cache: invalid key")

	// ErrStaleWrite reports that a Set was refused because the store's
	// StaleFunc judged the incoming value older than the stored one. The
	// write did not happen.
	//
	// It is returned rather than swallowed because a caller cannot otherwise
	// tell a completed write from a dropped one: reporting success for a
	// mutation that was silently discarded is how a cancel comes back OK
	// while the store still holds the old state. Callers for which losing to
	// a newer value is the intended outcome tolerate it explicitly with
	// errors.Is.
	ErrStaleWrite = errors.New("cache: write refused as stale")
	// ErrConflictingWrite reports a write that claims an existing version with
	// a different value. Unlike ErrStaleWrite it is never silently absorbed: a
	// stale write is merely late, a conflicting write is wrong.
	ErrConflictingWrite = errors.New("cache: conflicting write for the same version")
)

type Store[K comparable, V any] interface {
	Get(ctx context.Context, key K) (V, bool, error)
	Set(ctx context.Context, value V) error
	Delete(ctx context.Context, key K) error
}

type KeyFunc[K comparable, V any] func(V) K

// StaleFunc reports whether next is older than old and must not replace it.
// A Set whose value is judged stale returns ErrStaleWrite.
//
// How much concurrency control this gives depends on the store:
//
//   - AtomicLocalStore evaluates the predicate under the shard lock, so the
//     comparison and the write are atomic.
//   - RedisJSONStore and RefHMapStore read the current value and write in
//     separate round trips, so the predicate is **advisory only**: two
//     writers at the same version both pass it. Use a versioned
//     compare-and-set (redis.CompareAndSet) when correctness depends on the
//     loser being rejected.
type StaleFunc[V any] func(old V, next V) bool
type ValidateKeyFunc[K comparable] func(K) bool
type ValidateValueFunc[V any] func(V) error

type StoreConfig[K comparable, V any] struct {
	KeyOf         KeyFunc[K, V]
	Stale         StaleFunc[V]
	ValidateKey   ValidateKeyFunc[K]
	ValidateValue ValidateValueFunc[V]
	// Conflict reports that next claims the SAME version as old but is a
	// different value — a consistency violation, not an ordering question.
	// Stale answers "is next older"; Conflict answers "is next a lie about
	// this version". Checked under the same lock as Stale, so every path
	// that writes the local store — publish, loader fill, L2 backfill —
	// enforces one rule (RR-20260913-06). Optional.
	Conflict func(old, next V) bool
}

func (c StoreConfig[K, V]) keyOf(value V) (K, error) {
	var zero K
	if c.KeyOf == nil {
		return zero, ErrKeyFuncNil
	}
	key := c.KeyOf(value)
	if c.ValidateKey != nil && !c.ValidateKey(key) {
		return zero, ErrInvalidKey
	}
	return key, nil
}

func (c StoreConfig[K, V]) validKey(key K) bool {
	return c.ValidateKey == nil || c.ValidateKey(key)
}

func VersionStale[V any](versionOf func(V) int64) StaleFunc[V] {
	return func(old V, next V) bool {
		if versionOf == nil {
			return false
		}
		return versionOf(old) > versionOf(next)
	}
}
