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
	"math/rand"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
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
	// ErrMalformedRecord marks a stored record that cannot be read back.
	//
	// It exists so a caller can tell this apart from an infrastructure
	// failure, because the two need opposite responses: a timeout is worth
	// retrying immediately, while a record whose bytes do not decode will
	// fail the same way forever. A retry loop that cannot tell them apart
	// either gives up on transient errors or lets one unreadable record
	// occupy the head of its queue for good (RR-20260920-05).
	ErrMalformedRecord = errors.New("versionstore: record cannot be decoded")
	ErrKeyEmpty        = errors.New("versionstore: key is empty")
)

// Versioned pairs a value with the version the store assigned it. Version is
// zero only for a value that is not stored.
type Versioned[T any] struct {
	Value   T
	Version uint64
}

// ConditionalDeleter additionally fences deletion by logical identity. A
// version can recur after deletion/recreation; match must be pure and is
// checked against the very value atomically removed, not a caller's old read.
type ConditionalDeleter[K comparable, T any] interface {
	DeleteIf(ctx context.Context, key K, expect Versioned[T], match func(T) bool) error
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
	//
	// A transport error (connection reset, timeout) leaves the outcome open:
	// the write may have landed. Every write carries a one-time token stored
	// with the value (A2 ③), so the Redis store settles a lost reply itself:
	// it reads the key and returns the write's own result when its token is
	// there, resends only when the key proves the command has not run, and
	// treats it as a lost race when someone else's write took that version. It
	// never blindly replays a write (RR-20261005-NC-100). What it cannot prove
	// comes back as ErrOutcomeUnknown; pass that error to Resume so the next
	// call checks the token instead of re-applying mutate. Without Resume a
	// retry re-applies mutate to whatever is stored, so it still needs an
	// idempotent mutate — typically a request id recorded inside the value.
	Update(ctx context.Context, key K, mutate Mutate[T]) (result Versioned[T], applied bool, err error)

	// Create stores a value only if the key is absent, and reports whether it
	// did. It is separate from Update because "must not overwrite" is a
	// different intent than "compute from current", and expressing it through
	// Update would rely on the caller checking found — which is exactly the
	// check that gets forgotten.
	//
	// A lost reply is settled by the write's token: a Create that landed
	// reports created, never "already taken". One that cannot be proven — the
	// key is still absent, which is also what "ran, then deleted by someone"
	// looks like — comes back as ErrOutcomeUnknown and is not resent.
	//
	// When it does NOT create, the returned Versioned is the ZERO VALUE — not
	// the value it collided with. A caller that needs to see what is already
	// there must Get it. This is stated because assuming otherwise is a
	// plausible and quiet mistake: the zero value's fields read as empty
	// strings and zeros, so code that inspects them takes a branch meant for
	// "absent" while the key is in fact occupied.
	Create(ctx context.Context, key K, value T) (Versioned[T], bool, error)

	// Delete removes the key only if the caller's version still matches.
	//
	// A delete writes no token, so a lost reply is settled only while the key
	// still holds the caller's version (resent) or has moved on from it
	// (ErrVersionMismatch); an absent key does not say who deleted it and
	// comes back as ErrOutcomeUnknown.
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

// compare-and-set 的统一计数（维护者第十二轮决定“CAS 冲突率口径”，N06 观察 2）。
//
// 冲突率只在这一层数：RedisStore.Update 的每次尝试与预算用尽各计一次，不在 versionstore 里的
// compare-and-set 循环（例如 kit/service/rank 的有序集合 + 哈希脚本）调用同样的两个函数。服务不再
// 各自对 ErrConflict 报 servicemetrics.Conflict——那是同一个事件的第二份口径；servicemetrics 的
// Conflict 留给业务冲突（insert-only 撞号、已绑定到别处等），它们不是存储竞争。
const (
	// MetricCompareAndSet 是 compare-and-set 尝试次数，标签 store（键前缀）与 result=applied|lost。
	// 冲突率 = lost / (applied + lost)。导出名 versionstore_cas_total。
	MetricCompareAndSet = "versionstore.cas.total"
	// MetricConflict 是重试预算用尽、返回 ErrConflict 的次数，标签 store。导出名 versionstore_conflict_total。
	MetricConflict = "versionstore.conflict.total"
)

// CountCompareAndSet 记一次 compare-and-set 尝试。store 是存储的固定名字（键前缀），不能带每个键
// 不同的部分。
func CountCompareAndSet(store string, applied bool) {
	result := "lost"
	if applied {
		result = "applied"
	}
	metrics.IncCounter(MetricCompareAndSet, metrics.Labels{"store": storeLabel(store), "result": result}, 1)
}

// CountConflict 记一次预算用尽（调用方拿到 ErrConflict）。
func CountConflict(store string) {
	metrics.IncCounter(MetricConflict, metrics.Labels{"store": storeLabel(store)}, 1)
}

func storeLabel(store string) string {
	if store == "" {
		return "unnamed"
	}
	return store
}

// RetryBackoff waits before the next attempt of a compare-and-set loop,
// growing exponentially with full jitter so concurrent losers do not retry in
// lockstep.
//
// It is exported because not every compare-and-set can use this package's
// envelope model — a store that maintains a sorted set and a hash together
// needs its own script — and those loops need the same policy. Retrying
// immediately is what makes contention look like failure: N writers on one key
// all lose, all retry together, and exhaust the budget at the same moment. The
// caller then gets an error indistinguishable from an unreachable backend.
//
// base <= 0 selects DefaultRetryBackoff; a negative base disables sleeping,
// which is how a test exercises budget exhaustion quickly. sleep nil means
// time.Sleep.
func RetryBackoff(attempt int, base time.Duration, sleep func(time.Duration)) {
	if base < 0 {
		return
	}
	if base == 0 {
		base = DefaultRetryBackoff
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	shift := attempt
	if shift < 0 {
		shift = 0
	}
	if shift > 5 {
		shift = 5
	}
	window := base << shift
	// Full jitter: sleep somewhere in (0, window]. Sleeping the whole window
	// would only move the lockstep collision later.
	sleep(time.Duration(rand.Int63n(int64(window))) + time.Nanosecond)
}
