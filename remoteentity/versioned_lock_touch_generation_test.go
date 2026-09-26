package remoteentity

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// touchGenerationStub 在 unlockEvalStub 之上模拟续期与刷新脚本：
//   - expired 中的 token 已在 Redis 失效（touch 回 -1、refresh 回 0，并清掉 owner）；
//   - blocked 中的 token 的 touch 在 Redis 端挂起，直到测试放行或调用方取消；
//   - 其余 token 续期成功，按 token 计数，并在首次续期时通知 firstRenewal；每次续期都关闭并替换 renewed，
//     供回归按事件等待“续期次数达到 n”，不依赖固定时间窗口（RR-20260926-63）。
type touchGenerationStub struct {
	*unlockEvalStub
	tmu          sync.Mutex
	expired      map[string]bool
	blocked      map[string]chan struct{}
	blockedEnter chan string
	renewals     map[string]int
	firstRenewal map[string]chan struct{}
	renewed      chan struct{}
}

// touchHangGuard 只防测试挂死，不是正确性窗口：断言都由事件（续期发生、goroutine 登记 / 退出）决定，
// 负载下续期晚到只会让等待更久，不会让回归变红（RR-20260926-63）。
const touchHangGuard = 30 * time.Second

func newTouchGenerationStub() *touchGenerationStub {
	return &touchGenerationStub{
		unlockEvalStub: newUnlockEvalStub(),
		expired:        map[string]bool{},
		blocked:        map[string]chan struct{}{},
		blockedEnter:   make(chan string, 16),
		renewals:       map[string]int{},
		firstRenewal:   map[string]chan struct{}{},
		renewed:        make(chan struct{}),
	}
}

// awaitRenewals 等到 token 的续期次数达到 n；只有超过 touchHangGuard 才返回 false。
func (s *touchGenerationStub) awaitRenewals(token string, n int) bool {
	guard := time.NewTimer(touchHangGuard)
	defer guard.Stop()
	for {
		s.tmu.Lock()
		count, changed := s.renewals[token], s.renewed
		s.tmu.Unlock()
		if count >= n {
			return true
		}
		select {
		case <-changed:
		case <-guard.C:
			return false
		}
	}
}

// registeredTouchGeneration 读取锁当前登记续期 goroutine 的代际（TryLock 在 l.mu 临界区内同步登记）。
func registeredTouchGeneration(l *versionedLock) string {
	l.touchMu.Lock()
	defer l.touchMu.Unlock()
	return l.touchGeneration
}

func (s *touchGenerationStub) dropOwner(token string) {
	s.unlockEvalStub.mu.Lock()
	if s.hash["owner"] == token {
		delete(s.hash, "owner")
	}
	s.unlockEvalStub.mu.Unlock()
}

func (s *touchGenerationStub) renewedSignal(token string) <-chan struct{} {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	ch, ok := s.firstRenewal[token]
	if !ok {
		ch = make(chan struct{})
		s.firstRenewal[token] = ch
	}
	return ch
}

func (s *touchGenerationStub) renewalCount(token string) int {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	return s.renewals[token]
}

func (s *touchGenerationStub) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	switch script {
	case versionedTouchLua:
		token := args[0].(string)
		s.tmu.Lock()
		release, blocked := s.blocked[token]
		s.tmu.Unlock()
		if blocked {
			s.blockedEnter <- token
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		s.tmu.Lock()
		defer s.tmu.Unlock()
		if s.expired[token] {
			s.dropOwner(token)
			return int64(-1), nil
		}
		s.renewals[token]++
		close(s.renewed)
		s.renewed = make(chan struct{})
		if s.renewals[token] == 1 {
			ch, ok := s.firstRenewal[token]
			if !ok {
				ch = make(chan struct{})
				s.firstRenewal[token] = ch
			}
			close(ch)
		}
		return int64(1000), nil
	case versionedRefreshLua:
		token := args[0].(string)
		s.tmu.Lock()
		expired := s.expired[token]
		s.tmu.Unlock()
		if expired {
			s.dropOwner(token)
			return int64(0), nil
		}
		return int64(1), nil
	default:
		return s.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
}

func currentLockToken(l *versionedLock) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.token
}

