package versionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RedisClient is the slice of redis.IRedis that RedisStore uses. Taking the
// narrow interface documents exactly which backend capability the store
// depends on, and lets a test supply an evaluating double without stubbing a
// whole client. A real *redis client satisfies it unchanged.
type RedisClient interface {
	fredis.ScriptRunner
	Get(ctx context.Context, key string) ([]byte, error)
	Del(ctx context.Context, keys ...string) (int64, error)
}

// RedisConfig configures a RedisStore.
type RedisConfig[K comparable, T any] struct {
	// Prefix is prepended to every rendered key.
	Prefix string
	// KeyOf renders a key; required.
	KeyOf KeyFunc[K]
	// Codec encodes and decodes the value; required.
	Codec Codec[T]
	// TTL, when positive, expires stored values. Note that a TTL on
	// versioned state loses the version along with the value, so a key that
	// expires and is written again restarts at version 1 — only use it for
	// state whose absence is a valid outcome.
	TTL time.Duration
	// MaxAttempts bounds the retry loop; zero means DefaultMaxAttempts.
	MaxAttempts int
	// RetryBackoff is the base delay before re-reading after a lost
	// compare-and-set; zero means DefaultRetryBackoff. It grows exponentially
	// with jitter, capped at 32x the base.
	//
	// Retrying immediately is what makes contention look like failure: N
	// writers on one key all lose, all retry in lockstep, and exhaust the
	// budget together. Reporting that as a conflict gives the caller an error
	// indistinguishable from an unreachable backend — the defect this
	// primitive exists to remove, not reproduce. Set it to a negative value to
	// disable sleeping (tests that want to exercise exhaustion quickly).
	RetryBackoff time.Duration
	// Sleep is the delay function; nil means time.Sleep. Test seam.
	Sleep func(time.Duration)
	// Index, when set, is a durable index of the entries a reader has work
	// for, maintained in the SAME write as the value.
	//
	// A caller that keeps such an index outside the store has two writes and
	// therefore a window: the record lands, the process dies, and the record
	// is in no index — for a paid order that means a payment no background
	// loop can ever find (RR-20260919-04). Inside the store it is one script:
	// the entry moves if and only if the compare-and-set applies.
	Index *RedisIndex[T]
}

// RedisIndex describes the sorted-set index a store maintains beside its
// values. The score is what a reader pages by — typically when the entry is
// next worth looking at — and include is what makes the index a WORK LIST
// rather than a copy of the keyspace: a value the reader has nothing to do
// with is retired from it by the write that made it so.
type RedisIndex[T any] struct {
	Key   string
	Entry func(T) (score float64, include bool)
}

// RedisStore keeps versioned values as a framed envelope "<version>\n<payload>"
// under one key, and mutates them with fredis.CompareAndSet.
//
// The compare is on the exact bytes that were read, so it is a version compare
// in effect: the version leads the envelope and the payload cannot change
// without the version changing. Passing back the bytes as read — rather than
// re-encoding the previous value — is what keeps a serialization change from
// wedging writes across a rolling deploy.
type RedisStore[K comparable, T any] struct {
	client RedisClient
	cfg    RedisConfig[K, T]
}

