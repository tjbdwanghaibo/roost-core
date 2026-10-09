package driver

import (
	"context"
	"errors"
	"sync"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

// RR-20261005-NC-172：StopWithContext 受调用方 ctx 约束、可重试；停止后的投递交还 broker，停止后不再接受订阅。
func TestJetStreamSyncBusStopWithContextIsBoundedAndRetryable(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	calls := 0
	if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
		calls++
		if m.Key == 1 {
			close(entered)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
	<-entered

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bus.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext with a handler running = %v, want context.Canceled", err)
	}
	// 停止开始后到达的投递交还 broker（返回错误 → driver NAK），不进业务。
	if err := js.deliver("roost.sync.state", fanoutMsg(t, 2)); !errors.Is(err, errJetStreamSyncStopping) {
		t.Fatalf("delivery after stop = %v, want errJetStreamSyncStopping", err)
	}
	if _, err := bus.Subscribe("other", func(context.Context, *fsyncbus.SyncMsg) error { return nil }); err == nil {
		t.Fatal("Subscribe after stop created a consumer whose deliveries could never be admitted")
	}
	if err := bus.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry StopWithContext before the handler returned = %v, want context.Canceled", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	if err := bus.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext after the handler returned = %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("subscription stopped %d times, want 1", js.subs[0].stops.Load())
	}
}
