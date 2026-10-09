package driver

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

// RR-20261005-NC-172：JetStream 同步总线的 Stop 只停订阅（nats.go ConsumeContext.Stop 不等在途回调）就返回，
// 随后 SyncBusMod 释放总线、NatsMod 关连接，在途 handler 仍在运行、它的 ack 必然失败。三步停机：先关准入
// 并停订阅（幂等），在调用方 ctx 内等在途回调返回，超时返回 ctx 错误、保留总线，重试再等。

// stopWaitingIn 返回 stopped 的结果；若停止先进入 waitingFrame（在等在途回调）：cancel 为 nil 时直接报告 waited，
// 否则先 cancel 再取结果。
func stopWaitingIn(t *testing.T, stopped <-chan error, cancel context.CancelFunc, waitingFrame string) (err error, waited bool) {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			return err, false
		default:
		}
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), waitingFrame) {
			if cancel == nil {
				return nil, true // 不限时的停止：确认它在等即可，由调用方放行 handler
			}
			cancel()
			select {
			case err := <-stopped:
				return err, true
			case <-time.After(5 * time.Second):
				t.Fatal("stop ignored its context while waiting for the handler")
			}
		}
		runtime.Gosched()
	}
	t.Fatal("stop neither returned nor waited")
	return nil, false
}

func TestJetStreamSyncBusStopWaitsForAnInFlightHandler(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var handled []int64
	var handledMu sync.Mutex
	if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
		if m.Key == 1 {
			close(entered)
			<-release
		}
		handledMu.Lock()
		handled = append(handled, m.Key)
		handledMu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not enter")
	}

	// 修前唯一的停止入口 Stop()：在途 handler 返回之前不能返回。
	stopped := make(chan error, 1)
	go func() { bus.Stop(); stopped <- nil }()
	if _, waited := stopWaitingIn(t, stopped, nil, "jetStreamSyncBus).StopWithContext"); !waited {
		t.Fatal("Stop returned while a handler was still running on the subscription")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the handler finished")
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("subscription stops=%d, want 1", js.subs[0].stops.Load())
	}
}
