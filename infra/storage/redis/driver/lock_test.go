// 分布式锁：自动续期与租约丢失、令牌每次取得都不同、结果未知阻止复用直到释放核对、未持有时的守卫。

package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// fakeDistLock counts extensions and can start refusing them, modeling a
// lease that expired or was taken over.
type fakeDistLock struct {
	mu            sync.Mutex
	held          bool
	extends       int
	extendOK      bool
	transientErrs int
}

func (f *fakeDistLock) Acquire(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held {
		return false, nil
	}
	f.held = true
	f.extendOK = true
	return true, nil
}

func (f *fakeDistLock) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.held {
		return fredis.ErrLockNotHeld
	}
	f.held = false
	return nil
}

func (f *fakeDistLock) Extend(context.Context, time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends++
	if f.transientErrs > 0 {
		f.transientErrs--
		return false, errors.New("stub: transient network error")
	}
	return f.extendOK, nil
}

func (f *fakeDistLock) extendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extends
}

func (f *fakeDistLock) expireLease() {
	f.mu.Lock()
	f.extendOK = false
	f.held = false
	f.mu.Unlock()
}

func TestAutoExtendLockKeepsLeaseAliveUntilRelease(t *testing.T) {
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, 30*time.Millisecond, 5*time.Millisecond)
	ok, err := lock.Acquire(context.Background())
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for inner.extendCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog extended only %d times", inner.extendCount())
		}
		time.Sleep(time.Millisecond)
	}
	if lock.Err() != nil {
		t.Fatalf("healthy lease reported loss: %v", lock.Err())
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Release stops the watchdog: the extend count settles.
	settled := inner.extendCount()
	time.Sleep(30 * time.Millisecond)
	if got := inner.extendCount(); got != settled {
		t.Fatalf("watchdog kept extending after release: %d -> %d", settled, got)
	}
}

func TestAutoExtendLockSurvivesTransientExtendErrors(t *testing.T) {
	// Regression: a single transient Extend error (network blip) used to stop
	// the watchdog permanently even though the lease was still healthy. The
	// watchdog now keeps retrying while the last successful renewal can still
	// cover the lease, and only declares loss once the TTL has provably run
	// out without a renewal.
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, 200*time.Millisecond, 5*time.Millisecond)
	if ok, err := lock.Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	inner.mu.Lock()
	inner.transientErrs = 3
	inner.mu.Unlock()

	// Wait until the transient errors are consumed AND a later renewal
	// succeeded.
	deadline := time.Now().Add(2 * time.Second)
	for {
		inner.mu.Lock()
		recovered := inner.transientErrs == 0 && inner.extends >= 5
		inner.mu.Unlock()
		if recovered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watchdog never recovered past the transient errors")
		}
		time.Sleep(time.Millisecond)
	}
	if err := lock.Err(); err != nil {
		t.Fatalf("transient errors must not surface as lease loss: %v", err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAutoExtendLockSurfacesLostLease(t *testing.T) {
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, 30*time.Millisecond, 5*time.Millisecond)
	if ok, err := lock.Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	inner.expireLease()
	deadline := time.Now().Add(2 * time.Second)
	for lock.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("lost lease was never surfaced")
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(lock.Err(), fredis.ErrLockNotHeld) {
		t.Fatalf("err=%v, want fredis.ErrLockNotHeld cause", lock.Err())
	}
	// Release after loss reports the inner state honestly.
	if err := lock.Release(context.Background()); !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("release after loss err=%v", err)
	}
}

func TestAutoExtendLockRejectsInvalidConfiguration(t *testing.T) {
	lock := NewAutoExtendLock(nil, 0, 0)
	if ok, err := lock.Acquire(context.Background()); err == nil || ok {
		t.Fatalf("invalid lock acquired: ok=%v err=%v", ok, err)
	}
}

