package room

import (
	"context"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

func TestSyncModStopWithContextPrefersContextStopper(t *testing.T) {
	bus := &contextStopSyncBus{}
	mod := &RoomMod{bus: bus}
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
