//go:build integration

package remoteentity

// RR-20261004-01 真实 Redis：取锁脚本由真实 Redis 执行，注入点只在客户端包装上——执行后把回复换成 ctx 截止（回复丢失）、
// 不发出（Redis 不可达），或先回 ctx 截止、稍后再把同一条脚本按原参数发给 Redis（还在路上的旧代脚本迟到落地）。
// 之后的 TryLock 必须以 Redis 为准取回本锁对象较早一代持有的租约（新 token、真实 fence 计数器递增），
// 迟到的旧代脚本不能挤掉已取得的新代际。键前缀 roost-remote-it:<用例>:<随机>，用例结束逐键删除。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

var errRealAcquireOutage = errors.New("integration: injected Redis outage during acquire")

// acquireOutcomeProbe 包住真实客户端（含 unlockOutageProbe 的释放注入），按计划改写取锁脚本的结果。
type acquireOutcomeProbe struct {
	*unlockOutageProbe
	mu        sync.Mutex
	plan      []acquireFault
	held      []heldEval
	lastToken string
}

func (p *acquireOutcomeProbe) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedTryLockLua {
		return p.unlockOutageProbe.Eval(ctx, script, keys, args...)
	}
	p.mu.Lock()
	p.lastToken = args[0].(string)
	var fault acquireFault
	if len(p.plan) > 0 {
		fault, p.plan = p.plan[0], p.plan[1:]
	}
	if fault == acquireInFlight {
		p.held = append(p.held, heldEval{keys: keys, args: args})
	}
	p.mu.Unlock()
	switch fault {
	case acquireAppliedReplyLost:
		if _, err := p.IRedis.Eval(ctx, script, keys, args...); err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	case acquireNotSent:
		return nil, errRealAcquireOutage
	case acquireInFlight:
		return nil, context.DeadlineExceeded
	default:
		return p.IRedis.Eval(ctx, script, keys, args...)
	}
}

func (p *acquireOutcomeProbe) failNext(faults ...acquireFault) {
	p.mu.Lock()
	p.plan = append(p.plan, faults...)
	p.mu.Unlock()
}

func (p *acquireOutcomeProbe) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastToken
}

func (p *acquireOutcomeProbe) deliverHeld(t *testing.T, ctx context.Context) {
	t.Helper()
	p.mu.Lock()
	if len(p.held) == 0 {
		p.mu.Unlock()
		t.Fatal("no in-flight acquire script to deliver")
	}
	call := p.held[0]
	p.held = p.held[1:]
	p.mu.Unlock()
	if _, err := p.IRedis.Eval(ctx, versionedTryLockLua, call.keys, call.args...); err != nil {
		t.Fatalf("delivering the in-flight acquire script: %v", err)
	}
}

