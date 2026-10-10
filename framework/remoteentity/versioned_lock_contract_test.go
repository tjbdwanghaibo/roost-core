package remoteentity

import (
	"context"
	"errors"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"sync"
	"testing"
	"time"
)

type acquireFault int

const (
	acquireAppliedReplyLost acquireFault = iota + 1 // 脚本已在 Redis 执行，客户端只看到 ctx 截止
	acquireNotSent                                  // Redis 不可达，脚本没有执行
	acquireInFlight                                 // 脚本还在路上：客户端先看到 ctx 截止，脚本稍后由 deliverHeld 落地
)

type heldEval struct {
	keys []string
	args []any
}

// acquireOutcomeStub 在 outageEvalStub 之上按计划给取锁脚本注入"没有明确答复"的结果；计划用完后取锁脚本正常执行。
type acquireOutcomeStub struct {
	*outageEvalStub
	plan      []acquireFault
	held      []heldEval
	lastToken string // 最近一次到达替身的取锁 token（不论结果）
}

func newAcquireOutcomeStub() *acquireOutcomeStub {
	return &acquireOutcomeStub{outageEvalStub: newOutageEvalStub()}
}

func (s *acquireOutcomeStub) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedTryLockLua {
		return s.outageEvalStub.Eval(ctx, script, keys, args...)
	}
	s.state.Lock()
	s.lastToken = args[0].(string)
	var fault acquireFault
	if len(s.plan) > 0 {
		fault, s.plan = s.plan[0], s.plan[1:]
	}
	if fault == acquireInFlight {
		s.held = append(s.held, heldEval{keys: keys, args: args})
	}
	s.state.Unlock()
	switch fault {
	case acquireAppliedReplyLost:
		if _, err := s.outageEvalStub.Eval(ctx, script, keys, args...); err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	case acquireNotSent:
		return nil, errRedisOutage
	case acquireInFlight:
		return nil, context.DeadlineExceeded
	default:
		return s.outageEvalStub.Eval(ctx, script, keys, args...)
	}
}

func (s *acquireOutcomeStub) failNextAcquires(faults ...acquireFault) {
	s.state.Lock()
	s.plan = append(s.plan, faults...)
	s.state.Unlock()
}

func (s *acquireOutcomeStub) lastAcquireToken() string {
	s.state.Lock()
	defer s.state.Unlock()
	return s.lastToken
}

// deliverHeld 让最早一条还在路上的取锁脚本按原参数在替身里落地。
func (s *acquireOutcomeStub) deliverHeld(t *testing.T) {
	t.Helper()
	s.state.Lock()
	if len(s.held) == 0 {
		s.state.Unlock()
		t.Fatal("no in-flight acquire script to deliver")
	}
	call := s.held[0]
	s.held = s.held[1:]
	s.state.Unlock()
	if _, err := s.outageEvalStub.Eval(context.Background(), versionedTryLockLua, call.keys, call.args...); err != nil {
		t.Fatalf("delivering the in-flight acquire script: %v", err)
	}
}

func (s *acquireOutcomeStub) fenceCounter() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(s.fence)
}

// lostAcquire 让一次 TryLock 的脚本在 Redis 执行后丢掉回复，返回落在 Redis 上的 token 与 fence。
func lostAcquire(t *testing.T, stub *acquireOutcomeStub, lock *versionedLock) (string, uint64) {
	t.Helper()
	stub.failNextAcquires(acquireAppliedReplyLost)
	if err := lock.TryLock(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: TryLock whose reply was lost = %v, want context.DeadlineExceeded", err)
	}
	token := stub.lastAcquireToken()
	if got := stub.owner(); got == "" || got != token {
		t.Fatalf("premise: Redis owner after the lost reply=%q, want the attempted token %q", got, token)
	}
	if lock.IsAcquired() {
		t.Fatal("premise: a TryLock without a Redis answer reports acquired")
	}
	return token, stub.fenceCounter()
}