func NewRedisStore[K comparable, T any](client RedisClient, cfg RedisConfig[K, T]) (*RedisStore[K, T], error) {
	if client == nil {
		return nil, fmt.Errorf("versionstore: redis client is nil")
	}
	if cfg.KeyOf == nil {
		return nil, fmt.Errorf("versionstore: key func is nil")
	}
	if cfg.Codec == nil {
		return nil, ErrCodecNil
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = DefaultRetryBackoff
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Index != nil {
		if strings.TrimSpace(cfg.Index.Key) == "" {
			return nil, fmt.Errorf("versionstore: index key is required")
		}
		if cfg.Index.Entry == nil {
			return nil, fmt.Errorf("versionstore: index entry func is required")
		}
	}
	return &RedisStore[K, T]{client: client, cfg: cfg}, nil
}

// indexEntry is the index side of one write. A store without an index returns
// nil and the script stays single-key.
func (s *RedisStore[K, T]) indexEntry(key K, value T) *fredis.CompareAndSetIndex {
	if s.cfg.Index == nil {
		return nil
	}
	member := s.cfg.KeyOf(key)
	if member == "" {
		return nil
	}
	score, include := s.cfg.Index.Entry(value)
	return &fredis.CompareAndSetIndex{
		Key: s.cfg.Index.Key, Member: member, Score: score, Remove: !include,
	}
}

// IndexDue returns up to limit indexed keys whose score is at or below
// maxScore, lowest first.
//
// Lowest first is the fairness property: an entry whose work failed moves its
// own score forward, so it cannot sit at the head of every page and starve
// what is queued behind it. The limit is the caller's batch, not a page
// cursor — the next call starts from the lowest score again, which is what a
// retry loop wants.
func (s *RedisStore[K, T]) IndexDue(ctx context.Context, maxScore float64, limit int) ([]string, error) {
	if s.cfg.Index == nil {
		return nil, fmt.Errorf("versionstore: this store has no index")
	}
	if limit <= 0 {
		return nil, nil
	}
	raw, err := s.client.Eval(ctx, indexDueScript, []string{s.cfg.Index.Key},
		strconv.FormatFloat(maxScore, 'f', -1, 64), strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	items, ok := raw.([]any)
	if !ok {
		if typed, converted := raw.([]interface{}); converted {
			items = typed
		} else if raw == nil {
			return nil, nil
		} else {
			return nil, fmt.Errorf("versionstore: unexpected index reply %T", raw)
		}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			out = append(out, typed)
		case []byte:
			out = append(out, string(typed))
		default:
			return nil, fmt.Errorf("versionstore: unexpected index member %T", item)
		}
	}
	return out, nil
}

// IndexRemove drops one member from the index without touching its value.
//
// It is for entries that name a value which is not there — the only case in
// which the index and the keyspace can legitimately disagree, and one a reader
// would otherwise carry on every page forever. Removing an entry whose value
// DOES exist would hide live work, so callers must establish absence first.
func (s *RedisStore[K, T]) IndexRemove(ctx context.Context, key K) (bool, error) {
	if s.cfg.Index == nil {
		return false, fmt.Errorf("versionstore: this store has no index")
	}
	member := s.cfg.KeyOf(key)
	if member == "" {
		return false, ErrKeyEmpty
	}
	raw, err := s.client.Eval(ctx, indexRemoveScript, []string{s.cfg.Index.Key}, member)
	if err != nil {
		return false, err
	}
	removed, _ := raw.(int64)
	return removed > 0, nil
}

const indexRemoveScript = `
return redis.call("ZREM", KEYS[1], ARGV[1])
`

const indexDueScript = `
return redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[2])
`

func (s *RedisStore[K, T]) key(key K) (string, error) {
	rendered := s.cfg.KeyOf(key)
	if rendered == "" {
		return "", ErrKeyEmpty
	}
	return s.cfg.Prefix + rendered, nil
}

// readRaw returns the stored envelope alongside the decoded value, because
// Update needs the exact bytes for the compare and the value for mutate.
func (s *RedisStore[K, T]) readRaw(ctx context.Context, redisKey string) (raw []byte, value Versioned[T], found bool, err error) {
	stored, err := s.client.Get(ctx, redisKey)
	if err != nil {
		if isRedisMiss(err) {
			return nil, Versioned[T]{}, false, nil
		}
		return nil, Versioned[T]{}, false, err
	}
	if stored == nil {
		return nil, Versioned[T]{}, false, nil
	}
	decoded, err := s.decodeEnvelope(stored)
	if err != nil {
		return nil, Versioned[T]{}, false, err
	}
	return stored, decoded, true, nil
}

func (s *RedisStore[K, T]) Get(ctx context.Context, key K) (Versioned[T], bool, error) {
	redisKey, err := s.key(key)
	if err != nil {
		return Versioned[T]{}, false, err
	}
	_, value, found, err := s.readRaw(ctx, redisKey)
	return value, found, err
}

func (s *RedisStore[K, T]) Update(ctx context.Context, key K, mutate Mutate[T]) (Versioned[T], bool, error) {
	if mutate == nil {
		return Versioned[T]{}, false, fmt.Errorf("versionstore: mutate is nil")
	}
	redisKey, err := s.key(key)
	if err != nil {
		return Versioned[T]{}, false, err
	}

	raw, current, found, err := s.readRaw(ctx, redisKey)
	if err != nil {
		return Versioned[T]{}, false, err
	}
	for attempt := 0; attempt < s.cfg.MaxAttempts; attempt++ {
		next, save, err := mutate(current.Value, found)
		if err != nil {
			return Versioned[T]{}, false, err
		}
		if !save {
			return current, false, nil
		}
		envelope, err := s.encodeEnvelope(next, current.Version+1)
		if err != nil {
			return Versioned[T]{}, false, err
		}
		// A missing key must be created, not overwritten: Expected == nil
		// makes CompareAndSet require absence, so a value that appeared since
		// the read loses instead of being clobbered.
		var expected []byte
		if found {
			expected = raw
		}
		result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{
			Key: redisKey, Expected: expected, Next: envelope, TTL: s.cfg.TTL,
			Index: s.indexEntry(key, next),
		})
		if err != nil {
			return Versioned[T]{}, false, err
		}
		if result.Applied {
			return Versioned[T]{Value: next, Version: current.Version + 1}, true, nil
		}
		// Lost the race. CompareAndSet hands back what is stored now, so the
		// retry re-applies mutate to fresh state without another round trip.
		s.backoff(attempt)
		raw = result.Current
		if len(raw) == 0 {
			current, found = Versioned[T]{}, false
			continue
		}
		current, err = s.decodeEnvelope(raw)
		if err != nil {
			return Versioned[T]{}, false, err
		}
		found = true
	}
	return Versioned[T]{}, false, fmt.Errorf("%w: %s after %d attempts", ErrConflict, redisKey, s.cfg.MaxAttempts)
}

