package syncbus

import (
	"context"
	"errors"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
)

// 停止委托给总线自己的 StopWithContext，成功后释放总线；没有 Provide 过（总线为 nil）的 Mod 停止直接返回 nil。
func TestSyncModStopDelegatesToTheBusAndReleasesIt(t *testing.T) {
	bus := &contextStopSyncBus{}
	mod := &SyncBusMod{bus: bus}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext: %v", err)
	}
	if !bus.contextStopped || mod.bus != nil {
		t.Fatalf("stop did not reach the bus or keep releasing it: stopped=%v kept=%v", bus.contextStopped, mod.bus != nil)
	}
	if err := (&SyncBusMod{}).StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext before Provide = %v, want nil", err)
	}
}

// 普通 NATS 总线也走同一条停机路径：driver.Bus 在编译期要求 StopWithContext，它没有要等的东西，
// 停止返回 nil 并释放总线（之前 Mod 对它做运行期断言、断言不中就跳过）。
func TestSyncModStopsThePlainNatsBusThroughTheSameContract(t *testing.T) {
	mod := &SyncBusMod{bus: driver.NewNatsSyncBus(nil, 7, "")}
	if err := mod.StopWithContext(context.Background()); err != nil || mod.bus != nil {
		t.Fatalf("StopWithContext = %v, bus kept = %v; want nil and released", err, mod.bus != nil)
	}
}

type contextStopSyncBus struct {
	contextStopped bool
}

func (b *contextStopSyncBus) Publish(*fsyncbus.SyncMsg) error { return nil }

func (b *contextStopSyncBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return fsyncbus.NewSubscription(topic, h, nil), nil
}

func (b *contextStopSyncBus) StopWithContext(context.Context) error {
	b.contextStopped = true
	return nil
}

// 总线停止超时时 Mod 保留总线（NatsMod 随后看到停机不完整、不关连接），重试再停同一个总线。
func TestSyncModStopTimeoutKeepsTheBusForRetry(t *testing.T) {
	bus := &drainingStopSyncBus{}
	mod := &SyncBusMod{bus: bus}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mod.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext = %v, want context.Canceled", err)
	}
	if mod.bus == nil {
		t.Fatal("timed-out stop released the bus")
	}
	bus.drained = true
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("retry StopWithContext = %v", err)
	}
	if mod.bus != nil || bus.calls != 2 {
		t.Fatalf("retry did not finish the stop: bus kept=%v calls=%d", mod.bus != nil, bus.calls)
	}
}

type drainingStopSyncBus struct {
	contextStopSyncBus
	drained bool
	calls   int
}

func (b *drainingStopSyncBus) StopWithContext(ctx context.Context) error {
	b.calls++
	if !b.drained {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