func TestVersionedLockReacquiresAfterAcquireReplyLost(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	lostToken, lostFence := lostAcquire(t, stub, lock)

	// Redis 上 owner 是上一次没拿到回复的 token：下一次 TryLock 必须以 Redis 为准取得新代际，而不是 NotAcquired 到 TTL。
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after an acquire whose reply was lost (lease held by our own lost token) = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if newToken == lostToken || newToken == "" {
		t.Fatalf("re-acquisition reused the lost token %q", lostToken)
	}
	if lock.Fence() <= lostFence {
		t.Fatalf("re-acquisition fence=%d did not advance past the lost attempt's %d", lock.Fence(), lostFence)
	}
	if got := stub.owner(); got != newToken {
		t.Fatalf("Redis owner=%q, want the new token %q", got, newToken)
	}
	if err := lock.Unlock(context.Background(), 5, time.Second); err != nil {
		t.Fatalf("unlock of the new generation: %v", err)
	}
	if got := stub.owner(); got != "" {
		t.Fatalf("owner after unlock=%q, want released", got)
	}
}

func TestVersionedLockLostAcquireDefersToOtherHolder(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	lostAcquire(t, stub, lock)

	// 丢回复的租约按 TTL 过期后被别的持有者取得：这一侧走 NotAcquired，不能把别人挤掉。
	stub.setOwner("someone-else")
	if err := lock.TryLock(context.Background()); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("TryLock while another holder owns the lease = %v, want ErrVersionedLockNotAcquired", err)
	}
	if got := stub.owner(); got != "someone-else" {
		t.Fatalf("TryLock evicted the other holder: owner=%q", got)
	}
	stub.setOwner("")
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the other holder released = %v", err)
	}
	if err := lock.Unlock(context.Background(), 5, time.Second); err != nil {
		t.Fatal(err)
	}
}

// 释放结果未知（RR-20260930-21）之后 Redis 仍不可达，下一次取锁根本没发出去：owner 仍是释放未知的那一代，恢复后要能取回。
// 只记"最近一次未知 token"的修法会在这里用没执行的 token 覆盖真正的 owner。
func TestVersionedLockUnsentAcquireKeepsUnknownReleaseReclaimable(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	releasedToken, _ := failedUnlock(t, stub.outageEvalStub, lock)
	stub.failNextAcquires(acquireNotSent, acquireNotSent)
	for i := 0; i < 2; i++ {
		if err := lock.TryLock(context.Background()); !errors.Is(err, errRedisOutage) {
			t.Fatalf("premise: TryLock during the outage = %v", err)
		}
	}
	if got := stub.owner(); got != releasedToken {
		t.Fatalf("premise: owner=%q, want the unknown-release token %q", got, releasedToken)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after Redis recovered (lease still held by the unknown-release generation) = %v, want re-acquisition", err)
	}
	if got := stub.owner(); got != lockToken(lock) || got == releasedToken {
		t.Fatalf("Redis owner=%q, want the new token %q", got, lockToken(lock))
	}
	if err := lock.Unlock(context.Background(), 6, time.Second); err != nil {
		t.Fatal(err)
	}
}

// 释放未知 → 一次取锁执行后丢回复 → 一次取锁没发出：Redis 上 owner 是中间那一代。只记首个或最近一个未知 token 都取不回它。
func TestVersionedLockReacquiresAcrossUnknownChain(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	releasedToken, _ := failedUnlock(t, stub.outageEvalStub, lock)
	stub.failNextAcquires(acquireAppliedReplyLost)
	if err := lock.TryLock(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: lost-reply TryLock = %v", err)
	}
	middle := stub.owner()
	if middle == "" || middle == releasedToken || middle != stub.lastAcquireToken() {
		t.Fatalf("premise: owner after the lost-reply acquire=%q (released %q)", middle, releasedToken)
	}
	stub.failNextAcquires(acquireNotSent)
	if err := lock.TryLock(context.Background()); !errors.Is(err, errRedisOutage) {
		t.Fatalf("premise: unsent TryLock = %v", err)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after Redis recovered (lease held by the middle unknown generation) = %v, want re-acquisition", err)
	}
	if got := stub.owner(); got != lockToken(lock) || got == middle {
		t.Fatalf("Redis owner=%q, want the new token %q", got, lockToken(lock))
	}
	if err := lock.Unlock(context.Background(), 6, time.Second); err != nil {
		t.Fatal(err)
	}
}