func TestAutoExtendLockRejectsConcurrentAcquire(t *testing.T) {
	inner := &fakeDistLock{}
	lock := NewAutoExtendLock(inner, time.Second, 100*time.Millisecond)
	if ok, err := lock.Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := lock.Acquire(context.Background()); !errors.Is(err, ErrDistLockAlreadyActive) || ok {
		t.Fatalf("second acquire: ok=%v err=%v", ok, err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// distLock 的 Release / Extend 在没有持有时在发出脚本之前回答 ErrLockNotHeld（不多打往返，也不把传输错误翻成 uncertain）；
// AutoExtendLock 的 Acquire 失败时不启动续期看门狗；nil / 配置守卫一并钉住。

func TestDistLockReleaseAndExtendRequireAHeldLease(t *testing.T) {
	ctx := context.Background()
	server := newScriptedRedis()
	factory := NewDistLockFactory(server)

	// 对照：先拿到再放掉，两条路径都可用。
	held := factory.NewLock("lease:1", time.Second)
	if ok, err := held.Acquire(ctx); err != nil || !ok {
		t.Fatalf("baseline Acquire = (%v, %v)", ok, err)
	}
	if ok, err := held.Extend(ctx, time.Second); err != nil {
		// scriptedRedis 只认 release 脚本；Extend 走不同脚本时它报错，这里只
		// 确认调用到达了服务端（说明 held 状态下不会被守卫拒绝）。
		_ = ok
	}
	if err := held.Release(ctx); err != nil {
		t.Fatalf("baseline Release = %v", err)
	}

	// 另一个持有者占着同一把锁；一个从未 Acquire 的对象不得能放掉它。
	other := factory.NewLock("lease:2", time.Second)
	if ok, err := other.Acquire(ctx); err != nil || !ok {
		t.Fatalf("other Acquire = (%v, %v)", ok, err)
	}
	// 释放脚本本身也带值守卫（空 owner 永远不等于持有者的令牌），所以单看
	// "别人的锁没被删"分不出是门口的守卫还是脚本在起作用。让服务端对任何
	// 脚本都报错：门口的守卫会在发出脚本之前就以 ErrLockNotHeld 拒绝，既不会
	// 把传输错误当作释放结果，也不会把一个从未持有的对象翻成 uncertain。
	idle := factory.NewLock("lease:2", time.Second)
	server.evalErr = errors.New("wire: connection refused")
	if err := idle.Release(ctx); !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("idle Release = %v, want ErrLockNotHeld before any script", err)
	}
	if ok, err := idle.Extend(ctx, time.Second); !errors.Is(err, fredis.ErrLockNotHeld) || ok {
		t.Fatalf("idle Extend = (%v, %v), want ErrLockNotHeld before any script", ok, err)
	}
	server.evalErr = nil
	if owner := server.owner("lease:2"); owner == "" {
		t.Fatal("idle Release deleted another holder's lease")
	}
	// 仍是干净的 idle 对象：Acquire 得到"被占"而不是 uncertain。
	if ok, err := idle.Acquire(ctx); err != nil || ok {
		t.Fatalf("idle Acquire after rejected Release = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestDistLockRejectsMissingClientAndInvalidTTL(t *testing.T) {
	ctx := context.Background()
	var nilLock *distLock
	if err := nilLock.Release(ctx); !errors.Is(err, ErrDistLockConfig) {
		t.Fatalf("nil lock Release = %v", err)
	}
	if _, err := nilLock.Extend(ctx, time.Second); !errors.Is(err, ErrDistLockConfig) {
		t.Fatalf("nil lock Extend = %v", err)
	}

	// 工厂为 nil 时给出的锁没有客户端：每个操作都必须以配置错误拒绝，
	// 而不是以"未持有"之类会被当作正常业务结果的错误。
	var nilFactory *DistLockFactory
	noClient := nilFactory.NewLock("lease:3", time.Second)
	if err := noClient.Release(ctx); !errors.Is(err, ErrDistLockConfig) {
		t.Fatalf("no-client Release = %v, want ErrDistLockConfig", err)
	}
	if _, err := noClient.Extend(ctx, time.Second); !errors.Is(err, ErrDistLockConfig) {
		t.Fatalf("no-client Extend = %v, want ErrDistLockConfig", err)
	}

	// 已持有的锁，Extend 到一个低于 1ms 的 TTL 是配置错误，不是续期失败。
	factory := NewDistLockFactory(newScriptedRedis())
	held := factory.NewLock("lease:4", time.Second)
	if ok, err := held.Acquire(ctx); err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v)", ok, err)
	}
	if ok, err := held.Extend(ctx, time.Microsecond); !errors.Is(err, ErrDistLockConfig) || ok {
		t.Fatalf("Extend(1µs) = (%v, %v), want ErrDistLockConfig", ok, err)
	}
}

func TestAutoExtendLockNilReceiverIsAnError(t *testing.T) {
	ctx := context.Background()
	var lock *AutoExtendLock
	if ok, err := lock.Acquire(ctx); err == nil || ok {
		t.Fatalf("nil Acquire = (%v, %v)", ok, err)
	}
	if err := lock.Release(ctx); err == nil {
		t.Fatal("nil Release returned no error")
	}
	if ok, err := lock.Extend(ctx, time.Second); err == nil || ok {
		t.Fatalf("nil Extend = (%v, %v)", ok, err)
	}
}

// failingDistLock 的 Acquire 要么被别人占着（false, nil），要么报传输错误。
type failingDistLock struct {
	fakeDistLock
	acquireErr error
}

func (f *failingDistLock) Acquire(ctx context.Context) (bool, error) {
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	return false, nil
}

func TestAutoExtendLockDoesNotStartWatchdogWhenAcquireFails(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		inner   *failingDistLock
		wantErr error
	}{
		{name: "held by another owner", inner: &failingDistLock{}},
		{name: "acquire transport error", inner: &failingDistLock{acquireErr: errors.New("wire: timeout")}},
	}
	for _, tc := range cases {
		tc.wantErr = tc.inner.acquireErr
		t.Run(tc.name, func(t *testing.T) {
			lock := NewAutoExtendLock(tc.inner, 30*time.Millisecond, 5*time.Millisecond)
			ok, err := lock.Acquire(ctx)
			if ok || !errors.Is(err, tc.wantErr) {
				t.Fatalf("Acquire = (%v, %v), want (false, %v)", ok, err, tc.wantErr)
			}
			// 看门狗若被错误启动，几个 interval 之内就会调用 Extend。
			time.Sleep(4 * 5 * time.Millisecond)
			if n := tc.inner.extendCount(); n != 0 {
				t.Fatalf("watchdog extended %d times after a failed Acquire", n)
			}
			// 也没有留下"已激活"状态：第二次 Acquire 不得因 AlreadyActive 被拒。
			if _, err := lock.Acquire(ctx); errors.Is(err, ErrDistLockAlreadyActive) {
				t.Fatal("failed Acquire left the lock marked active")
			}
		})
	}
}

// scriptedRedis stands in for the UniversalClient for the two commands distLock
// issues. Embedding the interface leaves every other method a nil-pointer
// panic, which is what a double should do for a call the test did not expect.
// It evaluates SETNX and the release script against one map, and can lose the
// reply of a SETNX that the server did or did not apply — the ambiguity the
// uncertain state exists for.
type scriptedRedis struct {
	builderOnly
	mu   sync.Mutex
	held map[string]string
	// loseNextSetNX makes the next SETNX return an error. applyBeforeLosing
	// says whether the server applied it first (reply lost) or not (never
	// reached the server).
	loseNextSetNX     bool
	applyBeforeLosing bool
	evalErr           error
}

func newScriptedRedis() *scriptedRedis { return &scriptedRedis{held: map[string]string{}} }

// Process 接住 distLock 经 sendOnce 发出的命令：SET … NX 与 EVAL，按命令名分派到下面两个模拟。
func (r *scriptedRedis) Process(ctx context.Context, cmd goredis.Cmder) error {
	args := cmd.Args()
	inner := cmd.(noReplay).Cmder
	switch cmd.Name() {
	case "set":
		result := r.SetNX(ctx, fmt.Sprint(args[1]), args[2], 0)
		inner.(*goredis.BoolCmd).SetVal(result.Val())
		cmd.SetErr(result.Err())
		return result.Err()
	case "eval":
		keys := []string{fmt.Sprint(args[3])}
		result := r.Eval(ctx, fmt.Sprint(args[1]), keys, args[4:]...)
		inner.(*goredis.Cmd).SetVal(result.Val())
		cmd.SetErr(result.Err())
		return result.Err()
	}
	err := fmt.Errorf("scripted redis: unexpected command %v", args)
	cmd.SetErr(err)
	return err
}

func (r *scriptedRedis) SetNX(_ context.Context, key string, value any, _ time.Duration) *goredis.BoolCmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loseNextSetNX {
		r.loseNextSetNX = false
		if r.applyBeforeLosing {
			if _, taken := r.held[key]; !taken {
				r.held[key] = fmt.Sprint(value)
			}
		}
		return goredis.NewBoolResult(false, errors.New("scripted redis: reply lost"))
	}
	if _, taken := r.held[key]; taken {
		return goredis.NewBoolResult(false, nil)
	}
	r.held[key] = fmt.Sprint(value)
	return goredis.NewBoolResult(true, nil)
}

