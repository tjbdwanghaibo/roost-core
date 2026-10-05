package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// singletonPingTimeout 是打开 SingletonStore 时首次 Ping 的超时。
const singletonPingTimeout = 5 * time.Second

// SingletonStore 是 App 单实例锁的 Redis 后端（app.SingletonOpener），在 bootstrap 里这样安装：
//
//	app.New(...).Singleton(kitredis.SingletonStore)
//
// 它在任何 Mod 之前被调用，只依赖已读完的 redis.* 配置，自己建一条独立的小连接
// （PoolSize 2、MinIdleConns 0），不依赖 Redis Mod。缺 redis.addr 与 redis.cluster_addrs 时报错——
// 不沿用 RedisMod 的 localhost:6379 兜底，否则一个忘了配 Redis 的服务会对着本机的 Redis 加锁。
//
// CAS 只涉及单键，不需要 hash tag。驱动的自动重试关闭（MaxRetries -1）：每次 CAS 是一次往返，
// 重试与“丢回复”的判定由 App 的状态机按节拍负责；单次调用的超时由调用方的 ctx 截止时间决定
// （客户端开启 ContextTimeoutEnabled）。
func SingletonStore(cfg *viper.Viper) (app.SingletonStore, error) {
	if cfg == nil {
		return nil, errors.New("kitredis: singleton store: nil config")
	}
	conn := redisConfig(cfg)
	if strings.TrimSpace(conn.Addr) == "" && !conn.IsCluster() {
		return nil, errors.New("kitredis: singleton store: redis.addr or redis.cluster_addrs is required when singleton.enabled=true")
	}
	conn.PoolSize = 2
	conn.MinIdleConns = 0
	conn.MaxRetries = -1
	client, err := redisdriver.NewClient(conn)
	if err != nil {
		return nil, fmt.Errorf("kitredis: singleton store: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), singletonPingTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("kitredis: singleton store: ping: %w", err)
	}
	return &singletonStore{client: client}, nil
}

// singletonStore 把 app.SingletonStore 转到 core 的 redis.CompareAndSet / CompareAndDelete。
// go-redis 客户端并发安全，续期 goroutine 与 Live 查询可以同时调用。
type singletonStore struct {
	client fredis.IRedis
}

func (s *singletonStore) CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (bool, []byte, error) {
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{Key: key, Expected: expected, Next: next, TTL: ttl})
	if err != nil {
		return false, nil, err
	}
	return result.Applied, result.Current, nil
}

func (s *singletonStore) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	result, err := fredis.CompareAndDelete(ctx, s.client, key, expected)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

// Get 逐键 GET，放进一个 pipeline：单机是一次往返，Redis Cluster 由客户端按槽拆分。不用 MGET——
// 各 sid 的键散在不同槽，单条 MGET 在 Cluster 下报 CROSSSLOT，也就不要求 key_prefix 带 hash tag。
func (s *singletonStore) Get(ctx context.Context, keys []string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	futures := make([]*fredis.FutureBytes, len(keys))
	for i, key := range keys {
		futures[i] = pipe.Get(ctx, key)
	}
	if err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([][]byte, len(keys))
	for i, future := range futures {
		value, err := future.Result()
		switch {
		case errors.Is(err, fredis.ErrNil):
		case err != nil:
			return nil, fmt.Errorf("get %s: %w", keys[i], err)
		default:
			out[i] = value
		}
	}
	return out, nil
}

func (s *singletonStore) Close() error {
	return s.client.Close()
}

var _ app.SingletonStore = (*singletonStore)(nil)
