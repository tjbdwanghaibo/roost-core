package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// singletonPingTimeout 是打开 SingletonStore 时首次 Ping 的超时。
const singletonPingTimeout = 5 * time.Second

// 两个客户端各自的连接池大小（MinIdleConns 0）。锁操作只有一个写者顺序调用（启动获取、续期、
// 最后的 Release），留 2 条给 Cluster 拓扑刷新与超时后重拨的余量；Live 查询可能并发，但只是
// 只读的活性查询，超出时排队等连接即可。
const (
	singletonLockPoolSize = 2
	singletonLivePoolSize = 2
)

// SingletonStore 是 App 单实例锁的 Redis 后端（app.SingletonOpener），在 bootstrap 里这样安装：
//
//	app.New(...).Singleton(kitredis.SingletonStore)
//
// 它在任何 Mod 之前被调用，只依赖已读完的 redis.* 配置，自己建两个独立的小客户端，不依赖 Redis Mod：
// 一个只给 CAS / 认领 / 按值删除（锁操作），一个只给 Live 查询的 Get。二者不共用连接池：Redis 变慢时
// 并发的 Live 查询可能占满连接，续期若和它们抢连接，就会等不到连接而超时（Unknown），一直如此会被
// 误判 Lost、进程 fail-stop。缺 redis.addr 与 redis.cluster_addrs 时报错——不沿用 RedisMod 的
// localhost:6379 兜底，否则一个忘了配 Redis 的服务会对着本机的 Redis 加锁。
//
// CAS 只涉及单键，不需要 hash tag。单次调用的超时由调用方的 ctx 截止时间决定（客户端开启
// ContextTimeoutEnabled），满足 app.SingletonStore 的契约。驱动的自动重试关闭（MaxRetries -1），
// 跨节拍的重试与“丢回复”的判定由 App 的状态机负责：单机下每次调用是一次往返；Redis Cluster 下
// go-redis 仍按 MaxRedirects（缺省 3）处理 MOVED / ASK、对网络错误换节点重试（osscluster.go 的
// process），这些重试与其间的退避都受同一个 ctx 限定，重复执行也安全（CAS(v, v) 幂等、获取重试时
// 读到自己的值由 App 认领、按值删除只删自己的值）。
func SingletonStore(cfg *viper.Viper) (app.SingletonStore, error) {
	if cfg == nil {
		return nil, errors.New("kitredis: singleton store: nil config")
	}
	conn, err := redisConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kitredis: singleton store: %w", err)
	}
	if strings.TrimSpace(conn.Addr) == "" && !conn.IsCluster() {
		return nil, errors.New("kitredis: singleton store: redis.addr or redis.cluster_addrs is required when singleton.enabled=true")
	}
	store, err := newSingletonStore(conn)
	if err != nil {
		return nil, fmt.Errorf("kitredis: singleton store: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), singletonPingTimeout)
	defer cancel()
	if err := store.lock.Ping(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("kitredis: singleton store: ping: %w", err)
	}
	return store, nil
}

// newSingletonStore 按 conn 建锁操作与 Live 两个客户端（不拨号）。
func newSingletonStore(conn *fredis.Config) (*singletonStore, error) {
	client := func(poolSize int) (fredis.IRedis, error) {
		c := *conn
		c.PoolSize = poolSize
		c.MinIdleConns = 0
		c.MaxRetries = -1
		return redisdriver.NewClient(&c)
	}
	lock, err := client(singletonLockPoolSize)
	if err != nil {
		return nil, err
	}
	live, err := client(singletonLivePoolSize)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &singletonStore{lock: lock, live: live}, nil
}

// singletonStore 把 app.SingletonStore 转到 core 的 redis.CompareAndSet / CompareAndDelete。
// go-redis 客户端并发安全，续期 goroutine 与 Live 查询可以同时调用；二者走不同的客户端，互不占连接。
type singletonStore struct {
	lock fredis.IRedis // CompareAndSet / CompareAndDelete
	live fredis.IRedis // Get

	closeOnce sync.Once
	closeErr  error
}

func (s *singletonStore) CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (bool, []byte, error) {
	result, err := fredis.CompareAndSet(ctx, s.lock, fredis.CompareAndSetCommand{Key: key, Expected: expected, Next: next, TTL: ttl})
	if err != nil {
		return false, nil, err
	}
	return result.Applied, result.Current, nil
}

func (s *singletonStore) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	result, err := fredis.CompareAndDelete(ctx, s.lock, key, expected)
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
	pipe := s.live.Pipeline()
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

// Close 幂等：只有第一次真正关闭两个客户端，之后的调用返回第一次的结果。
func (s *singletonStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = errors.Join(s.lock.Close(), s.live.Close()) })
	return s.closeErr
}

var _ app.SingletonStore = (*singletonStore)(nil)
