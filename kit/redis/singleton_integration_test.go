//go:build integration

package redis

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// SingletonStore 在真实 Redis 上的语义（App 单实例锁方案 §8.1 kit 部分）：Acquire / Renew / Release /
// 丢键 / 他人持有 / Get 多键。键用每次运行随机的前缀，用例结束删除。
//
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./kit/redis/ -run Singleton
//
// Get 跨槽另在真实 Redis Cluster 上跑一次（ROOST_REVIEW_CLUSTER，入口 kit/scripts/integration/redis-cluster-suites.sh）。

func openSingletonStoreForTest(t *testing.T, cfg *viper.Viper) (app.SingletonStore, string) {
	t.Helper()
	store, err := SingletonStore(cfg)
	if err != nil {
		t.Fatalf("open singleton store: %v", err)
	}
	prefix := "roost:test:kitredis-singleton:" + rand.Text()
	t.Cleanup(func() { _ = store.Close() })
	return store, prefix
}

func singletonRedisStore(t *testing.T) (app.SingletonStore, string) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.addr", addr)
	return openSingletonStoreForTest(t, cfg)
}

func cleanupSingletonKeys(t *testing.T, store app.SingletonStore, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		values, err := store.Get(ctx, keys)
		if err != nil {
			return
		}
		for i, value := range values {
			if value != nil {
				_, _ = store.CompareAndDelete(ctx, keys[i], value)
			}
		}
	})
}

func TestSingletonStoreAcquireRenewRelease(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	mine := []byte("token-a|host|1|1")

	applied, current, err := store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || !applied || !bytes.Equal(current, mine) {
		t.Fatalf("acquire = %v %q %v, want applied", applied, current, err)
	}
	// 回复丢失后的重试：键已是自己的值 → Applied=false、current 等于自己（App 据此认领）。
	applied, current, err = store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, mine) {
		t.Fatalf("re-acquire = %v %q %v, want not applied with current = own value", applied, current, err)
	}
	applied, _, err = store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || !applied {
		t.Fatalf("renew = %v %v, want applied", applied, err)
	}
	applied, err = store.CompareAndDelete(ctx, key, mine)
	if err != nil || !applied {
		t.Fatalf("release = %v %v, want applied", applied, err)
	}
	values, err := store.Get(ctx, []string{key})
	if err != nil || values[0] != nil {
		t.Fatalf("after release Get = %q %v, want absent", values, err)
	}
}

func TestSingletonStoreRenewAfterTheKeyExpiredIsNotHeld(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	mine := []byte("token-a|host|1|1")
	if applied, _, err := store.CompareAndSet(ctx, key, nil, mine, 50*time.Millisecond); err != nil || !applied {
		t.Fatalf("acquire = %v %v", applied, err)
	}
	time.Sleep(150 * time.Millisecond) // 真实 Redis 的 TTL，只能等它过期
	applied, current, err := store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || applied || current != nil {
		t.Fatalf("renew after expiry = %v %q %v, want NotHeld with the key gone", applied, current, err)
	}
	if applied, err := store.CompareAndDelete(ctx, key, mine); err != nil || applied {
		t.Fatalf("release after expiry = %v %v, want not applied", applied, err)
	}
}

func TestSingletonStoreDoesNotTouchAnotherHoldersKey(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	other := []byte("token-b|other|2|1")
	mine := []byte("token-a|host|1|1")
	if applied, _, err := store.CompareAndSet(ctx, key, nil, other, 15*time.Second); err != nil || !applied {
		t.Fatalf("other acquire = %v %v", applied, err)
	}
	applied, current, err := store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, other) {
		t.Fatalf("acquire held key = %v %q %v, want not applied with the holder's value", applied, current, err)
	}
	applied, current, err = store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, other) {
		t.Fatalf("renew someone else's key = %v %q %v, want NotHeld", applied, current, err)
	}
	if applied, err := store.CompareAndDelete(ctx, key, mine); err != nil || applied {
		t.Fatalf("release someone else's key = %v %v, want not applied", applied, err)
	}
	values, err := store.Get(ctx, []string{key})
	if err != nil || !bytes.Equal(values[0], other) {
		t.Fatalf("holder's key = %q %v, want untouched", values, err)
	}
}

func TestSingletonStoreGetReadsEveryKeyInOrder(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	assertSingletonGetAcrossKeys(t, store, prefix)
}

// Cluster 下各 sid 的键落在不同槽：Get 不能用单条 MGET（CROSSSLOT）。
func TestSingletonStoreGetAcrossClusterSlots(t *testing.T) {
	addrs := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addrs == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.cluster_addrs", addrs)
	store, prefix := openSingletonStoreForTest(t, cfg)
	assertSingletonGetAcrossKeys(t, store, prefix)
}

func assertSingletonGetAcrossKeys(t *testing.T, store app.SingletonStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 8)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s:game:%d", prefix, 1000+i)
	}
	cleanupSingletonKeys(t, store, keys...)
	for _, i := range []int{1, 4, 6} {
		if applied, _, err := store.CompareAndSet(ctx, keys[i], nil, []byte(fmt.Sprintf("v%d", i)), 15*time.Second); err != nil || !applied {
			t.Fatalf("set %s = %v %v", keys[i], applied, err)
		}
	}
	values, err := store.Get(ctx, keys)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(values) != len(keys) {
		t.Fatalf("Get returned %d values for %d keys", len(values), len(keys))
	}
	for i, value := range values {
		want := ""
		if i == 1 || i == 4 || i == 6 {
			want = fmt.Sprintf("v%d", i)
		}
		if string(value) != want || (want == "" && value != nil) {
			t.Fatalf("Get[%d] = %q, want %q", i, value, want)
		}
	}
}
