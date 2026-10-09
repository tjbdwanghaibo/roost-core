package driver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：驱动 Close 的统一口径——重复 Close 幂等返回 nil，第一次的
// 错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他调用返回已关闭错误（这里是
// goredis.ErrClosed）。
//
// 旧行为：Client.Close 直接透传 go-redis。单机部署第二次 Close 返回 “redis: client is closed”，Cluster
// 返回 nil，调用方拿到的结果取决于部署形态；并发 Close 时单机的后到者立刻返回 ErrClosed。分布式锁在
// 客户端关闭后 Acquire 一次就被记成“结果未知”，之后一直返回 ErrDistLockStateUncertain——而 ErrClosed
// 按 IsDefinitelyNotExecuted 的分类是“确定没执行”，锁的状态并没有不确定。
//
// 用不可达地址（127.0.0.1:1），不需要真实 Redis：Close / 关闭后的命令都不经过网络。
func closeContractConfig(cluster bool) *fredis.Config {
	cfg := fredis.DefaultConfig("127.0.0.1:1")
	cfg.MaxRetries = -1
	cfg.DialTimeout = 200 * time.Millisecond
	cfg.MinIdleConns = 0
	if cluster {
		cfg.Addr = ""
		cfg.ClusterAddrs = []string{"127.0.0.1:1", "127.0.0.1:2"}
	}
	return cfg
}

func forEachDeployment(t *testing.T, run func(t *testing.T, cluster bool)) {
	for _, cluster := range []bool{false, true} {
		name := "single"
		if cluster {
			name = "cluster"
		}
		t.Run(name, func(t *testing.T) { run(t, cluster) })
	}
}

func TestClientRepeatedCloseReturnsNilOnEveryDeployment(t *testing.T) {
	forEachDeployment(t, func(t *testing.T, cluster bool) {
		c := NewRedisClient(closeContractConfig(cluster))
		if err := c.Close(); err != nil {
			t.Fatalf("first Close = %v", err)
		}
		for attempt := 2; attempt <= 3; attempt++ {
			if err := c.Close(); err != nil {
				t.Fatalf("Close #%d = %v, want nil: repeated Close is idempotent on every deployment", attempt, err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := c.Get(ctx, "k"); !errors.Is(err, goredis.ErrClosed) {
			t.Fatalf("Get after Close = %v, want goredis.ErrClosed", err)
		}
		if err := c.Set(ctx, "k", "v", 0); !errors.Is(err, goredis.ErrClosed) {
			t.Fatalf("Set after Close = %v, want goredis.ErrClosed", err)
		}
	})
}

// 第一次 Close 出错（底层已被别处关过）只报这一次，之后返回 nil。
func TestClientCloseErrorIsReportedOnce(t *testing.T) {
	forEachDeployment(t, func(t *testing.T, cluster bool) {
		c := NewRedisClient(closeContractConfig(cluster))
		_ = c.Raw().Close()
		first := c.Close()
		if !cluster && !errors.Is(first, goredis.ErrClosed) {
			// go-redis 单机对已关闭的连接池返回 ErrClosed；Cluster 自己幂等，第一次就是 nil。
			t.Fatalf("first Close on a pool closed underneath = %v, want goredis.ErrClosed", first)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil: the first error is reported once", err)
		}
	})
}

func TestClientConcurrentCloseAllReturnNil(t *testing.T) {
	forEachDeployment(t, func(t *testing.T, cluster bool) {
		for round := 0; round < 5; round++ {
			c := NewRedisClient(closeContractConfig(cluster))
			var wg sync.WaitGroup
			errs := make([]error, 4)
			for i := range errs {
				wg.Add(1)
				go func(i int) { defer wg.Done(); errs[i] = c.Close() }(i)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("round %d: concurrent Close #%d = %v, want nil (all = %v)", round, i, err, errs)
				}
			}
		}
	})
}

func TestAssemblyRepeatedCloseReturnsNil(t *testing.T) {
	forEachDeployment(t, func(t *testing.T, cluster bool) {
		asm, err := Assemble(closeContractConfig(cluster))
		if err != nil {
			t.Fatal(err)
		}
		if err := asm.Close(); err != nil {
			t.Fatalf("first Assembly.Close = %v", err)
		}
		if err := asm.Close(); err != nil {
			t.Fatalf("second Assembly.Close = %v, want nil", err)
		}
	})
}

// 客户端关闭后，锁操作返回 ErrClosed，而且每次都是 ErrClosed：命令确定没发出去，锁状态没有变成未知。
func TestDistLockAfterClientCloseKeepsReportingErrClosed(t *testing.T) {
	forEachDeployment(t, func(t *testing.T, cluster bool) {
		asm, err := Assemble(closeContractConfig(cluster))
		if err != nil {
			t.Fatal(err)
		}
		lock := asm.Locks.NewLock("close-contract-lock", time.Second)
		if err := asm.Close(); err != nil {
			t.Fatalf("Assembly.Close = %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for attempt := 1; attempt <= 2; attempt++ {
			ok, err := lock.Acquire(ctx)
			if ok || !errors.Is(err, goredis.ErrClosed) {
				t.Fatalf("Acquire #%d after Close = (%v, %v), want (false, goredis.ErrClosed): the SetNX was never sent", attempt, ok, err)
			}
		}
		if err := lock.Release(ctx); !errors.Is(err, fredis.ErrLockNotHeld) {
			t.Fatalf("Release of a lock never acquired = %v, want ErrLockNotHeld", err)
		}
	})
}
