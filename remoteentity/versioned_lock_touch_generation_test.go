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
//   - 其余 token 续期成功，按 token 计数，并在首次续期时通知 firstRenewal。
type touchGenerationStub struct {
	*unlockEvalStub
	tmu          sync.Mutex
	expired      map[string]bool
	blocked      map[string]chan struct{}
	blockedEnter chan string
	renewals     map[string]int
	firstRenewal map[string]chan struct{}
}

func newTouchGenerationStub() *touchGenerationStub {
	return &touchGenerationStub{
		unlockEvalStub: newUnlockEvalStub(),
		expired:        map[string]bool{},
		blocked:        map[string]chan struct{}{},
		blockedEnter:   make(chan string, 16),
		renewals:       map[string]int{},
		firstRenewal:   map[string]chan struct{}{},
	}
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
	close(release) // 旧 goroutine 的迟到回复（-1）到达并退出。
	select {
	case <-stub.renewedSignal(newToken):
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("new generation got no renewal goroutine: acquired=%v renewals=%d (interval 2ms)", lock.IsAcquired(), stub.renewalCount(newToken))
	}
	// 旧 goroutine 退出后，新代际的续期仍持续。
	before := stub.renewalCount(newToken)
	deadline := time.Now().Add(500 * time.Millisecond)
	for stub.renewalCount(newToken) < before+3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := stub.renewalCount(newToken); got < before+3 {
		t.Fatalf("new generation renewal stopped after the old goroutine exited: before=%d after=%d", before, got)
	}
	if !lock.IsAcquired() {
		t.Fatal("new generation lost acquisition")
	}
	if err := lock.Unlock(context.Background(), 1, time.Second); err != nil {
		t.Fatalf("unlock new generation: %v", err)
	}
	// Unlock 会等全部续期 goroutine 退出；之后不能再有续期。
	after := stub.renewalCount(newToken)
	time.Sleep(10 * time.Millisecond)
	if got := stub.renewalCount(newToken); got != after {
		t.Fatalf("renewal continued after Unlock: %d -> %d", after, got)
	}
	// 每个代际至多一个续期 goroutine，且都随代际结束退出，不随加锁次数累积。
	deadline = time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("renewal goroutines leaked: baseline=%d now=%d", baseline, n)
	}
}

// RR-20260926-43 高迭代压测：真实 goroutine、无状态注入。上一代际由续期 goroutine 自己观察到失效，
// 持有方见 !IsAcquired 跳过 Unlock，下一写者立即 TryLock；新代际必须在有界时间内获得续期。
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
				}
				if err := lock.TryLock(context.Background()); err != nil {
					t.Error(err)
					return
				}
				token := currentLockToken(lock)
				select {
				case <-stub.renewedSignal(token):
				case <-time.After(200 * time.Millisecond):
					if lock.IsAcquired() {
						lost.Add(1)
						firstLost.CompareAndSwap(nil, token)
					}
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