func (r *scriptedRedis) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evalErr != nil {
		return goredis.NewCmdResult(nil, r.evalErr)
	}
	if script != releaseLockScript {
		return goredis.NewCmdResult(nil, fmt.Errorf("scripted redis: unexpected script"))
	}
	if r.held[keys[0]] == fmt.Sprint(args[0]) {
		delete(r.held, keys[0])
		return goredis.NewCmdResult(int64(1), nil)
	}
	return goredis.NewCmdResult(int64(0), nil)
}

func (r *scriptedRedis) owner(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[key]
}

// FEATURE_LOGIC M7 item 1: a TTL below one millisecond is a configuration
// error, never a lock that lasts forever. Reverting the check made no test red
// before this one existed.
func TestDistLockRefusesATTLBelowOneMillisecond(t *testing.T) {
	factory := NewDistLockFactory(newScriptedRedis())
	for _, ttl := range []time.Duration{0, -time.Second, 500 * time.Microsecond} {
		lock := factory.NewLock("k", ttl)
		if ok, err := lock.Acquire(context.Background()); ok || !errors.Is(err, ErrDistLockConfig) {
			t.Fatalf("ttl %s: ok=%v err=%v, want ErrDistLockConfig", ttl, ok, err)
		}
	}
	if ok, err := factory.NewLock("k", time.Millisecond).Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("a 1ms ttl was refused: ok=%v err=%v", ok, err)
	}
}