// RR-20260924-20：还在路上的旧代取锁脚本在新代际取得之后才落地，不能挤掉新代际。
func TestVersionedLockLateAcquireScriptCannotTakeNewGeneration(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	stub.failNextAcquires(acquireInFlight)
	if err := lock.TryLock(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: in-flight TryLock = %v", err)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock while the earlier script is still in flight (lease free) = %v", err)
	}
	current, fence := lockToken(lock), lock.Fence()
	stub.deliverHeld(t)
	if got := stub.owner(); got != current {
		t.Fatalf("the late earlier-generation acquire script replaced the current generation: owner=%q want %q", got, current)
	}
	if !lock.IsAcquired() || lock.Fence() != fence {
		t.Fatalf("current generation disturbed: acquired=%v fence=%d want %d", lock.IsAcquired(), lock.Fence(), fence)
	}
	if err := lock.Unlock(context.Background(), 4, time.Second); err != nil {
		t.Fatalf("unlock of the current generation after the late script: %v", err)
	}
}

// 还在路上的旧代取锁脚本在新代际明确释放之后才落地（租约空闲，于是它取得了）：这是本锁对象较早的一代，
// 下一次 TryLock 也要能以 Redis 为准取回，而不是 NotAcquired 到 TTL。
func TestVersionedLockReacquiresAfterLateAcquireScriptLands(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	stub.failNextAcquires(acquireInFlight)
	if err := lock.TryLock(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premise: in-flight TryLock = %v", err)
	}
	late := stub.lastAcquireToken()
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(context.Background(), 4, time.Second); err != nil {
		t.Fatal(err)
	}
	stub.deliverHeld(t)
	if got := stub.owner(); got != late {
		t.Fatalf("premise: owner after the late script landed=%q, want %q", got, late)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after our own late acquire script took the free lease = %v, want re-acquisition", err)
	}
	if got := stub.owner(); got != lockToken(lock) || got == late {
		t.Fatalf("Redis owner=%q, want the new token %q", got, lockToken(lock))
	}
	if err := lock.Unlock(context.Background(), 5, time.Second); err != nil {
		t.Fatal(err)
	}
}

// RR-20260926-43：丢回复的那一代从未在本地持有，不登记续期；重新取得的新代际登记并续期自己的 token。
func TestVersionedLockLostAcquireRenewalFollowsNewGeneration(t *testing.T) {
	stub := newAcquireOutcomeStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: 40 * time.Millisecond, AutoAsyncTouch: true, AsyncTouchInterval: 2 * time.Millisecond, AsyncTouchExtend: 20 * time.Millisecond})
	lostToken, _ := lostAcquire(t, stub, lock)
	if got := touchGenerationOf(lock); got != "" {
		t.Fatalf("renewal registered for %q after an acquire without a Redis answer, want none", got)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the lost reply = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if got := touchGenerationOf(lock); got != newToken {
		t.Fatalf("renewal registered for %q, want the new token %q", got, newToken)
	}
	if got := awaitTouch(t, stub.outageEvalStub); got != newToken {
		t.Fatalf("renewal used token %q, want the new token %q (lost %q)", got, newToken, lostToken)
	}
	if err := lock.Unlock(context.Background(), 5, time.Second); err != nil {
		t.Fatal(err)
	}
	for len(stub.touches) > 0 {
		if got := <-stub.touches; got == lostToken {
			t.Fatalf("the lost token %q was renewed", lostToken)
		}
	}
}