func (s *RedisStore[K, T]) Create(ctx context.Context, key K, value T) (Versioned[T], bool, error) {
	redisKey, err := s.key(key)
	if err != nil {
		return Versioned[T]{}, false, err
	}
	envelope, err := s.encodeEnvelope(value, 1)
	if err != nil {
		return Versioned[T]{}, false, err
	}
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{
		Key: redisKey, Expected: nil, Next: envelope, TTL: s.cfg.TTL,
		Index: s.indexEntry(key, value),
	})
	if err != nil {
		return Versioned[T]{}, false, err
	}
	if !result.Applied {
		return Versioned[T]{}, false, nil
	}
	return Versioned[T]{Value: value, Version: 1}, true, nil
}

func (s *RedisStore[K, T]) Delete(ctx context.Context, key K, expect Versioned[T]) error {
	redisKey, err := s.key(key)
	if err != nil {
		return err
	}
	raw, current, found, err := s.readRaw(ctx, redisKey)
	if err != nil {
		return err
	}
	if !found {
		if expect.Version == 0 {
			return nil
		}
		return fmt.Errorf("%w: %s is absent, caller held version %d", ErrVersionMismatch, redisKey, expect.Version)
	}
	if current.Version != expect.Version {
		return fmt.Errorf("%w: %s is at version %d, caller held %d", ErrVersionMismatch, redisKey, current.Version, expect.Version)
	}
	// Delete by compare-and-set to a tombstone-free state is not expressible
	// with CompareAndSet, so the version check above is confirmed by a
	// conditional delete: swap to a sentinel only if unchanged, then remove.
	// Doing it in one step would need a dedicated script; the swap makes the
	// window observable rather than silent.
	var retire *fredis.CompareAndSetIndex
	if s.cfg.Index != nil {
		if member := s.cfg.KeyOf(key); member != "" {
			retire = &fredis.CompareAndSetIndex{Key: s.cfg.Index.Key, Member: member, Remove: true}
		}
	}
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{
		Key: redisKey, Expected: raw, Next: deleteSentinel, TTL: time.Second,
		Index: retire,
	})
	if err != nil {
		return err
	}
	if !result.Applied {
		return fmt.Errorf("%w: %s changed during delete", ErrVersionMismatch, redisKey)
	}
	if _, err := s.client.Del(ctx, redisKey); err != nil {
		return err
	}
	return nil
}

var deleteSentinel = []byte("0\n")

func (s *RedisStore[K, T]) backoff(attempt int) {
	RetryBackoff(attempt, s.cfg.RetryBackoff, s.cfg.Sleep)
}

func (s *RedisStore[K, T]) encodeEnvelope(value T, version uint64) ([]byte, error) {
	payload, err := s.cfg.Codec.Encode(value)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(strconv.FormatUint(version, 10))
	buf.WriteByte('\n')
	buf.Write(payload)
	return buf.Bytes(), nil
}

func (s *RedisStore[K, T]) decodeEnvelope(raw []byte) (Versioned[T], error) {
	index := bytes.IndexByte(raw, '\n')
	if index <= 0 {
		return Versioned[T]{}, fmt.Errorf("versionstore: malformed envelope")
	}
	version, err := strconv.ParseUint(string(raw[:index]), 10, 64)
	if err != nil {
		return Versioned[T]{}, fmt.Errorf("versionstore: malformed envelope version: %w", err)
	}
	if version == 0 {
		return Versioned[T]{}, fmt.Errorf("versionstore: envelope version is zero")
	}
	value, err := s.cfg.Codec.Decode(raw[index+1:])
	if err != nil {
		return Versioned[T]{}, err
	}
	return Versioned[T]{Value: value, Version: version}, nil
}

func isRedisMiss(err error) bool {
	return errors.Is(err, fredis.ErrNil)
}

var _ Store[string, int] = (*RedisStore[string, int])(nil)
