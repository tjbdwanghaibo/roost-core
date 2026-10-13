// 分层存储：冷键元数据有界、过期淘汰回权威、过期 L1 不否决权威、旧写报 ErrStaleWrite、TTL 语义。

package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// 冷键不再读取时，L1/L2 淘汰不能留下随历史访问量增长的准入元数据。
func TestLayeredColdKeysHaveBoundedMetadata(t *testing.T) {
	cfg := StoreConfig[int, int]{KeyOf: func(v int) int { return v }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, int]{StoreConfig: cfg, Shards: 1, MaxEntries: 1})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, int]{StoreConfig: cfg, Shards: 1, MaxEntries: 1})
	store := NewLayeredStore[int, int](local, remote, time.Hour, cfg)
	for key := range 70000 {
		if err := store.Set(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(store.expiry); n > 65536 {
		t.Fatalf("expiry entries=%d, want <=65536 while each backing cache holds 1", n)
	}
}

func TestLayeredExpiryEvictionRevalidatesAuthority(t *testing.T) {
	cfg := staleConfig()
	local, remote := NewLocalStore(cfg), NewLocalStore(cfg)
	store := NewLayeredStore[int, staleValue](local, remote, time.Hour, cfg, LayeredOptions{MaxExpiryEntries: 2})
	ctx := context.Background()
	for key := 1; key <= 3; key++ {
		if err := store.Set(ctx, staleValue{Key: key, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := remote.Set(ctx, staleValue{Key: 1, Version: 2}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Version != 2 {
		t.Fatalf("evicted admission served stale L1: got=%+v ok=%v err=%v", got, ok, err)
	}
	if len(store.expiry) != 2 || store.order.Len() != 2 {
		t.Fatalf("metadata not bounded: map=%d order=%d", len(store.expiry), store.order.Len())
	}
}

func TestLayeredExpiryChurnAndColdExpiration(t *testing.T) {
	cfg := StoreConfig[int, int]{KeyOf: func(v int) int { return v }}
	store := NewLayeredStore[int, int](NewLocalStore(cfg), NewLocalStore(cfg), time.Second, cfg, LayeredOptions{MaxExpiryEntries: 8})
	now := time.Now()
	for i := range 10000 {
		store.setLocalExpiry(i%8, now)
	}
	if len(store.expiry) != 8 || store.order.Len() != 8 {
		t.Fatal("overwrites retained old metadata")
	}
	store.setLocalExpiry(9, now.Add(time.Second))
	if len(store.expiry) != 1 || store.order.Len() != 1 {
		t.Fatal("cold expired entries were not reclaimed")
	}
	store.clearLocalExpiry(9)
	if len(store.expiry) != 0 || store.order.Len() != 0 {
		t.Fatal("delete retained metadata")
	}
}

// Layered 的 L1 副本只在自己的 TTL 窗口内可信：窗口过后（或 ttl≤0）它只是旧缓存，不能凭 Stale 否决权威（L2）的值；远端已接受的写
// 也不能因为 L1 的旧副本拒写而报 ErrStaleWrite。两个副本 A、B 共用同一个权威；B 删除后以较低版本开始新的生命周期，A 的 L1 仍记着
// 上一个生命周期的 v5。L1 窗口由 expireLayeredWindow 直接置为过期，不靠 sleep 等 TTL。
// 历史：RR-20261004-02。
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

type staleValue struct {
	Key     int
	Version int64
	Payload string
}

func staleConfig() StoreConfig[int, staleValue] {
	return StoreConfig[int, staleValue]{
		KeyOf: func(v staleValue) int { return v.Key },
		Stale: func(old, next staleValue) bool { return old.Version > next.Version },
	}
}

// A dropped write must be distinguishable from a completed one. Returning nil
// is how a caller ends up reporting success for a mutation the store refused —
// a cancel that answers OK while the store still holds the old state.
func TestStaleSetIsReportedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	store := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 2})

	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "new"}); err != nil {
		t.Fatal(err)
	}
	err := store.Set(ctx, staleValue{Key: 1, Version: 4, Payload: "old"})
	if !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale Set returned %v, want ErrStaleWrite", err)
	}
	// And the refusal must be real: the stored value is untouched.
	got, ok, getErr := store.Get(ctx, 1)
	if getErr != nil || !ok {
		t.Fatalf("Get after refused write: ok=%v err=%v", ok, getErr)
	}
	if got.Payload != "new" || got.Version != 5 {
		t.Fatalf("refused write still landed: %+v", got)
	}
	// An equal version is not stale under this predicate, so it must be
	// accepted — the sentinel must not leak into the non-stale path.
	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "equal"}); err != nil {
		t.Fatalf("equal-version Set returned %v, want nil", err)
	}
}

// The same contract has to hold for a store with no prior value: there is
// nothing to be stale against, so the write must be accepted.
func TestStaleSetAcceptsFirstWrite(t *testing.T) {
	store := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 2})
	if err := store.Set(context.Background(), staleValue{Key: 7, Version: 1}); err != nil {
		t.Fatalf("first Set returned %v, want nil", err)
	}
}

// A LayeredStore with no TTL configured must not serve from L1 forever. A
// missing config key yields 0 from viper.GetDuration, so treating 0 as
// "cache forever" turns an unset option into a permanently stale first level —
// every replica reading its own private view.
func TestLayeredStoreWithoutTTLAlwaysRevalidates(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})

	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v1"}); err != nil {
		t.Fatal(err)
	}
	layered := NewLayeredStore[int, staleValue](local, remote, 0, cfg)

	if got, ok, err := layered.Get(ctx, 1); err != nil || !ok || got.Payload != "v1" {
		t.Fatalf("first read: %+v ok=%v err=%v", got, ok, err)
	}
	// Remote moves on. With ttl == 0 the L1 must not be trusted, so the next
	// read has to observe the new value.
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("second read: ok=%v err=%v", ok, err)
	}
	if got.Payload != "v2" {
		t.Fatalf("served a stale L1 copy %q; ttl<=0 must mean always revalidate", got.Payload)
	}
}

// With a TTL configured the L1 is authoritative for that long — the fix must
// not disable layering altogether.
func TestLayeredStoreServesFromL1WithinTTL(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v1"}); err != nil {
		t.Fatal(err)
	}
	layered := NewLayeredStore[int, staleValue](local, remote, time.Minute, cfg)

	if _, _, err := layered.Get(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("read within TTL: ok=%v err=%v", ok, err)
	}
	if got.Payload != "v1" {
		t.Fatalf("L1 was not used within its TTL: got %q", got.Payload)
	}
}

// A store with only a local level must keep working: Get short-circuits before
// consulting localValid, so the ttl<=0 change must not make it unreadable.
func TestLayeredStoreWithoutRemoteStillServesLocal(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	layered := NewLayeredStore[int, staleValue](local, nil, 0, cfg)

	if err := layered.Set(ctx, staleValue{Key: 1, Payload: "only"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok || got.Payload != "only" {
		t.Fatalf("local-only layered store: %+v ok=%v err=%v", got, ok, err)
	}
}
