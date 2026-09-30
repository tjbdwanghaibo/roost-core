//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RR-20260926-43 后续验证（B27 第 4 批）：在真实 Redis 上构造"续期 goroutine 观察到锁失效 → 下一写者立即加锁"的时序。
// 修复记录里的回归只用 Eval stub 控制回复；这里 Lua 全部由真实 Redis 执行，失效点用注入的方式确定：
// 在旧代际的某一次 touch 到达 Redis 之前，对锁键执行 PEXPIRE 0（租约到期的确定性替身），这一次 touch 必然回 -1，
// 旧代际的续期 goroutine 由此观察到失效。之后按持有方 releaseRemoteEntries 的行为——见 !IsAcquired 跳过 Unlock——
// 立即在同一锁对象上 TryLock，断言新代际登记了自己的续期并在真实 Redis 上续到了期，旧代际的迟到 -1 不影响它。
// 键前缀 b27b4:<pid>:<ts>:…，用例结束逐键删除。

// b27TouchReply 记录一次真实 touch 的 token 与 Lua 返回值（-1 = owner 不是本 token）。
type b27TouchReply struct {
	token string
	value int64
}

// b27TouchProbe 包住真实 Redis：只观察与延后 touch，脚本本身不改、不替身。
//   - expireBefore：下一次以该 token 发出的 touch 到达 Redis 前，先让锁键到期（PEXPIRE 0），这是注入的失效点；
//   - hold：该 token 的 touch 在真实 Redis 已回复后不立即返回，等测试放行——用来把"旧 goroutine 仍在退出路径上"钉住；
//   - log：全部 touch 回复按顺序记录，await 按事件等待，不用固定时间窗口。
type b27TouchProbe struct {
	fredis.IRedis
	mu           sync.Mutex
	expireBefore string
	hold         map[string]chan struct{}
	held         chan string
	log          []b27TouchReply
	changed      chan struct{}
}

func newB27TouchProbe(r fredis.IRedis) *b27TouchProbe {
	return &b27TouchProbe{IRedis: r, hold: map[string]chan struct{}{}, held: make(chan string, 16), changed: make(chan struct{})}
}

func (p *b27TouchProbe) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedTouchLua {
		return p.IRedis.Eval(ctx, script, keys, args...)
	}
	token := args[0].(string)
	p.mu.Lock()
	expire := p.expireBefore == token
	if expire {
		p.expireBefore = ""
	}
	release := p.hold[token]
	p.mu.Unlock()
	if expire {
		// 注入的失效点：租约在这一次 touch 之前到期。用不受 evalCtx 影响的 ctx，保证失效一定发生。
		expireCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := p.IRedis.Eval(expireCtx, `return redis.call("PEXPIRE", KEYS[1], 0)`, keys)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("inject lease expiry: %w", err)
		}
	}
	value, err := p.IRedis.Eval(ctx, script, keys, args...)
	if err == nil {
		if n, parseErr := toInt64(value); parseErr == nil {
			p.mu.Lock()
			p.log = append(p.log, b27TouchReply{token: token, value: n})
			close(p.changed)
			p.changed = make(chan struct{})
			p.mu.Unlock()
		}
	}
	if release != nil {
		// 真实 Redis 已经回复；把回复压住，让旧代际的 goroutine 停在"已失效、尚未退出"的位置。
		// 不看 ctx：放行后回复原样送达，走的是迟到 -1 回复的路径。
		p.held <- token
		<-release
	}
	return value, err
}

// await 等到 touch 记录满足 pred；超过 touchHangGuard 返回 false（只防挂死，不是正确性窗口）。
func (p *b27TouchProbe) await(pred func([]b27TouchReply) bool) bool {
	guard := time.NewTimer(touchHangGuard)
	defer guard.Stop()
	for {
		p.mu.Lock()
		entries := append([]b27TouchReply(nil), p.log...)
		changed := p.changed
		p.mu.Unlock()
		if pred(entries) {
			return true
		}
		select {
		case <-changed:
		case <-guard.C:
			return false
		}
	}
}

