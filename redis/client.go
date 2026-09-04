package redis

import (
	"context"
	"time"
)

// IRedis is the abstraction for Redis operations.
type IRedis interface {
	// --- String/KV ---
	Get(ctx context.Context, key string) ([]byte, error)
	// MGet reads several keys in ONE round trip.
	//
	// The result is POSITIONAL: len(result) always equals len(keys), and a nil
	// element means that key is absent. It is a slice rather than a map on
	// purpose — a map would omit missing keys, and then a reply that came back
	// short for any other reason would be indistinguishable from "those keys
	// were not there". A caller that can compare lengths can detect a
	// truncated read; one holding a map cannot.
	//
	// Zero keys returns an empty result and makes NO round trip. This is part
	// of the contract because `MGET` with no arguments is an error in Redis,
	// so an implementation that passed the empty case through would fail on
	// an ordinary empty page.
	//
	// Unlike Get, an absent key is not ErrNil: with several keys, absence is a
	// per-element outcome and not a call-level one.
	MGet(ctx context.Context, keys ...string) ([][]byte, error)
	Set(ctx context.Context, key string, value any, expiration time.Duration) error
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error)
	Del(ctx context.Context, keys ...string) (int64, error)
	Exists(ctx context.Context, keys ...string) (int64, error)
	Expire(ctx context.Context, key string, expiration time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	Incr(ctx context.Context, key string) (int64, error)
	IncrBy(ctx context.Context, key string, value int64) (int64, error)

	// --- Hash ---
	HGet(ctx context.Context, key, field string) ([]byte, error)
	HSet(ctx context.Context, key string, values ...any) error
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HDel(ctx context.Context, key string, fields ...string) (int64, error)
	HExists(ctx context.Context, key, field string) (bool, error)

	// --- List ---
	LPush(ctx context.Context, key string, values ...any) (int64, error)
	RPush(ctx context.Context, key string, values ...any) (int64, error)
	LPop(ctx context.Context, key string) ([]byte, error)
	RPop(ctx context.Context, key string) ([]byte, error)
	LLen(ctx context.Context, key string) (int64, error)
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)

	// --- Sorted Set ---
	ZAdd(ctx context.Context, key string, members ...Z) (int64, error)
	ZRem(ctx context.Context, key string, members ...any) (int64, error)
	ZScore(ctx context.Context, key string, member string) (float64, error)
	ZRank(ctx context.Context, key string, member string) (int64, error)
	ZRevRank(ctx context.Context, key string, member string) (int64, error)
	ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error)
	ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error)
	ZCard(ctx context.Context, key string) (int64, error)

	// --- Set ---
	SAdd(ctx context.Context, key string, members ...any) (int64, error)
	SRem(ctx context.Context, key string, members ...any) (int64, error)
	SMembers(ctx context.Context, key string) ([]string, error)
	SIsMember(ctx context.Context, key string, member any) (bool, error)

	// --- Pipeline / Script ---
	Pipeline() IPipeline
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
	EvalSha(ctx context.Context, sha string, keys []string, args ...any) (any, error)

	// --- PubSub ---
	Publish(ctx context.Context, channel string, message any) error
	Subscribe(ctx context.Context, channels ...string) IPubSub

	// --- Connection ---
	Ping(ctx context.Context) error
	Close() error
}

type EvalCall struct {
	Keys []string
	Args []any
}

// DurableEvaler executes a script and WAITAOF on the same physical Redis
// connection. Independent pooled calls do not satisfy this contract.
type DurableEvaler interface {
	EvalDurable(ctx context.Context, script string, keys []string, numLocal, numReplicas int, timeout time.Duration, args ...any) (result any, local, replicas int64, err error)
}

// DurableBatchEvaler pipelines all scripts and one trailing WAITAOF on one
// physical connection, amortizing the fsync across the admitted batch.
type DurableBatchEvaler interface {
	EvalBatchDurable(ctx context.Context, script string, calls []EvalCall, numLocal, numReplicas int, timeout time.Duration) (results []any, local, replicas int64, err error)
}

// ListTrimmer trims a list in place (LTRIM). Optional capability: callers
// that would otherwise emulate a trim with DEL+RPUSH (a window in which a
// crash loses the whole list) should prefer this when the client provides it.
type ListTrimmer interface {
	LTrim(ctx context.Context, key string, start, stop int64) error
}

// ListRemover removes occurrences of a value from a list in place (LREM).
// Optional capability with the same motivation as ListTrimmer.
type ListRemover interface {
	LRem(ctx context.Context, key string, count int64, value any) (int64, error)
}

// Z represents a sorted set member with score.
type Z struct {
	Score  float64
	Member string
}
