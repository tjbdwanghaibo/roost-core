package versionstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
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
	return &RedisStore[K, T]{client: client, cfg: cfg}, nil
}

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
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{
		Key: redisKey, Expected: raw, Next: deleteSentinel, TTL: time.Second,
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

// backoff waits before the next attempt, growing exponentially with jitter so
// concurrent losers do not retry in lockstep.
func (s *RedisStore[K, T]) backoff(attempt int) {
	base := s.cfg.RetryBackoff
	if base < 0 {
		return
	}
	shift := attempt
	if shift > 5 {
		shift = 5
	}
	window := base << shift
	// Full jitter: sleep somewhere in (0, window]. Sleeping the full window
	// would just move the lockstep collision later.
	delay := time.Duration(rand.Int63n(int64(window))) + time.Nanosecond
	s.cfg.Sleep(delay)
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