func (p *b27TouchProbe) count(token string, ok bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.log {
		if e.token == token && (e.value > 0) == ok {
			n++
		}
	}
	return n
}

func (p *b27TouchProbe) awaitRenewals(token string, n int) bool {
	return p.await(func([]b27TouchReply) bool { return p.count(token, true) >= n })
}

func (p *b27TouchProbe) awaitExpiryObserved(token string) bool {
	return p.await(func([]b27TouchReply) bool { return p.count(token, false) >= 1 })
}

// renewedAfterExpiry 报告 token 在拿到 -1 之后是否还有成功的续期（被替换的代际不得再续期）。
func (p *b27TouchProbe) renewedAfterExpiry(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	expired := false
	for _, e := range p.log {
		if e.token != token {
			continue
		}
		if e.value < 0 {
			expired = true
		} else if expired {
			return true
		}
	}
	return false
}

// injectExpiryBefore 让 token 的下一次 touch 之前租约到期；hold 非 nil 时同一次 touch 的回复也被压住，
// 两个开关在同一临界区设置，保证被压住的正是那次拿到 -1 的 touch。
func (p *b27TouchProbe) injectExpiryBefore(token string, hold bool) chan struct{} {
	var release chan struct{}
	p.mu.Lock()
	p.expireBefore = token
	if hold {
		release = make(chan struct{})
		p.hold[token] = release
	}
	p.mu.Unlock()
	return release
}

// awaitFewerGoroutines 等到 goroutine 数低于 n（旧续期 goroutine 退出）；只受 touchHangGuard 限制。
func awaitFewerGoroutines(n int) bool {
	deadline := time.Now().Add(touchHangGuard)
	for runtime.NumGoroutine() >= n {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// b27LockKey 是本批用例的隔离锁键：b27b4:<pid>:<ts>:<用例>:<随机>，结束时连 :fence 一起删除。
func b27LockKey(t *testing.T, r fredis.IRedis) string {
	t.Helper()
	key := fmt.Sprintf("b27b4:%d:%d:%s:%s", os.Getpid(), time.Now().UnixNano(), t.Name(), generateToken())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.Del(ctx, key, key+":fence"); err != nil {
			t.Errorf("cleanup %s: %v", key, err)
		}
	})
	return key
}

func b27RedisOwner(t *testing.T, r fredis.IRedis, key string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner, err := r.HGet(ctx, key, "owner")
	if errors.Is(err, fredis.ErrNil) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(owner), true
}