var errRedisOutage = errors.New("stub: redis unavailable")

// outageEvalStub 在 unlockEvalStub 的锁脚本模拟之上：unlockErr 非空时 unlock 脚本不落地、直接回错（Redis 故障）；
// touch 脚本按 owner 判定并把每次 touch 的 token 送到 touches；holdToken 的 touch 在到达后先等 release 放行（迟到回复）。
type outageEvalStub struct {
	*unlockEvalStub
	state     sync.Mutex
	unlockErr error
	touches   chan string
	holdToken string
	held      chan struct{}
	release   chan struct{}
}

func newOutageEvalStub() *outageEvalStub {
	return &outageEvalStub{unlockEvalStub: newUnlockEvalStub(), touches: make(chan string, 256)}
}

func (s *outageEvalStub) failUnlocks(err error) {
	s.state.Lock()
	s.unlockErr = err
	s.state.Unlock()
}

func (s *outageEvalStub) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	switch script {
	case versionedUnlockLua:
		s.state.Lock()
		err := s.unlockErr
		s.state.Unlock()
		if err != nil {
			s.mu.Lock()
			s.unlockCalls++
			s.mu.Unlock()
			return nil, err
		}
		return s.unlockEvalStub.Eval(ctx, script, keys, args...)
	case versionedTouchLua:
		token := args[0].(string)
		s.state.Lock()
		hold := s.holdToken == token
		held, release := s.held, s.release
		s.state.Unlock()
		if hold {
			close(held)
			<-release
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		select {
		case s.touches <- token:
		default:
		}
		if s.hash["owner"] != token {
			return int64(-1), nil
		}
		return int64(1000), nil
	default:
		return s.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
}

func (s *outageEvalStub) owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hash["owner"]
}

func (s *outageEvalStub) setOwner(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		delete(s.hash, "owner")
		return
	}
	s.hash["owner"] = token
}

func lockToken(l *versionedLock) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.token
}

// failedUnlock 取得锁，然后让 Redis 在释放时一直出错直到重试用尽；返回失败前的 token 与 fence。
func failedUnlock(t *testing.T, stub *outageEvalStub, lock *versionedLock) (string, uint64) {
	t.Helper()
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	token, fence := lockToken(lock), lock.Fence()
	stub.failUnlocks(errRedisOutage)
	before := stub.unlockCalls
	err := lock.UnlockWithRetry(context.Background(), 7, time.Second, 2, time.Millisecond)
	if !errors.Is(err, errRedisOutage) {
		t.Fatalf("premise: unlock during the Redis outage returned %v, want the Redis error after the retries", err)
	}
	if calls := stub.unlockCalls - before; calls != 3 {
		t.Fatalf("premise: unlock attempts=%d, want 3 (retries exhausted)", calls)
	}
	stub.failUnlocks(nil)
	if got := stub.owner(); got != token {
		t.Fatalf("premise: the failed unlock changed the Redis owner to %q", got)
	}
	return token, fence
}

