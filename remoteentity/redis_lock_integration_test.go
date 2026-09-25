//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

func realRemoteRedis(t *testing.T) fredis.IRedis {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with isolated Redis")
	}
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if addr == "" {
		t.Fatal("isolated Redis address required")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
func remoteRedisKey(t *testing.T, r fredis.IRedis) string {
	t.Helper()
	key := fmt.Sprintf("roost-remote-it:%s:%s", t.Name(), generateToken())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.Del(ctx, key, key+":fence"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return key
}
func TestRealVersionedLockExactCounters(t *testing.T) {
	r := realRemoteRedis(t)
	for _, version := range []int64{1<<53 + 1, math.MaxInt64} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			lock := newVersionedLock(r, 42, fredis.VersionedLockOptions{TTL: time.Second})
			lock.key = remoteRedisKey(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := r.Eval(ctx, `redis.call("HSET",KEYS[1],"version",ARGV[1]); redis.call("SET",KEYS[2],ARGV[2]); return 1`, []string{lock.key, lock.key + ":fence"}, version, version-1)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.TryLock(ctx); err != nil {
				t.Fatal(err)
			}
			if lock.Version() != version || lock.Fence() != uint64(version) {
				t.Fatalf("version=%d fence=%d want=%d", lock.Version(), lock.Fence(), version)
			}
			if err := lock.Unlock(ctx, version, time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRealVersionedLockCounterFailureDoesNotLeaveOwner(t *testing.T) {
	r := realRemoteRedis(t)
	lock := newVersionedLock(r, 42, fredis.VersionedLockOptions{TTL: time.Second})
	lock.key = remoteRedisKey(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.Eval(ctx, `return redis.call("SET",KEYS[1],ARGV[1])`, []string{lock.key + ":fence"}, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := lock.TryLock(ctx); err == nil {
		t.Fatal("overflow acquired")
	}
	if owner, err := r.HGet(ctx, lock.key, "owner"); !errors.Is(err, fredis.ErrNil) {
		t.Fatalf("failed allocation left owner=%q err=%v", owner, err)
	}
	// 修复夹具中的溢出计数后应能重新申请，不允许遗留无 TTL 的 owner 阻塞后续请求。
	if _, err := r.Eval(ctx, `return redis.call("SET",KEYS[1],0)`, []string{lock.key + ":fence"}); err != nil {
		t.Fatal(err)
	}
	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("retry after failed allocation: %v", err)
	}
	if err := lock.Unlock(ctx, 1, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRealVersionedLockRenewExpiryAndHandoff(t *testing.T) {
	firstRedis, secondRedis := realRemoteRedis(t), realRemoteRedis(t)
	key := remoteRedisKey(t, firstRedis)
	opts := fredis.VersionedLockOptions{TTL: time.Second}
	first, second := newVersionedLock(firstRedis, 42, opts), newVersionedLock(secondRedis, 42, opts)
	first.key, second.key = key, key
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("contender=%v", err)
	}
	oldFence := first.Fence()
	if err := first.Touch(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	ttl, err := firstRedis.Eval(ctx, `return redis.call("PTTL",KEYS[1])`, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	ttlMs, err := toInt64(ttl)
	if err != nil || ttlMs <= 1000 || ttlMs > 2000 {
		t.Fatalf("extended TTL=%v err=%v", ttl, err)
	}
	if err := first.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	// 用真实 Redis 到期操作控制交接时刻，不依赖调度 sleep。
	if _, err := firstRedis.Eval(ctx, `return redis.call("PEXPIRE",KEYS[1],0)`, []string{key}); err != nil {
		t.Fatal(err)
	}
	if err := second.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if second.Fence() <= oldFence {
		t.Fatal("fence did not survive lease expiry")
	}
	if err := first.Unlock(ctx, 99, time.Second); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("stale unlock=%v", err)
	}
	if err := second.Refresh(ctx); err != nil {
		t.Fatalf("old owner affected new lease: %v", err)
	}
	if err := second.Unlock(ctx, 7, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := first.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if first.Version() != 7 {
		t.Fatalf("version=%d", first.Version())
	}
	if err := first.Unlock(ctx, 8, time.Second); err != nil {
		t.Fatal(err)
	}
}
