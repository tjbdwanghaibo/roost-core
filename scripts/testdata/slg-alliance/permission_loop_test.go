package alliance

import (
	"context"
	"errors"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"strconv"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 同一持久联盟权威、真实 TCP；不另造成员缓存。循环在测试主 goroutine 运行，回调控制业务事件。
func verifyPermissionLoop(t *testing.T, parent context.Context, a *alliance, prefix string) {
	t.Helper()
	f := newFog(t)
	alice, bob := newFogClient(), newFogClient()
	load := func() (state, error) { return a.read(parent) }
	consume := func(wantAlice, wantBob bool) {
		alice.consume(t, f.sink.take(1))
		bob.consume(t, f.sink.take(2))
		if (len(alice.objects) == 1) != wantAlice || (len(bob.objects) == 1) != wantBob {
			t.Fatal("spatial/permission intersection leaked", alice.objects, bob.objects)
		}
	}
	// Alice 有权但远离；Bob 在范围内却已退盟：两个客户端都不能收到目标。
	f.positions[1] = spatial.Point{X: 800, Y: 800}
	check(t, f.reconcileFrom(load))
	consume(false, false)
	// 侦察也必须在范围内；权限源并集不能越过空间过滤。
	f.scout[2] = true
	check(t, f.reconcileFrom(load))
	consume(false, true)
	f.positions[2] = spatial.Point{X: 800, Y: 800}
	check(t, f.reconcileFrom(load))
	consume(false, false)
	f.scout[2] = false
	f.positions[2] = spatial.Point{X: 10, Y: 10}
	f.positions[1] = spatial.Point{X: 10, Y: 10}
	check(t, f.reconcileFrom(load))
	consume(true, false)
	loop, e := newPermissionLoop(20*time.Millisecond, 200*time.Millisecond)
	check(t, e)
	// 实际 NATS 消费回调只做有界解析和唤醒，不触碰视野对象。
	_, publisher := newBus(t, prefix, "view-authority", 2000)
	_, subscriber := newBus(t, prefix, "view-refresh", 1001)
	seen := make(chan struct{}, 1)
	sub, e := subscriber.Subscribe(prefix+".view-refresh", func(msg *fnats.Msg) {
		if len(msg.Data) > 20 {
			return
		}
		generation, e := strconv.ParseUint(string(msg.Data), 10, 64)
		if e != nil {
			return
		}
		loop.notify(generation)
		select {
		case seen <- struct{}{}:
		default:
		}
	})
	check(t, e)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	check(t, subscriber.FlushContext(parent))
	check(t, publisher.Publish(prefix+".view-refresh", []byte(strconv.FormatUint(f.generation, 10))))
	check(t, publisher.FlushContext(parent))
	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("NATS refresh hint missing")
	}
	// 通知风暴只占一个槽，且不能降低权限代际下限。
	for i := 0; i < 1000; i++ {
		loop.notify(f.generation)
	}
	loop.notify(f.generation - 1)
	if len(loop.wake) != 1 || loop.minimum.Load() != f.generation {
		t.Fatal("hint coalescing lost floor")
	}
	<-loop.wake // 本轮以下变化故意不发通知，由真实 ticker 触发重读。
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cycles := 0
	e = loop.run(ctx, func(readCtx context.Context, minimum uint64) error {
		return f.reconcileAtLeast(minimum, func() (state, error) {
			if cycles == 1 {
				<-readCtx.Done()
				return state{}, readCtx.Err()
			}
			return a.read(readCtx)
		})
	}, func(e error) {
		cycles++
		switch cycles {
		case 1:
			check(t, e)
			consume(true, false)
			check(t, a.revoke(parent, "alice")) // 持久撤权后丢失通知。
		case 2:
			if !errors.Is(e, context.DeadlineExceeded) {
				t.Fatal("authority timeout not propagated", e)
			}
			if len(f.sink.take(1))+len(f.sink.take(2)) != 0 {
				t.Fatal("failed refresh published stale permissions")
			}
		case 3:
			check(t, e)
			consume(false, false)
			cancel()
		default:
			t.Fatal("loop ignored cancellation")
		}
	})
	if !errors.Is(e, context.Canceled) || cycles != 3 {
		t.Fatal("permission loop did not stop cleanly", e, cycles)
	}
	t.Log("TCP intersection: near unauthorized and far authorized absent; scout also range-bound; periodic refresh recovers lost revoke hint after read deadline, cancellation drains loop")
}
