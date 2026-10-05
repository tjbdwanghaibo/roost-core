package syncbus

import (
	"context"
	"errors"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

func TestSyncModStopWithContextPrefersContextStopper(t *testing.T) {
	bus := &contextStopSyncBus{}
	mod := &SyncBusMod{bus: bus}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext: %v", err)
	}
	if !bus.contextStopped {
		t.Fatal("StopWithContext should call bus StopWithContext")
	}
	if bus.legacyStopped {
		t.Fatal("legacy Stop should not be called when context stop is available")
	}
}

type contextStopSyncBus struct {
	contextStopped bool
	legacyStopped  bool
}

func (b *contextStopSyncBus) Publish(*fsyncbus.SyncMsg) error { return nil }

func (b *contextStopSyncBus) Subscribe(string, fsyncbus.Handler) (func(), error) {
	return func() {}, nil
}

func (b *contextStopSyncBus) Stop() {
	b.legacyStopped = true
}

func (b *contextStopSyncBus) StopWithContext(context.Context) error {
	b.contextStopped = true
	return nil
}

// RR-20261005-NC-172：总线停止超时时 Mod 保留总线（NatsMod 随后看到停机不完整、不关连接），重试再停同一个总线。
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