func TestVersionedLockReacquiresAfterUnlockRetriesExhausted(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	oldToken, oldFence := failedUnlock(t, stub, lock)

	// Redis 恢复，租约仍是上一代 token：下一次 TryLock 必须以 Redis 为准重新取得，而不是看本地状态拒绝。
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after a failed unlock (Redis healthy again, lease still ours) = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if newToken == oldToken || newToken == "" {
		t.Fatalf("re-acquisition reused the old generation token %q", oldToken)
	}
	if lock.Fence() <= oldFence {
		t.Fatalf("re-acquisition fence=%d did not advance past %d", lock.Fence(), oldFence)
	}
	if got := stub.owner(); got != newToken {
		t.Fatalf("Redis owner=%q, want the new token %q", got, newToken)
	}
	if !lock.IsAcquired() {
		t.Fatal("re-acquired lock reports not acquired")
	}
	if err := lock.Unlock(context.Background(), 8, time.Second); err != nil {
		t.Fatalf("unlock of the new generation: %v", err)
	}
	stub.mu.Lock()
	owner, version := stub.hash["owner"], stub.hash["version"]
	stub.mu.Unlock()
	if owner != "" || version != "8" {
		t.Fatalf("after the new generation's unlock owner=%q version=%q, want released with version 8", owner, version)
	}
	if err := lock.UnlockWithRetry(context.Background(), 8, time.Second, 0, 0); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("second unlock=%v, want ErrVersionedLockNotOwned", err)
	}
}

func TestVersionedLockUnknownReleaseDefersToRedisOwner(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	_, oldFence := failedUnlock(t, stub, lock)

	// 租约到期后被别的持有者取得：这一侧要走"别人持有"的 NotAcquired 路径，并且不能把别人挤掉。
	stub.setOwner("someone-else")
	if err := lock.TryLock(context.Background()); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("TryLock while another holder owns the lease = %v, want ErrVersionedLockNotAcquired", err)
	}
	if got := stub.owner(); got != "someone-else" {
		t.Fatalf("TryLock after an unknown release evicted the other holder: owner=%q", got)
	}
	if lock.IsAcquired() {
		t.Fatal("lock reports acquired while another holder owns the lease")
	}
	stub.setOwner("")
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the other holder released = %v", err)
	}
	if lock.Fence() <= oldFence {
		t.Fatalf("fence=%d did not advance past %d", lock.Fence(), oldFence)
	}
	if err := lock.Unlock(context.Background(), 9, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestVersionedLockUnknownReleaseAfterLeaseLapsed(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	oldToken, oldFence := failedUnlock(t, stub, lock)

	// Redis 恢复时租约已按 TTL 过期：正常取锁。
	stub.setOwner("")
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the lease lapsed = %v, want acquisition", err)
	}
	if got := lockToken(lock); got == oldToken {
		t.Fatalf("acquisition reused the old token %q", oldToken)
	}
	if lock.Fence() <= oldFence {
		t.Fatalf("fence=%d did not advance past %d", lock.Fence(), oldFence)
	}
	if err := lock.Unlock(context.Background(), 9, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestVersionedLockUnlockContextExpiryEntersUnknownHold(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldToken := lockToken(lock)
	// 释放的 ctx 已到期：一次 Eval 都没发出，租约仍在 Redis 上。这也是"没有明确答复"，不能留着本地持有状态。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lock.UnlockWithRetry(ctx, 7, time.Second, 2, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("premise: unlock with an expired ctx = %v", err)
	}
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after a ctx-expired unlock = %v, want re-acquisition", err)
	}
	if got := lockToken(lock); got == oldToken {
		t.Fatalf("re-acquisition reused the old token %q", oldToken)
	}
	if got := stub.owner(); got != lockToken(lock) {
		t.Fatalf("Redis owner=%q, want the new token", got)
	}
	if err := lock.Unlock(context.Background(), 8, time.Second); err != nil {
		t.Fatal(err)
	}
}

func touchGenerationOf(l *versionedLock) string {
	l.touchMu.Lock()
	defer l.touchMu.Unlock()
	return l.touchGeneration
}

// awaitTouch 等待下一次 touch 到达 Redis 替身并返回它的 token。
func awaitTouch(t *testing.T, stub *outageEvalStub) string {
	t.Helper()
	return awaitChan(t, stub.touches, "a renewal to reach the stub")
}

