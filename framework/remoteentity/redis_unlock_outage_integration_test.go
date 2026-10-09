//go:build integration

package remoteentity

// RR-20260930-21 真实 Redis：释放锁时 Redis 出错用尽重试（注入点只在客户端包装的 unlock 脚本上，Lua 全部由真实 Redis 执行），
// 之后 Redis 恢复：租约仍是上一代 token 时下一次 TryLock 在同一条脚本里换成新 token、新 fence（真实计数器递增）；
// 租约已被另一个持有者取得时走 NotAcquired 且不挤掉对方，对方释放后再取得。键前缀 roost-remote-it:<用例>:<随机>，用例结束逐键删除。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

var errRealUnlockOutage = errors.New("integration: injected Redis outage during unlock")

// unlockOutageProbe 包住真实客户端：fail 非空时 unlock 脚本不发往 Redis、直接回错，其余脚本原样透传。
type unlockOutageProbe struct {
	fredis.IRedis
	fail atomic.Pointer[error]
}

func (p *unlockOutageProbe) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script == versionedUnlockLua {
		if err := p.fail.Load(); err != nil {
			return nil, *err
		}
	}
	return p.IRedis.Eval(ctx, script, keys, args...)
}

func realOwner(t *testing.T, r fredis.IRedis, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner, err := r.HGet(ctx, key, "owner")
	if errors.Is(err, fredis.ErrNil) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(owner)
}

// realFailedUnlock 在真实 Redis 上取锁，然后让释放在客户端侧一直出错到重试用尽；返回失败前的 token 与 fence。
func realFailedUnlock(t *testing.T, probe *unlockOutageProbe, lock *versionedLock) (string, uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lock.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	token, fence := lockToken(lock), lock.Fence()
	outage := errRealUnlockOutage
	probe.fail.Store(&outage)
	err := lock.UnlockWithRetry(ctx, 5, time.Second, 2, time.Millisecond)
	probe.fail.Store(nil)
	if !errors.Is(err, errRealUnlockOutage) {
		t.Fatalf("premise: unlock during the outage = %v", err)
	}
	if got := realOwner(t, probe.IRedis, lock.key); got != token {
		t.Fatalf("premise: Redis owner after the failed unlock=%q, want the lease still held by %q", got, token)
	}
	return token, fence
}

func TestRealVersionedLockReacquiresAfterUnlockOutage(t *testing.T) {
	r := realRemoteRedis(t)
	probe := &unlockOutageProbe{IRedis: r}
	lock := newVersionedLock(probe, 42, fredis.VersionedLockOptions{TTL: 2 * time.Second})
	lock.key = remoteRedisKey(t, r)
	oldToken, oldFence := realFailedUnlock(t, probe, lock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("TryLock after the failed unlock (lease still ours on Redis) = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if newToken == oldToken {
		t.Fatalf("re-acquisition reused the old token %q", oldToken)
	}
	if lock.Fence() <= oldFence {
		t.Fatalf("fence=%d did not advance past %d", lock.Fence(), oldFence)
	}
	if got := realOwner(t, r, lock.key); got != newToken {
		t.Fatalf("Redis owner=%q, want the new token %q", got, newToken)
	}
	ttl, err := r.Eval(ctx, `return redis.call("PTTL",KEYS[1])`, []string{lock.key})
	if err != nil {
		t.Fatal(err)
	}
	if ttlMs, err := toInt64(ttl); err != nil || ttlMs <= 0 || ttlMs > 2000 {
		t.Fatalf("re-acquired lease PTTL=%v err=%v, want (0, 2000]", ttl, err)
	}
	if err := lock.Unlock(ctx, 6, time.Second); err != nil {
		t.Fatalf("unlock of the new generation: %v", err)
	}
	if got := realOwner(t, r, lock.key); got != "" {
		t.Fatalf("owner after unlock=%q, want released", got)
	}
	version, err := r.HGet(ctx, lock.key, "version")
	if err != nil || string(version) != "6" {
		t.Fatalf("version after unlock=%q err=%v, want 6", version, err)
	}
	if err := lock.UnlockWithRetry(ctx, 6, time.Second, 0, 0); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("second unlock=%v, want ErrVersionedLockNotOwned", err)
	}
}

func TestRealVersionedLockUnknownReleaseDefersToOtherHolder(t *testing.T) {
	r := realRemoteRedis(t)
	probe := &unlockOutageProbe{IRedis: r}
	key := remoteRedisKey(t, r)
	first := newVersionedLock(probe, 42, fredis.VersionedLockOptions{TTL: 2 * time.Second})
	first.key = key
	_, oldFence := realFailedUnlock(t, probe, first)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 租约按 TTL 过期（真实到期操作代替等待），另一个持有者取得。
	if _, err := r.Eval(ctx, `return redis.call("PEXPIRE",KEYS[1],0)`, []string{key}); err != nil {
		t.Fatal(err)
	}
	second := newVersionedLock(r, 42, fredis.VersionedLockOptions{TTL: 2 * time.Second})
	second.key = key
	if err := second.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("TryLock while another holder owns the lease = %v, want ErrVersionedLockNotAcquired", err)
	}
	if got := realOwner(t, r, key); got != lockToken(second) {
		t.Fatalf("the first lock's TryLock evicted the other holder: owner=%q", got)
	}
	if err := second.Unlock(ctx, 9, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := first.TryLock(ctx); err != nil {
		t.Fatalf("TryLock after the other holder released = %v", err)
	}
	if first.Version() != 9 || first.Fence() <= oldFence || first.Fence() <= second.Fence() {
		t.Fatalf("version=%d fence=%d (old %d, other holder %d)", first.Version(), first.Fence(), oldFence, second.Fence())
	}
	if err := first.Unlock(ctx, 10, time.Second); err != nil {
		t.Fatal(err)
	}
}
