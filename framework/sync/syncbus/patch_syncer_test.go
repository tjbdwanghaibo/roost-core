package syncbus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type testPatch struct {
	PlayerID int64  `json:"player_id"`
	Name     string `json:"name,omitempty"`
	Version  int64  `json:"version,omitempty"`
}

func (p testPatch) HasData() bool {
	return p.Name != ""
}

func TestPatchSyncerPublishesAndAppliesRemotePatch(t *testing.T) {
	bus := newPatchFakeBus()
	applied := make([]testPatch, 0, 1)
	syncer := NewPatchSyncer[testPatch](bus, PatchSyncerConfig[testPatch]{
		Topic:     "player.patch",
		LocalSid:  2,
		KeyOf:     func(p testPatch) int64 { return p.PlayerID },
		VersionOf: func(p testPatch) int64 { return p.Version },
		WithKey: func(p testPatch, key int64) testPatch {
			p.PlayerID = key
			return p
		},
		HasData: func(p testPatch) bool { return p.HasData() },
		Apply: func(_ context.Context, p testPatch) error {
			applied = append(applied, p)
			return nil
		},
	})
	if err := syncer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syncer.Stop(context.Background()) }()

	if err := syncer.Publish(context.Background(), testPatch{PlayerID: 7, Name: "hero", Version: 11}); err != nil {
		t.Fatal(err)
	}
	if err := syncer.Publish(context.Background(), testPatch{PlayerID: 7, Name: "hero", Version: 11}); err != nil {
		t.Fatal(err)
	}
	if len(bus.published) != 2 || bus.published[0].MessageID == "" || bus.published[0].MessageID == bus.published[1].MessageID {
		t.Fatalf("delivery ids are not unique: %+v", bus.published)
	}
	if bus.published[0].Version != 11 {
		t.Fatalf("business version=%d, want 11", bus.published[0].Version)
	}

	if len(applied) != 0 {
		t.Fatalf("local publish should be skipped by same sid, applied=%+v", applied)
	}
	if err := bus.Publish(&SyncMsg{
		Topic:   "player.patch",
		Key:     8,
		Version: 9,
		Data:    []byte(`{"name":"remote"}`),
		FromSid: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].PlayerID != 8 || applied[0].Name != "remote" {
		t.Fatalf("applied patches = %+v", applied)
	}
}

func TestPatchSyncerRejectsMismatchedKey(t *testing.T) {
	bus := newPatchFakeBus()
	syncer := NewPatchSyncer[testPatch](bus, PatchSyncerConfig[testPatch]{
		Topic: "player.patch",
		KeyOf: func(p testPatch) int64 { return p.PlayerID },
		Apply: func(context.Context, testPatch) error {
			t.Fatal("apply should not be called")
			return nil
		},
	})
	if err := syncer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syncer.Stop(context.Background()) }()

	err := bus.Publish(&SyncMsg{
		Topic: "player.patch",
		Key:   8,
		Data:  []byte(`{"player_id":9,"name":"bad"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "key mismatch") {
		t.Fatalf("err = %v, want key mismatch", err)
	}
}

func TestPatchSyncerSkipsEmptyPatch(t *testing.T) {
	bus := newPatchFakeBus()
	applied := 0
	syncer := NewPatchSyncer[testPatch](bus, PatchSyncerConfig[testPatch]{
		Topic:   "player.patch",
		KeyOf:   func(p testPatch) int64 { return p.PlayerID },
		HasData: func(p testPatch) bool { return p.HasData() },
		Apply: func(context.Context, testPatch) error {
			applied++
			return nil
		},
	})
	if err := syncer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syncer.Stop(context.Background()) }()

	if err := syncer.Publish(context.Background(), testPatch{PlayerID: 7}); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("empty publish applied %d patches", applied)
	}
}

type patchFakeBus struct {
	handlers  map[string][]*Subscription
	published []*SyncMsg
}

func newPatchFakeBus() *patchFakeBus {
	return &patchFakeBus{handlers: make(map[string][]*Subscription)}
}

func (b *patchFakeBus) Publish(msg *SyncMsg) error {
	if msg == nil {
		return nil
	}
	clone := *msg
	clone.Data = append([]byte(nil), msg.Data...)
	b.published = append(b.published, &clone)
	for _, sub := range b.handlers[msg.Topic] {
		if err := sub.Deliver(context.Background(), msg); err != nil && err != ErrUnsubscribed {
			return err
		}
	}
	return nil
}

func (b *patchFakeBus) Subscribe(topic string, handler Handler) (*Subscription, error) {
	var sub *Subscription
	sub = NewSubscription(topic, handler, func() {
		b.handlers[topic] = slices.DeleteFunc(b.handlers[topic], func(s *Subscription) bool { return s == sub })
	})
	b.handlers[topic] = append(b.handlers[topic], sub)
	return sub, nil
}

var _ ISyncBus = (*patchFakeBus)(nil)

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
