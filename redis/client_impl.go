package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client implements IRedis by wrapping go-redis.
type Client struct {
	rdb goredis.UniversalClient
}

// NewClient builds a client from a configuration, outside the Mod lifecycle.
//
// Production wiring goes through RedisMod, which owns config parsing, the
// registry capability and shutdown. This exists for the cases that have no
// registry: integration tests that must exercise real Redis semantics
// (Lua scripts, pipelines, WATCH) that an in-memory double reimplements
// rather than executes, and one-off operational tools. Callers own Close.
func NewClient(cfg *Config) (IRedis, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis: configuration is required")
	}
	if cfg.Addr == "" && !cfg.IsCluster() {
		return nil, fmt.Errorf("redis: addr or cluster addrs are required")
	}
	return NewRedisClient(cfg), nil
}

func NewRedisClient(cfg *Config) *Client {
	var rdb goredis.UniversalClient
	if cfg.IsCluster() {
		rdb = goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs:        cfg.ClusterAddrs,
			Password:     cfg.Password,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			MaxRetries:   cfg.MaxRetries,
			// Without this go-redis ignores the caller's context deadline on
			// the wire and waits for ReadTimeout instead: a lock Acquire with a
			// 500ms budget sat for the full 2s read timeout under injected
			// latency. The caller's deadline is the contract; ReadTimeout is
			// only the backstop for callers that gave none.
			ContextTimeoutEnabled: true,
		})
	} else {
		rdb = goredis.NewClient(&goredis.Options{
			Addr:                  cfg.Addr,
			Password:              cfg.Password,
			DB:                    cfg.DB,
			PoolSize:              cfg.PoolSize,
			MinIdleConns:          cfg.MinIdleConns,
			DialTimeout:           cfg.DialTimeout,
			ReadTimeout:           cfg.ReadTimeout,
			WriteTimeout:          cfg.WriteTimeout,
			MaxRetries:            cfg.MaxRetries,
			ContextTimeoutEnabled: true,
		})
	}
	return &Client{rdb: rdb}
}

// --- String/KV ---

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, ErrNil
	}
	return val, err
}

// MGet reads several keys in one round trip.
//
// Two details of the contract are handled here rather than left to callers.
//
// Zero keys returns early WITHOUT a round trip, because `MGET` with no
// arguments is an error in Redis — a caller paging an empty result should not
// have to special-case that, and one that forgot to would fail on an ordinary
// empty page.
//
// The result is positional and always as long as keys: go-redis returns
// `[]any` with a nil for each absent key, and that nil is preserved as a nil
// element rather than dropped. Dropping it would shorten the result, and a
// short result is indistinguishable from a truncated read — which is the
// silent-loss failure this shape exists to make detectable.
func (c *Client) MGet(ctx context.Context, keys ...string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	values, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	if len(values) != len(keys) {
		// An assertion, not a branch: a conforming driver always returns one
		// element per key, so no test can reach this against a real Redis.
		// It is here because the alternative to failing on a driver bug is
		// returning a result whose positions no longer line up with the keys
		// the caller asked for — misaligned data rather than an error.
		return nil, fmt.Errorf("redis: MGET returned %d values for %d keys", len(values), len(keys))
	}
	out := make([][]byte, len(keys))
	for index, value := range values {
		switch typed := value.(type) {
		case nil:
			// Absent. Left as a nil element, deliberately.
		case string:
			out[index] = []byte(typed)
		case []byte:
			out[index] = append([]byte(nil), typed...)
		default:
			return nil, fmt.Errorf("redis: MGET element %d is %T, want a string", index, value)
		}
	}
	return out, nil
}

func (c *Client) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	return c.rdb.Set(ctx, key, value, expiration).Err()
}

func (c *Client) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, value, expiration).Result()
}

func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Del(ctx, keys...).Result()
}

func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

func (c *Client) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return c.rdb.Expire(ctx, key, expiration).Result()
}

func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.rdb.TTL(ctx, key).Result()
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

func (c *Client) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return c.rdb.IncrBy(ctx, key, value).Result()
}

// --- Hash ---

func (c *Client) HGet(ctx context.Context, key, field string) ([]byte, error) {
	val, err := c.rdb.HGet(ctx, key, field).Bytes()
	if err == goredis.Nil {
		return nil, ErrNil
	}
	return val, err
}

func (c *Client) HSet(ctx context.Context, key string, values ...any) error {
	return c.rdb.HSet(ctx, key, values...).Err()
}

func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

func (c *Client) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return c.rdb.HDel(ctx, key, fields...).Result()
}

func (c *Client) HExists(ctx context.Context, key, field string) (bool, error) {
	return c.rdb.HExists(ctx, key, field).Result()
}

// --- List ---

func (c *Client) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.LPush(ctx, key, values...).Result()
}

func (c *Client) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.RPush(ctx, key, values...).Result()
}

func (c *Client) LPop(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.LPop(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, ErrNil
	}
	return val, err
}

func (c *Client) RPop(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.RPop(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, ErrNil
	}
	return val, err
}

func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

func (c *Client) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.LRange(ctx, key, start, stop).Result()
}

