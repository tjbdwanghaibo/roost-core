// RR-20261004-07：Bus.StopWithContext 超出预算后只是调用方不再等待，停止本身仍由 Bus
// 持有。之后的调用必须继续等同一次 pool 排空，排空完成才返回终态结果；旧实现第一次
// 调用就丢掉 pool 并把 ctx 错误缓存成终态，重试永远返回同一个 DeadlineExceeded。
// 停止已经发起时，业务 handler 里的 Stop() 不能等待正在等它自己的那次排空。
package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type stopRetryResult struct {
	err             error
	handlerFinished bool
}

// startBlockedBus starts a Bus with one worker and parks a business handler in
// it. after runs once the handler has been released, still inside the handler.
func startBlockedBus(t *testing.T, after func(*Bus)) (b *Bus, release func(), finished *atomic.Bool) {
	t.Helper()
	b = New(&lifecycleClient{}, &lifecycleRpc{replies: make(chan []byte, 1)}, nil, Config{Sid: 7, SvcType: "game", WorkerNum: 1})
	entered, released := make(chan struct{}), make(chan struct{})
	finished = &atomic.Bool{}
	if err := b.Handle("mail", "Changed", func(*MsgContext) {
		close(entered)
		<-released
		if after != nil {
			after(b)
		}
		finished.Store(true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	wire, err := b.encodeMsg(7, "mail", "Changed", struct{}{}, fnats.BroadcastNone)
	if err != nil {
		t.Fatal(err)
	}
	b.onMessage(&fnats.Msg{Data: wire})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	return b, release, finished
}

func TestBusStopRetryWaitsForTheRetainedDrain(t *testing.T) {
	b, release, finished := startBlockedBus(t, nil)

	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.StopWithContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budgeted stop = %v, want DeadlineExceeded", err)
	}
	again, cancelAgain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelAgain()
	if err := b.StopWithContext(again); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second budgeted stop while the handler still runs = %v, want DeadlineExceeded", err)
	}

	retried := make(chan stopRetryResult, 1)
	go func() {
		err := b.StopWithContext(context.Background())
		retried <- stopRetryResult{err: err, handlerFinished: finished.Load()}
	}()
	release()
	var got stopRetryResult
	select {
	case got = <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("retry stop did not return after the handler exited")
	}
	t.Logf("retry_err=%v handler_finished_before_return=%v", got.err, got.handlerFinished)
	if got.err != nil || !got.handlerFinished {
		t.Fatal("retry stop did not wait for the retained pool drain to finish")
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatalf("stop after the drain finished = %v", err)
	}
}

func TestBusStopInsideHandlerAfterBudgetedStopReturns(t *testing.T) {
	inner := make(chan struct{})
	b, release, _ := startBlockedBus(t, func(b *Bus) {
		b.Stop()
		close(inner)
	})
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.StopWithContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budgeted stop = %v, want DeadlineExceeded", err)
	}
	release()
	select {
	case <-inner:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop inside the handler waited for the drain that is waiting for this handler")
	}
	done, cancelDone := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDone()
	if err := b.StopWithContext(done); err != nil {
		t.Fatalf("final stop = %v", err)
	}
}
