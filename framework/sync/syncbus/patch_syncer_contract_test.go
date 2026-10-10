package syncbus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPatchSyncerRefusesIncompleteConfigurationAndZeroKeys(t *testing.T) {
	ctx := context.Background()
	keyOf := func(v int64) int64 { return v }
	apply := func(context.Context, int64) error { return nil }
	var none *PatchSyncer[int64]
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "syncer is nil") {
		t.Fatalf("Start on a nil syncer = %v", err)
	}
	if err := none.Publish(ctx, 1); err == nil || !strings.Contains(err.Error(), "syncer is nil") {
		t.Fatalf("Publish on a nil syncer = %v", err)
	}
	noApply := NewPatchSyncer[int64](newPatchFakeBus(), PatchSyncerConfig[int64]{Topic: "patch", KeyOf: keyOf})
	if err := noApply.Start(); err == nil || !strings.Contains(err.Error(), "apply function are required") {
		t.Fatalf("Start without Apply = %v", err)
	}
	noTopic := NewPatchSyncer[int64](newPatchFakeBus(), PatchSyncerConfig[int64]{KeyOf: keyOf, Apply: apply})
	if err := noTopic.Publish(ctx, 1); err == nil || !strings.Contains(err.Error(), "bus, topic and key function") {
		t.Fatalf("Publish without a topic = %v", err)
	}
	bus := newPatchFakeBus()
	full := NewPatchSyncer[int64](bus, PatchSyncerConfig[int64]{Topic: "patch", KeyOf: keyOf, Apply: apply})
	if err := full.Start(); err != nil {
		t.Fatalf("Start with a full config = %v", err)
	}
	if err := full.Publish(ctx, 0); err == nil || !strings.Contains(err.Error(), "key is zero") {
		t.Fatalf("Publish with a zero key = %v", err)
	}
	if len(bus.published) != 0 {
		t.Fatal("a refused publish reached the bus")
	}
	if err := full.Publish(ctx, 7); err != nil || len(bus.published) != 1 {
		t.Fatalf("Publish with a real key = %v, published=%d", err, len(bus.published))
	}
}

func TestPatchSyncerStopWaitsForInFlightApply(t *testing.T) {
	bus := newPatchFakeBus()
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	applied := 0
	s := NewPatchSyncer[testPatch](bus, PatchSyncerConfig[testPatch]{Topic: "patch", LocalSid: 1,
		KeyOf: func(p testPatch) int64 { return p.PlayerID },
		Apply: func(_ context.Context, p testPatch) error {
			applied++
			if p.Name == "block" {
				close(entered)
				<-release
				close(returned)
			}
			return nil
		}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	sub := bus.handlers["patch"][0]
	deliver := func(name string) error {
		return sub.Deliver(context.Background(), &SyncMsg{Topic: "patch", Key: 7, FromSid: 2, Data: []byte(`{"player_id":7,"name":"` + name + `"}`)})
	}
	go func() { _ = deliver("block") }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with Apply in flight = %v, want DeadlineExceeded", err)
	}
	close(release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop = %v, want nil", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Stop returned nil while Apply was still running")
	}
	if err := deliver("late"); !errors.Is(err, ErrUnsubscribed) || applied != 1 {
		t.Fatalf("late delivery = %v applied=%d, want ErrUnsubscribed and 1", err, applied)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
