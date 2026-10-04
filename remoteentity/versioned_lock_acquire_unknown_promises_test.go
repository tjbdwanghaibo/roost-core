package remoteentity

// RR-20261004-01：取锁没有拿到 Redis 的明确答复（Eval 因 ctx 截止 / 网络错误返回错误）时，脚本可能已经在 Redis 执行，
// owner 已是这次的 token。下一次 TryLock 必须以 Redis 为准：owner 仍是本锁对象铸造的较早一代 token（取锁或释放结果未知）
// 就在同一条脚本里换成新 token、新 fence（新代际），被别人持有走 NotAcquired，空闲就正常取得。
// 旧行为：TryLock 的 Eval 出错时不记录本次 token，下一次 TryLock 用新 token 争锁只看到"别人持有"，ErrVersionedLockNotAcquired
// 一直到 LockTTL（压测与正式缺省 24h）过期，实体在本进程内不可写。
// 另外两条承诺沿用 RR-20260924-20 / RR-20260926-43：迟到的旧代取锁脚本不能挤掉已经取得的新代际；续期按新代际登记。
// 这里用 Eval 替身控制每一次取锁脚本是"执行后丢回复 / 根本没发出 / 还在路上稍后落地"，不用 sleep 赌时序。

import (
	"context"
	"errors"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
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