// 续期 goroutine 的登记按代际（RR-20260926-43）：失败的释放已停掉旧代际的续期，重新取得的新代际登记自己的续期，
// 旧 token 在释放失败之后不再被续期（否则会把一把没人持有的租约续下去）。
func TestVersionedLockUnknownReleaseRenewalFollowsNewGeneration(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: 40 * time.Millisecond, AutoAsyncTouch: true, AsyncTouchInterval: 2 * time.Millisecond, AsyncTouchExtend: 20 * time.Millisecond})
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldToken := lockToken(lock)
	if got := awaitTouch(t, stub); got != oldToken {
		t.Fatalf("first renewal token=%q, want the holder's %q", got, oldToken)
	}
	stub.failUnlocks(errRedisOutage)
	if err := lock.UnlockWithRetry(context.Background(), 7, time.Second, 1, time.Millisecond); !errors.Is(err, errRedisOutage) {
		t.Fatalf("premise: unlock during the outage = %v", err)
	}
	stub.failUnlocks(nil)
	// UnlockWithRetry 已等待全部续期 goroutine 退出：登记为空，之后不会再有旧 token 的续期。
	if got := touchGenerationOf(lock); got != "" {
		t.Fatalf("renewal registration after the failed unlock=%q, want none", got)
	}
	for len(stub.touches) > 0 {
		<-stub.touches
	}

	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the failed unlock = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	if got := touchGenerationOf(lock); got != newToken {
		t.Fatalf("renewal registered for %q after re-acquisition, want the new token %q", got, newToken)
	}
	if got := awaitTouch(t, stub); got != newToken {
		t.Fatalf("renewal after re-acquisition used token %q, want the new token %q (old %q)", got, newToken, oldToken)
	}
	if err := lock.Unlock(context.Background(), 8, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := touchGenerationOf(lock); got != "" {
		t.Fatalf("renewal registration after unlock=%q, want none", got)
	}
	for len(stub.touches) > 0 {
		if got := <-stub.touches; got == oldToken {
			t.Fatalf("the old token %q was renewed after its failed unlock", oldToken)
		}
	}
}

// 旧代际的迟到 touch 回复（RR-20260924-20）：一次公开 Touch 在释放失败之前发出、在重新取得之后才返回 -1，
// 只能把它自己的代际标记为失效，不能清掉新代际的持有。
func TestVersionedLockLateOldTouchDoesNotClearReclaimedGeneration(t *testing.T) {
	stub := newOutageEvalStub()
	lock := newVersionedLock(stub, 42, fredis.VersionedLockOptions{TTL: time.Second})
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldToken := lockToken(lock)
	stub.state.Lock()
	stub.holdToken, stub.held, stub.release = oldToken, make(chan struct{}), make(chan struct{})
	held, release := stub.held, stub.release
	stub.state.Unlock()
	touchDone := make(chan error, 1)
	go func() { touchDone <- lock.Touch(context.Background(), time.Millisecond) }()
	awaitChan(t, held, "the old generation's touch to reach the stub")

	stub.failUnlocks(errRedisOutage)
	if err := lock.UnlockWithRetry(context.Background(), 7, time.Second, 1, time.Millisecond); !errors.Is(err, errRedisOutage) {
		t.Fatalf("premise: unlock during the outage = %v", err)
	}
	stub.failUnlocks(nil)
	if err := lock.TryLock(context.Background()); err != nil {
		t.Fatalf("TryLock after the failed unlock = %v, want re-acquisition", err)
	}
	newToken := lockToken(lock)
	close(release)
	if err := awaitChan(t, touchDone, "the late touch to return"); !errors.Is(err, ErrVersionedLockExpired) {
		t.Fatalf("late old-generation touch = %v, want ErrVersionedLockExpired", err)
	}
	if !lock.IsAcquired() || lockToken(lock) != newToken {
		t.Fatalf("the late old-generation touch disturbed the new generation: acquired=%v token=%q want %q", lock.IsAcquired(), lockToken(lock), newToken)
	}
	if err := lock.Unlock(context.Background(), 8, time.Second); err != nil {
		t.Fatal(err)
	}
}
