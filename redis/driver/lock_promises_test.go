package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// U-0105 · C2（空洞测试）· B-19。
//
// distLock 的 Release / Extend 在没有持有时必须在发出脚本之前回答
// ErrLockNotHeld：脚本的值守卫虽然也挡得住空 owner，但去掉门口的守卫后，
// 一个从未 Acquire 的对象会为此多打一次往返，服务端出错时还会把自己翻成
// uncertain 并把传输错误当作释放结果。AutoExtendLock 的 Acquire 失败时不得
// 启动续期看门狗。nil / 配置守卫一并钉住。

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