// LTrim implements ListTrimmer: in-place trim without the DEL+RPUSH
// loss window of the emulated fallback.
func (c *Client) LTrim(ctx context.Context, key string, start, stop int64) error {
	return c.rdb.LTrim(ctx, key, start, stop).Err()
}

// LRem implements ListRemover.
func (c *Client) LRem(ctx context.Context, key string, count int64, value any) (int64, error) {
	return c.rdb.LRem(ctx, key, count, value).Result()
}

// --- Sorted Set ---

func (c *Client) ZAdd(ctx context.Context, key string, members ...Z) (int64, error) {
	zs := make([]goredis.Z, len(members))
	for i, m := range members {
		zs[i] = goredis.Z{Score: m.Score, Member: m.Member}
	}
	return c.rdb.ZAdd(ctx, key, zs...).Result()
}

func (c *Client) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.ZRem(ctx, key, members...).Result()
}

func (c *Client) ZScore(ctx context.Context, key string, member string) (float64, error) {
	score, err := c.rdb.ZScore(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, ErrNil
	}
	return score, err
}

func (c *Client) ZRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, ErrNil
	}
	return rank, err
}

func (c *Client) ZRevRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRevRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, ErrNil
	}
	return rank, err
}

func (c *Client) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error) {
	result, err := c.rdb.ZRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *Client) ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error) {
	result, err := c.rdb.ZRevRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *Client) ZCard(ctx context.Context, key string) (int64, error) {
	return c.rdb.ZCard(ctx, key).Result()
}

// --- Set ---

func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SAdd(ctx, key, members...).Result()
}

func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SRem(ctx, key, members...).Result()
}

func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

// --- Pipeline / Script ---

func (c *Client) Pipeline() IPipeline {
	return newPipeline(c.rdb.Pipeline())
}

func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return c.rdb.Eval(ctx, script, keys, args...).Result()
}

func (c *Client) EvalSha(ctx context.Context, sha string, keys []string, args ...any) (any, error) {
	return c.rdb.EvalSha(ctx, sha, keys, args...).Result()
}

// EvalDurable pins a physical connection so WAITAOF observes the replication
// offset produced by the immediately preceding script. Redis Cluster cannot
// safely provide this through go-redis's keyless-command routing, so it is
// rejected instead of silently weakening the durability contract.
func (c *Client) EvalDurable(ctx context.Context, script string, keys []string, numLocal, numReplicas int, timeout time.Duration, args ...any) (any, int64, int64, error) {
	results, local, replicas, err := c.EvalBatchDurable(ctx, script, []EvalCall{{Keys: keys, Args: args}}, numLocal, numReplicas, timeout)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(results) != 1 {
		return nil, 0, 0, fmt.Errorf("redis: durable eval returned %d results, want 1", len(results))
	}
	return results[0], local, replicas, nil
}

func (c *Client) EvalBatchDurable(ctx context.Context, script string, calls []EvalCall, numLocal, numReplicas int, timeout time.Duration) ([]any, int64, int64, error) {
	if len(calls) == 0 {
		return nil, 0, 0, nil
	}
	client, ok := c.rdb.(*goredis.Client)
	if !ok {
		return nil, 0, 0, fmt.Errorf("redis: same-connection WAITAOF is unsupported for %T; use a single-primary or Sentinel endpoint", c.rdb)
	}
	conn := client.Conn()
	defer func() { _ = conn.Close() }()
	pipe := conn.Pipeline()

	commands := make([]*goredis.Cmd, 0, len(calls))
	for _, call := range calls {
		commands = append(commands, pipe.Eval(ctx, script, call.Keys, call.Args...))
	}
	waitCommand := pipe.Do(ctx, "WAITAOF", numLocal, numReplicas, timeout.Milliseconds())
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, 0, 0, err
	}
	reply, err := waitCommand.Slice()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(reply) != 2 {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF reply length %d", len(reply))
	}
	local, err := redisInteger(reply[0])
	if err != nil {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF local reply: %w", err)
	}
	replicas, err := redisInteger(reply[1])
	if err != nil {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF replica reply: %w", err)
	}
	results := make([]any, 0, len(commands))
	for _, command := range commands {
		result, err := command.Result()
		if err != nil {
			return nil, 0, 0, err
		}
		results = append(results, result)
	}
	return results, local, replicas, nil
}

// --- PubSub ---

func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	return c.rdb.Publish(ctx, channel, message).Err()
}

func (c *Client) Subscribe(ctx context.Context, channels ...string) IPubSub {
	return newPubSub(c.rdb.Subscribe(ctx, channels...))
}

// --- Connection ---

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

// --- helpers ---

func convertZSlice(zs []goredis.Z) []Z {
	result := make([]Z, len(zs))
	for i, z := range zs {
		member := ""
		if s, ok := z.Member.(string); ok {
			member = s
		}
		result[i] = Z{Score: z.Score, Member: member}
	}
	return result
}

func redisInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, fmt.Errorf("integer overflow: %d", typed)
		}
		return int64(typed), nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected integer type %T", value)
	}
}

var _ IRedis = (*Client)(nil)
var _ DurableEvaler = (*Client)(nil)
var _ DurableBatchEvaler = (*Client)(nil)
var _ ListTrimmer = (*Client)(nil)
var _ ListRemover = (*Client)(nil)
