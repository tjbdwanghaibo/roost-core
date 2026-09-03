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