func realFenceCounter(t *testing.T, ctx context.Context, r fredis.IRedis, key string) uint64 {
	t.Helper()
	raw, err := r.Eval(ctx, `return redis.call("GET",KEYS[1])`, []string{key + ":fence"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := toInt64(raw)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(n)
}

func newAcquireOutcomeLock(t *testing.T) (*acquireOutcomeProbe, *versionedLock, fredis.IRedis) {
	r := realRemoteRedis(t)
	probe := &acquireOutcomeProbe{unlockOutageProbe: &unlockOutageProbe{IRedis: r}}
	lock := newVersionedLock(probe, 42, fredis.VersionedLockOptions{TTL: 2 * time.Second})
	lock.key = remoteRedisKey(t, r)
	return probe, lock, r
}

func TestRealVersionedLockReacquiresAfterAcquireReplyLost(t *testing.T) {
	probe, lock, r := newAcquireOutcomeLock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	probe.failNext(acquireAppliedReplyLost)
	if err := lock.TryLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: TryLock whose reply was lost = %v", err)
	}
	lost := probe.last()
	if got := realOwner(t, r, lock.key); got != lost {
		t.Fatalf("premise: Redis owner=%q, want the lost attempt's token %q", got, lost)
	}
	lostFence := realFenceCounter(t, ctx, r, lock.key)

	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("TryLock after an acquire whose reply was lost (real lease held by our own lost token) = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if newToken == lost {
		t.Fatalf("re-acquisition reused the lost token %q", lost)
	}
	if lock.Fence() <= lostFence || lock.Fence() != realFenceCounter(t, ctx, r, lock.key) {
		t.Fatalf("fence=%d, want the real counter advanced past %d", lock.Fence(), lostFence)
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
}

// 释放未知 → 取锁执行后丢回复 → 取锁没发出 → 恢复：真实 owner 是中间那一代，要能取回。
func TestRealVersionedLockReacquiresAcrossUnknownChain(t *testing.T) {
	probe, lock, r := newAcquireOutcomeLock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	released, _ := realFailedUnlock(t, probe.unlockOutageProbe, lock)

	probe.failNext(acquireAppliedReplyLost, acquireNotSent)
	if err := lock.TryLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: lost-reply TryLock = %v", err)
	}
	middle := probe.last()
	if got := realOwner(t, r, lock.key); got != middle || got == released {
		t.Fatalf("premise: owner=%q, want the lost attempt's %q (released %q)", got, middle, released)
	}
	if err := lock.TryLock(ctx); !errors.Is(err, errRealAcquireOutage) {
		t.Fatalf("premise: unsent TryLock = %v", err)
	}
	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("TryLock after recovery (real lease held by the middle unknown generation) = %v, want re-acquisition", err)
	}
	if got := realOwner(t, r, lock.key); got != lockToken(lock) || got == middle {
		t.Fatalf("Redis owner=%q, want the new token %q", got, lockToken(lock))
	}
	if err := lock.Unlock(ctx, 7, time.Second); err != nil {
		t.Fatal(err)
	}
}

// 迟到的旧代取锁脚本：新代际持有时落地不能挤掉它；新代际释放后落地取得了空闲租约，下一次 TryLock 要能取回。
// 另一个锁对象（别的进程）持有时，本锁对象不能借"较早一代"的判断挤掉它。
func TestRealVersionedLockLateAcquireScript(t *testing.T) {
	probe, lock, r := newAcquireOutcomeLock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	probe.failNext(acquireInFlight, acquireInFlight)
	for i := 0; i < 2; i++ {
		if err := lock.TryLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("premise: in-flight TryLock = %v", err)
		}
	}
	if err := lock.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	current := lockToken(lock)
	probe.deliverHeld(t, ctx)
	if got := realOwner(t, r, lock.key); got != current || !lock.IsAcquired() {
		t.Fatalf("the late earlier-generation script replaced the current generation: owner=%q want %q", got, current)
	}
	if err := lock.Unlock(ctx, 3, time.Second); err != nil {
		t.Fatal(err)
	}

	probe.deliverHeld(t, ctx)
	late := realOwner(t, r, lock.key)
	if late == "" || late == current {
		t.Fatalf("premise: owner after the second late script=%q", late)
	}
	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("TryLock after our own late acquire script took the free lease = %v, want re-acquisition", err)
	}
	if got := realOwner(t, r, lock.key); got != lockToken(lock) || got == late {
		t.Fatalf("Redis owner=%q, want the new token %q", got, lockToken(lock))
	}
	if err := lock.Unlock(ctx, 4, time.Second); err != nil {
		t.Fatal(err)
	}

	other := newVersionedLock(r, 42, fredis.VersionedLockOptions{TTL: 2 * time.Second})
	other.key = lock.key
	if err := other.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lock.TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("TryLock while another lock object holds the lease = %v, want ErrVersionedLockNotAcquired", err)
	}
	if got := realOwner(t, r, lock.key); got != lockToken(other) {
		t.Fatalf("another lock object's lease was evicted: owner=%q", got)
	}
	if err := other.Unlock(ctx, 5, time.Second); err != nil {
		t.Fatal(err)
	}
}
