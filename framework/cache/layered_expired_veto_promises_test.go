package cache

import (
	"context"
	"crypto/rand"
	"os"
	"strconv"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// RR-20261004-02：Layered 的 L1 副本只在自己的 TTL 窗口内可信。窗口过后（或
// ttl≤0，本来就承诺“不从 L1 服务”），它只是旧缓存，不能凭 Stale 否决权威（L2）
// 的值；远端已经接受的写也不能因为 L1 的旧副本拒写而报 ErrStaleWrite。
// 旧行为（v1.19.0）：回填遇 ErrStaleWrite 一律读回 L1，过期副本永久胜过权威；
// Set 在远端写成功后把 L1 的拒绝原样返回。
//
// 两个副本 A、B 共用同一个权威；B 删除后以较低版本开始新的生命周期（Redis
// TTL 到期后晚到的写同理），A 的 L1 仍记着上一个生命周期的 v5。L1 窗口由
// expireLayeredWindow 直接置为过期，不靠 sleep 等 TTL。
func TestLayeredExpiredLocalCopyDoesNotVetoAuthority(t *testing.T) {
	runLayeredExpiredVeto(t, func(*testing.T) Store[int, staleValue] { return NewLocalStore(staleConfig()) })
}

// 生成的带版本字段的 Cached Redis DAO 就是 Layered(LocalStore, RedisRawJSON)，
// 用真实 Redis 作权威跑同一组场景。键带随机前缀，用例结束删除自己的键。
func TestLayeredExpiredLocalCopyDoesNotVetoAuthorityRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires an isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	runLayeredExpiredVeto(t, func(t *testing.T) Store[int, staleValue] {
		prefix := "roost:rr20261004-02:" + rand.Text() + ":"
		store := NewRedisRawJSONStore(client, time.Minute, func(key int) string { return prefix + strconv.Itoa(key) }, staleConfig())
		t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
		return store
	})
}

func runLayeredExpiredVeto(t *testing.T, newAuthority func(*testing.T) Store[int, staleValue]) {
	for _, ttl := range []time.Duration{0, 30 * time.Millisecond} {
		for _, op := range []string{"get", "set"} {
			t.Run(ttl.String()+"/"+op, func(t *testing.T) {
				ctx := context.Background()
				authority := newAuthority(t)
				a := NewLayeredStore[int, staleValue](NewLocalStore(staleConfig()), authority, ttl, staleConfig())
				b := NewLayeredStore[int, staleValue](NewLocalStore(staleConfig()), authority, ttl, staleConfig())
				if err := a.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "old lifecycle"}); err != nil {
					t.Fatal(err)
				}
				if err := b.Delete(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if err := b.Set(ctx, staleValue{Key: 1, Version: 1, Payload: "new lifecycle"}); err != nil {
					t.Fatal(err)
				}
				expireLayeredWindow(a, 1)

				if op == "get" {
					for i := 0; i < 3; i++ {
						got, ok, err := a.Get(ctx, 1)
						t.Logf("A.Get #%d = %+v ok=%v err=%v", i, got, ok, err)
						if err != nil || !ok || got.Version != 1 {
							t.Fatalf("A.Get #%d served %+v ok=%v err=%v, authority holds v1", i, got, ok, err)
						}
					}
				}

				err := a.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "next"})
				held, _, _ := authority.Get(ctx, 1)
				t.Logf("A.Set(v2) err=%v authority=%+v", err, held)
				if held.Version != 2 {
					t.Fatalf("authority did not apply v2: %+v", held)
				}
				if err != nil {
					t.Fatalf("A.Set(v2) returned %v although the authority applied v2", err)
				}
				got, ok, err := a.Get(ctx, 1)
				if err != nil || !ok || got.Version != 2 {
					t.Fatalf("A.Get after Set served %+v ok=%v err=%v, want v2", got, ok, err)
				}
			})
		}
	}
}

// expireLayeredWindow 把 key 的 L1 窗口置为已过期（ttl≤0 时没有窗口，不需要）。
func expireLayeredWindow[K comparable, V any](s *LayeredStore[K, V], key K) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.expiry[key]; ok {
		s.expiry[key].Value = layeredExpiry[K]{key: key, expiresAt: time.Now().Add(-time.Nanosecond)}
	}
}