// RR-20260926-43 状态注入：上一代际的续期 goroutine 仍在退出路径上（它的 touch 挂在 Redis 端），
// 本地已观察到该代际失效；持有方据此跳过 Unlock，下一写者在同一锁对象上 TryLock。
// 新代际必须得到自己的续期，旧 goroutine 的迟到返回也不能关掉新代际的续期。
func TestVersionedLockNewGenerationRenewsWhileOldTouchGoroutineExits(t *testing.T) {
	baseline := runtime.NumGoroutine()
	stub := newTouchGenerationStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{
		TTL: 30 * time.Millisecond, AutoAsyncTouch: true,
		AsyncTouchInterval: 2 * time.Millisecond, AsyncTouchExtend: 15 * time.Millisecond,
	})
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldToken := currentLockToken(lock)
	release := make(chan struct{})
	stub.tmu.Lock()
	stub.expired[oldToken] = true
	stub.blocked[oldToken] = release
	stub.tmu.Unlock()
	select {
	case <-stub.blockedEnter:
	case <-time.After(2 * time.Second):
		t.Fatal("old generation renewal goroutine never reached Redis")
	}
	// 本代际在 Redis 已失效，本地观察到后 acquired=false；旧续期 goroutine 仍卡在 touch 回复上。
	if err := lock.Refresh(context.Background()); err == nil || lock.IsAcquired() {
		t.Fatalf("setup: old generation must be observed expired, err=%v acquired=%v", err, lock.IsAcquired())
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("next writer TryLock: %v", err)
	}
	newToken := currentLockToken(lock)
	if newToken == oldToken {
		t.Fatal("setup: TryLock reused the old token")
	}
	if got := registeredTouchGeneration(lock); got != newToken {
		t.Fatalf("new generation got no renewal goroutine: registered generation=%q, want the new token (old token %q)", got, oldToken)
	}
	close(release) // 旧 goroutine 的迟到回复（-1）到达并退出。
	if !stub.awaitRenewals(newToken, 1) {
		t.Fatalf("new generation never renewed: acquired=%v renewals=%d", lock.IsAcquired(), stub.renewalCount(newToken))
	}
	// 旧 goroutine 退出后，新代际的续期仍持续，登记也没有被旧 goroutine 的退出清掉。
	before := stub.renewalCount(newToken)
	if !stub.awaitRenewals(newToken, before+3) {
		t.Fatalf("new generation renewal stopped after the old goroutine exited: before=%d after=%d", before, stub.renewalCount(newToken))
	}
	if got := registeredTouchGeneration(lock); got != newToken {
		t.Fatalf("old goroutine's exit cleared the new generation's registration: registered=%q", got)
	}
	if !lock.IsAcquired() {
		t.Fatal("new generation lost acquisition")
	}
	if err := lock.Unlock(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("unlock new generation: %v", err)
	}
	// Unlock 会等全部续期 goroutine 退出（touchWg）；之后登记为空，不能再有续期。
	if got := registeredTouchGeneration(lock); got != "" {
		t.Fatalf("renewal still registered after Unlock: %q", got)
	}
	after := stub.renewalCount(newToken)
	time.Sleep(10 * time.Millisecond)
	if got := stub.renewalCount(newToken); got != after {
		t.Fatalf("renewal continued after Unlock: %d -> %d", after, got)
	}
	// 每个代际至多一个续期 goroutine，且都随代际结束退出，不随加锁次数累积。
	deadline := time.Now().Add(touchHangGuard)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("renewal goroutines leaked: baseline=%d now=%d", baseline, n)
	}
}

// RR-20260926-43 高迭代压测：真实 goroutine、无状态注入。上一代际由续期 goroutine 自己观察到失效，
// 持有方见 !IsAcquired 跳过 Unlock，下一写者立即 TryLock；新代际必须登记自己的续期 goroutine 并真的续期。
// 判定按事件（RR-20260926-63）：TryLock 返回时同步检查登记的代际，再等到新代际的第一次续期；
// 原先 200ms 的等待窗口在 race + 负载下会把晚到的续期误判为漏启动（1,000,000 次中 2 次）。
func TestVersionedLockTouchRestartStressNoLostRenewal(t *testing.T) {
	const iterations = 20000
	workers := runtime.GOMAXPROCS(0) / 2
	if workers < 2 {
		workers = 2
	}
	var next, lost atomic.Int64
	var firstLost atomic.Value
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= iterations {
				stub := newTouchGenerationStub()
				lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: 3 * time.Millisecond, AutoAsyncTouch: true, AsyncTouchInterval: time.Millisecond, AsyncTouchExtend: time.Millisecond})
				if err := lock.TryLock(context.Background()); err != nil {
					t.Error(err)
					return
				}
				stub.tmu.Lock()
				stub.expired[currentLockToken(lock)] = true
				stub.tmu.Unlock()
				for lock.IsAcquired() {
					runtime.Gosched()
				}
				if err := lock.TryLock(context.Background()); err != nil {
					t.Error(err)
					return
				}
				token := currentLockToken(lock)
				switch {
				case registeredTouchGeneration(lock) != token:
					// 修前的漏启动：旧代际的登记还在，新代际没有续期 goroutine。
					lost.Add(1)
					firstLost.CompareAndSwap(nil, token)
				case !stub.awaitRenewals(token, 1):
					t.Errorf("generation %s registered a renewal goroutine but never renewed within the hang guard", token)
				}
				lock.stopAsyncTouch()
			}
		}()
	}
	wg.Wait()
	t.Logf("iterations=%d workers=%d generations left without renewal=%d", iterations, workers, lost.Load())
	if n := lost.Load(); n != 0 {
		t.Fatalf("%d/%d new generations never got a renewal goroutine", n, iterations)
	}
}
