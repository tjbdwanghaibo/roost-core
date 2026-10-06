package driver

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// A3 / RR-20261005-NC-172：JetStream 同步总线的 StopWithContext 套共用停机契约骨架。卡住的工作是 consume
// 回调里的本地 handler；总线自己不持有要释放的依赖，“资源”是调用方（SyncBusMod → NatsMod）在停止返回 nil
// 之后才交还的总线与连接（stopcontract.CallerReleases）。
func TestJetStreamSyncBusStopContract(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	var bus *jetStreamSyncBus
	entered, release := make(chan struct{}), make(chan struct{})
	delivered := make(chan error, 1)
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			var err error
			bus, err = NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
				if m.Key == 1 {
					close(entered)
					<-release
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(bus.StopWithContext)
		},
		Block: func(testing.TB) {
			go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not enter")
			}
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	if err := <-delivered; err != nil {
		t.Fatalf("the in-flight delivery = %v, want it acknowledged", err)
	}
}
