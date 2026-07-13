package redis

import (
	"context"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// redisClient implements fredis.IRedis by wrapping go-redis.
type redisClient struct {
	rdb goredis.UniversalClient
}

func newRedisClient(cfg *fredis.Config) *redisClient {
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
		})
	} else {
		rdb = goredis.NewClient(&goredis.Options{
			Addr:         cfg.Addr,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			MaxRetries:   cfg.MaxRetries,
		})
	}
	return &redisClient{rdb: rdb}
}

// --- String/KV ---

func (c *redisClient) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *redisClient) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	return c.rdb.Set(ctx, key, value, expiration).Err()
}

func (c *redisClient) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, value, expiration).Result()
}

func (c *redisClient) Del(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Del(ctx, keys...).Result()
}

func (c *redisClient) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

func (c *redisClient) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return c.rdb.Expire(ctx, key, expiration).Result()
}

func (c *redisClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.rdb.TTL(ctx, key).Result()
}

func (c *redisClient) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

func (c *redisClient) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return c.rdb.IncrBy(ctx, key, value).Result()
}

// --- Hash ---

func (c *redisClient) HGet(ctx context.Context, key, field string) ([]byte, error) {
	val, err := c.rdb.HGet(ctx, key, field).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *redisClient) HSet(ctx context.Context, key string, values ...any) error {
	return c.rdb.HSet(ctx, key, values...).Err()
}

func (c *redisClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

func (c *redisClient) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return c.rdb.HDel(ctx, key, fields...).Result()
}

func (c *redisClient) HExists(ctx context.Context, key, field string) (bool, error) {
	return c.rdb.HExists(ctx, key, field).Result()
}

// --- List ---

func (c *redisClient) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.LPush(ctx, key, values...).Result()
}

func (c *redisClient) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.RPush(ctx, key, values...).Result()
}

func (c *redisClient) LPop(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.LPop(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *redisClient) RPop(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.RPop(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *redisClient) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

func (c *redisClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.LRange(ctx, key, start, stop).Result()
}

// --- Sorted Set ---

func (c *redisClient) ZAdd(ctx context.Context, key string, members ...fredis.Z) (int64, error) {
	zs := make([]goredis.Z, len(members))
	for i, m := range members {
		zs[i] = goredis.Z{Score: m.Score, Member: m.Member}
	}
	return c.rdb.ZAdd(ctx, key, zs...).Result()
}

func (c *redisClient) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.ZRem(ctx, key, members...).Result()
}

func (c *redisClient) ZScore(ctx context.Context, key string, member string) (float64, error) {
	score, err := c.rdb.ZScore(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return score, err
}

func (c *redisClient) ZRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return rank, err
}

func (c *redisClient) ZRevRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRevRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return rank, err
}

func (c *redisClient) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]fredis.Z, error) {
	result, err := c.rdb.ZRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *redisClient) ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]fredis.Z, error) {
	result, err := c.rdb.ZRevRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *redisClient) ZCard(ctx context.Context, key string) (int64, error) {
	return c.rdb.ZCard(ctx, key).Result()
}

// --- Set ---

func (c *redisClient) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SAdd(ctx, key, members...).Result()
}

func (c *redisClient) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SRem(ctx, key, members...).Result()
}

func (c *redisClient) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *redisClient) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

// --- Pipeline / Script ---

func (c *redisClient) Pipeline() fredis.IPipeline {
	return newPipeline(c.rdb.Pipeline())
}

func (c *redisClient) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return c.rdb.Eval(ctx, script, keys, args...).Result()
}

func (c *redisClient) EvalSha(ctx context.Context, sha string, keys []string, args ...any) (any, error) {
	return c.rdb.EvalSha(ctx, sha, keys, args...).Result()
}

// --- PubSub ---

func (c *redisClient) Publish(ctx context.Context, channel string, message any) error {
	return c.rdb.Publish(ctx, channel, message).Err()
}

func (c *redisClient) Subscribe(ctx context.Context, channels ...string) fredis.IPubSub {
	return newPubSub(c.rdb.Subscribe(ctx, channels...))
}

// --- Connection ---

func (c *redisClient) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *redisClient) Close() error {
	return c.rdb.Close()
}

// --- helpers ---

func convertZSlice(zs []goredis.Z) []fredis.Z {
	result := make([]fredis.Z, len(zs))
	for i, z := range zs {
		member := ""
		if s, ok := z.Member.(string); ok {
			member = s
		}
		result[i] = fredis.Z{Score: z.Score, Member: member}
	}
	return result
}

var _ fredis.IRedis = (*redisClient)(nil)