func b27RedisPTTL(t *testing.T, r fredis.IRedis, key string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := r.Eval(ctx, `return redis.call("PTTL", KEYS[1])`, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := toInt64(v)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// b27 用例的锁参数：TTL 2s，20ms 一次续期、每次 +500ms（上限 2×TTL）。续期够密，用例只按事件等待。
var b27LockOptions = fredis.VersionedLockOptions{TTL: 2 * time.Second, AutoAsyncTouch: true, AsyncTouchInterval: 20 * time.Millisecond, AsyncTouchExtend: 500 * time.Millisecond}

// 确定性时序 1：旧代际的续期 goroutine 已经从真实 Redis 拿到 -1 但回复被压住（它仍存活、尚未清登记）；
// 持有方经 Refresh 观察到失效（真实 refresh Lua 回 0），跳过 Unlock，下一写者立即 TryLock。
// 放行迟到的 -1 后，新代际必须仍持有、仍在真实 Redis 上续期，最后能正常 Unlock。
func TestRealVersionedLockNextWriterLocksWhileOldRenewalReplyIsLate(t *testing.T) {
	r := realRemoteRedis(t)
	probe := newB27TouchProbe(r)
	lock := newVersionedLock(probe, 42, b27LockOptions)
	lock.key = b27LockKey(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := lock.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	oldToken, oldFence := currentLockToken(lock), lock.Fence()
	if !probe.awaitRenewals(oldToken, 1) {
		t.Fatal("old generation never renewed on real Redis")
	}
	release := probe.injectExpiryBefore(oldToken, true)
	select {
	case held := <-probe.held:
		if held != oldToken {
			t.Fatalf("held token %q, want old generation %q", held, oldToken)
		}
	case <-time.After(touchHangGuard):
		t.Fatal("old generation's touch never reached Redis after the injected expiry")
	}
	// 真实 Redis 上锁键已到期；旧 goroutine 还停在回复上，本地仍认为持有。
	if _, exists := b27RedisOwner(t, r, lock.key); exists {
		t.Fatal("injected expiry did not remove the lease from Redis")
	}
	if !lock.IsAcquired() {
		t.Fatal("setup: local state must still be acquired while the -1 reply is held")
	}
	if err := lock.Refresh(ctx); !errors.Is(err, ErrVersionedLockExpired) || lock.IsAcquired() {
		t.Fatalf("holder must observe expiry via real refresh: err=%v acquired=%v", err, lock.IsAcquired())
	}
	// 持有方见 !IsAcquired 跳过 Unlock；下一写者立即在同一锁对象上加锁。
	if err := lock.TryLock(ctx); err != nil {
		t.Fatalf("next writer TryLock: %v", err)
	}
	newToken := currentLockToken(lock)
	if newToken == oldToken {
		t.Fatal("TryLock reused the old token")
	}
	if lock.Fence() <= oldFence {
		t.Fatalf("fence did not advance across the expiry: old=%d new=%d", oldFence, lock.Fence())
	}
	if got := registeredTouchGeneration(lock); got != newToken {
		t.Fatalf("new generation got no renewal goroutine: registered=%q want %q (old %q)", got, newToken, oldToken)
	}
	if owner, ok := b27RedisOwner(t, r, lock.key); !ok || owner != newToken {
		t.Fatalf("Redis owner=%q ok=%v, want new token %q", owner, ok, newToken)
	}
	if !probe.awaitExpiryObserved(oldToken) {
		t.Fatal("old generation's -1 reply was never recorded")
	}
	goroutinesWhileOldAlive := runtime.NumGoroutine()
	close(release) // 旧代际的迟到 -1 现在到达 touchLease，旧 goroutine 随后退出。
	if !awaitFewerGoroutines(goroutinesWhileOldAlive) {
		t.Fatalf("old renewal goroutine did not exit after its late -1 reply (goroutines=%d)", runtime.NumGoroutine())
	}
	if !lock.IsAcquired() || currentLockToken(lock) != newToken {
		t.Fatalf("late -1 of the old generation killed the new one: acquired=%v token=%q", lock.IsAcquired(), currentLockToken(lock))
	}
	if got := registeredTouchGeneration(lock); got != newToken {
		t.Fatalf("old goroutine's exit cleared the new generation's registration: %q", got)
	}
	before := probe.count(newToken, true)
	if !probe.awaitRenewals(newToken, before+3) {
		t.Fatalf("new generation stopped renewing after the old goroutine exited: renewals=%d", probe.count(newToken, true))
	}
	if ttl := b27RedisPTTL(t, r, lock.key); ttl <= b27LockOptions.TTL.Milliseconds() {
		t.Fatalf("real Redis lease was not extended by the new generation: PTTL=%dms", ttl)
	}
	if probe.renewedAfterExpiry(oldToken) {
		t.Fatal("replaced generation renewed on real Redis after observing expiry")
	}
	// Unlock 先等全部续期 goroutine 退出；若迟到的 -1 错误地清了新代际的 acquired，这里会得到 NotOwned。
	if err := lock.Unlock(ctx, 7, time.Second); err != nil {
		t.Fatalf("unlock new generation: %v", err)
	}
	if got := registeredTouchGeneration(lock); got != "" {
		t.Fatalf("renewal still registered after Unlock: %q", got)
	}
	if _, exists := b27RedisOwner(t, r, lock.key); exists {
		t.Fatal("Unlock left an owner in Redis")
	}
	if err := lock.Unlock(ctx, 8, time.Second); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("second unlock=%v", err)
	}
}

// 确定性时序 2：旧代际由续期 goroutine 自己观察到失效（真实 touch 回 -1 → acquired=false），持有方见 !IsAcquired 跳过 Unlock，
// 下一写者立即 TryLock。同一锁对象连续换代 iterations 次，偶数轮先等旧 goroutine 清掉登记再加锁（"旧 goroutine 已退出"），
// 奇数轮观察到失效就加锁（"旧 goroutine 可能仍在退出"）；两种顺序下新代际都必须登记并在真实 Redis 上续期。
func TestRealVersionedLockRenewalGoroutineObservesExpiryThenNextWriterLocks(t *testing.T) {
	const iterations = 40
	r := realRemoteRedis(t)
	probe := newB27TouchProbe(r)
	lock := newVersionedLock(probe, 42, b27LockOptions)
	lock.key = b27LockKey(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := lock.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	var exitedFirst, stillExiting int
	for i := 0; i < iterations; i++ {
		oldToken := currentLockToken(lock)
		if !probe.awaitRenewals(oldToken, 1) {
			t.Fatalf("iteration %d: generation %q never renewed on real Redis", i, oldToken)
		}
		probe.injectExpiryBefore(oldToken, false)
		if !probe.awaitExpiryObserved(oldToken) {
			t.Fatalf("iteration %d: renewal goroutine never observed the injected expiry", i)
		}
		// 续期 goroutine 观察到失效：acquired 随即为 false（touchLease 的 -1 分支）。持有方据此跳过 Unlock。
		waitUntil := func(cond func() bool) bool {
			deadline := time.Now().Add(touchHangGuard)
			for !cond() {
				if time.Now().After(deadline) {
					return false
				}
				runtime.Gosched()
			}
			return true
		}
		if !waitUntil(func() bool { return !lock.IsAcquired() }) {
			t.Fatalf("iteration %d: local state never observed the expiry", i)
		}
		if i%2 == 0 {
			if !waitUntil(func() bool { return registeredTouchGeneration(lock) == "" }) {
				t.Fatalf("iteration %d: old renewal goroutine never cleared its registration", i)
			}
		}
		if registeredTouchGeneration(lock) == "" {
			exitedFirst++
		} else {
			stillExiting++
		}
		if err := lock.TryLock(ctx); err != nil {
			t.Fatalf("iteration %d: next writer TryLock: %v", i, err)
		}
		newToken := currentLockToken(lock)
		if newToken == oldToken {
			t.Fatalf("iteration %d: TryLock reused the old token", i)
		}
		if got := registeredTouchGeneration(lock); got != newToken {
			t.Fatalf("iteration %d: new generation got no renewal goroutine: registered=%q want %q", i, got, newToken)
		}
		if !probe.awaitRenewals(newToken, 1) {
			t.Fatalf("iteration %d: new generation never renewed on real Redis", i)
		}
		if owner, ok := b27RedisOwner(t, r, lock.key); !ok || owner != newToken {
			t.Fatalf("iteration %d: Redis owner=%q ok=%v, want %q", i, owner, ok, newToken)
		}
		if probe.renewedAfterExpiry(oldToken) {
			t.Fatalf("iteration %d: replaced generation %q renewed on real Redis after observing expiry", i, oldToken)
		}
	}
	t.Logf("iterations=%d old goroutine exited before TryLock=%d still exiting at TryLock=%d", iterations, exitedFirst, stillExiting)
	last := currentLockToken(lock)
	if ttl := b27RedisPTTL(t, r, lock.key); ttl <= 0 {
		t.Fatalf("lease missing at the end: PTTL=%d", ttl)
	}
	if err := lock.Unlock(ctx, int64(iterations), time.Second); err != nil {
		t.Fatalf("unlock %q: %v", last, err)
	}
	if got := registeredTouchGeneration(lock); got != "" {
		t.Fatalf("renewal still registered after Unlock: %q", got)
	}
	if _, exists := b27RedisOwner(t, r, lock.key); exists {
		t.Fatal("Unlock left an owner in Redis")
	}
}