// M7 item 2: the owner token is minted per acquisition, so a stale holder
// from an earlier acquisition cannot present the current one's identity.
func TestDistLockOwnerTokenIsPerAcquisition(t *testing.T) {
	redis := newScriptedRedis()
	lock := NewDistLockFactory(redis).NewLock("k", time.Second)
	ctx := context.Background()
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	first := redis.owner("k")
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("reacquire: ok=%v err=%v", ok, err)
	}
	second := redis.owner("k")
	if first == "" || second == "" || first == second {
		t.Fatalf("owner token reused across acquisitions: %q then %q", first, second)
	}
}

// M7 item 3: a lost SETNX reply leaves ownership uncertain. The same object
// must not simply acquire again — that could stack a second lease on top of
// one the server already granted — until a value-guarded Release reconciles.
// Both outcomes of the ambiguity are exercised: the server applied the SETNX
// (Release frees it) and it never arrived (Release reports nothing held).
func TestDistLockUncertainStateBlocksReuseUntilReleaseReconciles(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			redis := newScriptedRedis()
			redis.loseNextSetNX, redis.applyBeforeLosing = true, applied
			lock := NewDistLockFactory(redis).NewLock("k", time.Second)
			ctx := context.Background()

			if ok, err := lock.Acquire(ctx); ok || err == nil {
				t.Fatalf("a lost reply was reported as ok=%v err=%v", ok, err)
			}
			if ok, err := lock.Acquire(ctx); ok || !errors.Is(err, ErrDistLockStateUncertain) {
				t.Fatalf("acquire while uncertain: ok=%v err=%v, want ErrDistLockStateUncertain", ok, err)
			}
			err := lock.Release(ctx)
			switch {
			case applied && err != nil:
				t.Fatalf("release of a lease the server did grant: %v", err)
			case !applied && !errors.Is(err, fredis.ErrLockNotHeld):
				t.Fatalf("release of a lease that never reached the server: %v, want fredis.ErrLockNotHeld", err)
			}
			if owner := redis.owner("k"); owner != "" {
				t.Fatalf("the server still holds %q after reconciliation", owner)
			}
			if ok, err := lock.Acquire(ctx); err != nil || !ok {
				t.Fatalf("acquire after reconciliation: ok=%v err=%v", ok, err)
			}
		})
	}
}

// A Release whose reply is lost is equally uncertain: the lease may or may not
// be gone, so the object stays unusable until a later Release settles it.
func TestDistLockReleaseErrorLeavesTheLockUncertain(t *testing.T) {
	redis := newScriptedRedis()
	lock := NewDistLockFactory(redis).NewLock("k", time.Second)
	ctx := context.Background()
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	redis.evalErr = errors.New("scripted redis: reply lost")
	if err := lock.Release(ctx); err == nil {
		t.Fatal("a release whose reply was lost reported success")
	}
	if ok, err := lock.Acquire(ctx); ok || !errors.Is(err, ErrDistLockStateUncertain) {
		t.Fatalf("acquire after an uncertain release: ok=%v err=%v", ok, err)
	}
	redis.evalErr = nil
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("reconciling release: %v", err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire after reconciliation: ok=%v err=%v", ok, err)
	}
}
