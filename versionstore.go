// Package versionstore is the contract for state whose every mutation is
// version-checked, plus a Redis and an in-memory implementation.
//
// It exists because "read, decide, write" is the single most productive source
// of defects in the service layer, and because making the check optional makes
// it useless: given a contract that also offers an unconditional Set, some
// implementation will satisfy the interface without the check and no compiler
// will notice. So there is no unconditional write here. Update is the only way
// to change a value, it performs the compare-and-set retry itself, and a
// caller cannot express "write regardless".
//
// Three rules follow from defects found in production service code:
//
//   - The comparison is on the version, never on the value. Comparing a
//     re-marshalled value against stored bytes wedges every write the moment a
//     rolling deploy changes the struct's serialization.
//   - Versions are assigned by the store and are monotonic per key, so they
//     stay comparable across replicas and across restarts. A process-local
//     counter is not a version.
//   - Version zero means "absent", not "skip the check". A stored value always
//     has Version >= 1, so no code path can opt out of the comparison by
//     leaving a field at its zero value.
package versionstore

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrConflict reports that Update gave up after MaxAttempts losses. It is
	// distinguishable from a store failure on purpose: contention and an
	// unreachable backend need different operational responses.
	ErrConflict = errors.New("versionstore: version conflict")

	// ErrVersionMismatch reports that a Delete was refused because the caller
	// held a different version than the store.
	ErrVersionMismatch = errors.New("versionstore: version mismatch")

	// ErrAborted is returned by Update when the mutate function asks not to
	// save and there was nothing to return.
	ErrAborted = errors.New("versionstore: update aborted")

	ErrCodecNil = errors.New("versionstore: codec is nil")
	ErrKeyEmpty = errors.New("versionstore: key is empty")
)

// Versioned pairs a value with the version the store assigned it. Version is
// zero only for a value that is not stored.
type Versioned[T any] struct {
	Value   T
	Version uint64
}

// Mutate computes the next value from the current one. found reports whether
// the key exists; when it does not, current is the zero value.
//
// Mutate may be called more than once — every retry re-reads and re-applies
// it — so it must be a pure function of its arguments. Returning save == false
// leaves the store untouched.
type Mutate[T any] func(current T, found bool) (next T, save bool, err error)

// Store is versioned state. Note what is absent: there is no Set.
type Store[K comparable, T any] interface {
	// Get returns the stored value and its version.
	Get(ctx context.Context, key K) (Versioned[T], bool, error)

	// Update applies mutate under a compare-and-set, retrying on conflict.
	// It returns the value that was written and its new version; applied is
	// false when mutate declined to save.
	Update(ctx context.Context, key K, mutate Mutate[T]) (result Versioned[T], applied bool, err error)

	// Create stores a value only if the key is absent, and reports whether it
	// did. It is separate from Update because "must not overwrite" is a
	// different intent than "compute from current", and expressing it through
	// Update would rely on the caller checking found — which is exactly the
	// check that gets forgotten.
	Create(ctx context.Context, key K, value T) (Versioned[T], bool, error)

	// Delete removes the key only if the caller's version still matches.
	Delete(ctx context.Context, key K, expect Versioned[T]) error
}

// Codec converts a value to and from the bytes the store persists. The version
// is framed by the store, not by the codec, so a codec change cannot affect
// version comparison.
type Codec[T any] interface {
	Encode(T) ([]byte, error)
	Decode([]byte) (T, error)
}

// KeyFunc renders a key into the string namespace the backend uses.
type KeyFunc[K comparable] func(K) string

const (
	// DefaultMaxAttempts bounds the compare-and-set retry loop. It is a bound,
	// not a target: exceeding it means real contention on one key and is
	// reported as ErrConflict rather than retried forever.
	DefaultMaxAttempts = 8

	// DefaultRetryBackoff is the base delay between compare-and-set attempts.
	// Small enough not to matter for an uncontended write, large enough that
	// concurrent writers on one key spread out instead of exhausting the
	// budget together.
	DefaultRetryBackoff = 2 * time.Millisecond
)
