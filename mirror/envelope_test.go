package mirror

import (
	"context"
	"encoding/json"
	"errors"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	"testing"
)

func TestReplicatorPublishesAndAppliesEnvelope(t *testing.T) {
	bus := newFakeBus()
	store := &fakeStore{}
	rep := New(bus, "topic", store)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	defer rep.Stop()

	if err := rep.Publish(context.Background(), Envelope{
		Key:     7,
		Version: 3,
		Op:      OpUpsert,
		Payload: []byte("data"),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.items) != 1 || store.items[0].Key != 7 || string(store.items[0].Payload) != "data" {
		t.Fatalf("items = %+v", store.items)
	}

	if err := rep.PublishDelete(context.Background(), 7, 4); err != nil {
		t.Fatal(err)
	}
	if len(store.items) != 2 || store.items[1].Op != OpDelete || store.items[1].Version != 4 {
		t.Fatalf("delete item = %+v", store.items)
	}
}

func TestReplicatorRejectsForgedInnerIdentity(t *testing.T) {
	bus := newFakeBus()
	store := &fakeStore{}
	rep := New(bus, "topic", store)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Envelope{Topic: "topic", Key: 99, Version: 3, Op: OpUpsert})
	if err != nil {
		t.Fatal(err)
	}
	err = bus.Publish(&fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 3, Data: raw})
	if err == nil {
		t.Fatal("forged inner key was accepted")
	}
	if len(store.items) != 0 {
		t.Fatalf("forged envelope reached store: %+v", store.items)
	}
}

func TestReplicatorHonorsCanceledPublishContext(t *testing.T) {
	bus := newFakeBus()
	rep := New(bus, "topic", &fakeStore{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rep.Publish(ctx, Envelope{Key: 7, Version: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish error=%v, want context.Canceled", err)
	}
}

type fakeStore struct {
	items []Envelope
}

func (s *fakeStore) ApplyReplica(_ context.Context, env Envelope) error {
	s.items = append(s.items, env)
	return nil
}

type fakeBus struct {
	handlers map[string][]fsyncbus.Handler
}

func newFakeBus() *fakeBus {
	return &fakeBus{handlers: make(map[string][]fsyncbus.Handler)}
}

func (b *fakeBus) Publish(msg *fsyncbus.SyncMsg) error {
	for _, h := range b.handlers[msg.Topic] {
		if err := h(msg); err != nil {
			return err
		}
	}
	return nil
}

func (b *fakeBus) Subscribe(topic string, handler fsyncbus.Handler) (func(), error) {
	b.handlers[topic] = append(b.handlers[topic], handler)
	return func() {}, nil
}

var _ fsyncbus.ISyncBus = (*fakeBus)(nil)

// recordingBus keeps what was published so a test can inspect the transport
// envelope itself, not only what a subscriber received.
type recordingBus struct {
	fakeBus
	published []*fsyncbus.SyncMsg
}

func newRecordingBus() *recordingBus {
	return &recordingBus{fakeBus: fakeBus{handlers: make(map[string][]fsyncbus.Handler)}}
}

func (b *recordingBus) Publish(msg *fsyncbus.SyncMsg) error {
	copied := *msg
	b.published = append(b.published, &copied)
	return b.fakeBus.Publish(msg)
}

// Every published message carries its own delivery identity, minted per
// publisher instance and per publish, independent of the business version.
//
// Without one, the JetStream transport falls back to the tuple
// (topic, key, version, sid, part) — and the replicator sets no sid, so it was
// publishing with no dedup key at all. Had it set one, an upsert and a delete
// for the same key at the same version would have shared a single key, and the
// broker would have dropped whichever arrived second inside its dedup window.
// The business version is not an identity; that is the point of MessageID.
func TestPublishedMessagesCarryDistinctDeliveryIDs(t *testing.T) {
	bus := newRecordingBus()
	ctx := context.Background()
	first := New(bus, "topic", nil)
	if err := first.Publish(ctx, Envelope{Key: 7, Version: 3, Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if err := first.PublishDelete(ctx, 7, 3); err != nil {
		t.Fatal(err)
	}
	// The same content published again is a new delivery.
	if err := first.Publish(ctx, Envelope{Key: 7, Version: 3, Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	// A replicator in another process — or this one after a restart — must not
	// reuse the first one's identities.
	second := New(bus, "topic", nil)
	if err := second.Publish(ctx, Envelope{Key: 7, Version: 3, Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if len(bus.published) != 4 {
		t.Fatalf("published %d messages, want 4", len(bus.published))
	}
	seen := map[string]int{}
	for index, msg := range bus.published {
		if msg.MessageID == "" {
			t.Fatalf("message %d (key %d version %d) carries no delivery id; the transport would fall back to the version tuple", index, msg.Key, msg.Version)
		}
		if prior, dup := seen[msg.MessageID]; dup {
			t.Fatalf("messages %d and %d share delivery id %q", prior, index, msg.MessageID)
		}
		seen[msg.MessageID] = index
	}
}
