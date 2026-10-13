// Mirror 复制器：信封发布与应用、内外身份一致、畸形入站与发布逐项拒绝、投递 ID 互不相同。

package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
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

// fakeBus 的退订不撤登记（release 为 nil）：已退订的订阅由 Subscription 的准入拒绝投递。
type fakeBus struct {
	handlers map[string][]*fsyncbus.Subscription
}

func newFakeBus() *fakeBus {
	return &fakeBus{handlers: make(map[string][]*fsyncbus.Subscription)}
}

func (b *fakeBus) Publish(msg *fsyncbus.SyncMsg) error {
	for _, sub := range b.handlers[msg.Topic] {
		if err := sub.Deliver(context.Background(), msg); err != nil && !errors.Is(err, fsyncbus.ErrUnsubscribed) {
			return err
		}
	}
	return nil
}

func (b *fakeBus) Subscribe(topic string, handler fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	sub := fsyncbus.NewSubscription(topic, handler, nil)
	b.handlers[topic] = append(b.handlers[topic], sub)
	return sub, nil
}

var _ fsyncbus.ISyncBus = (*fakeBus)(nil)

// recordingBus keeps what was published so a test can inspect the transport
// envelope itself, not only what a subscriber received.
type recordingBus struct {
	fakeBus
	published []*fsyncbus.SyncMsg
}

func newRecordingBus() *recordingBus {
	return &recordingBus{fakeBus: fakeBus{handlers: make(map[string][]*fsyncbus.Subscription)}}
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

func startedReplicator(t *testing.T) (*fakeBus, *fakeStore, *Replicator) {
	t.Helper()
	bus := newFakeBus()
	store := &fakeStore{}
	rep := New(bus, "topic", store)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rep.Stop)
	return bus, store, rep
}

// The inbound envelope is a wire contract between the publishing process and
// every mirror. Each rule the subscriber enforces is pinned by message, and
// the store must see nothing when a rule fires — a rejected message that
// still reaches ApplyReplica would be worse than no check at all.
func TestReplicatorRefusesEachMalformedInboundMessage(t *testing.T) {
	encode := func(t *testing.T, env Envelope) []byte {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name string
		msg  func(t *testing.T) *fsyncbus.SyncMsg
		want string
	}{
		{"nil message", func(*testing.T) *fsyncbus.SyncMsg { return nil }, "message is nil"},
		{"zero key", func(*testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "topic", Key: 0, Version: 1, Data: []byte("{}")}
		}, "message key is zero"},
		{"outer topic mismatch", func(*testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "other", Key: 7, Version: 1, Data: []byte("{}")}
		}, "outer topic mismatch"},
		{"inner topic mismatch", func(t *testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 1, Data: encode(t, Envelope{Topic: "other", Key: 7, Version: 1})}
		}, "inner topic mismatch"},
		{"inner version mismatch", func(t *testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 2, Data: encode(t, Envelope{Key: 7, Version: 1})}
		}, "inner version mismatch"},
		{"unsupported operation", func(t *testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 1, Data: encode(t, Envelope{Key: 7, Version: 1, Op: Op(9)})}
		}, "unsupported operation 9"},
		{"undecodable payload", func(*testing.T) *fsyncbus.SyncMsg {
			return &fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 1, Data: []byte("{not json")}
		}, "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus, store, _ := startedReplicator(t)
			// Deliver straight to the subscriber, the way a transport would:
			// the topic on the wire is whatever the publisher put there.
			err := bus.handlers["topic"][0].Deliver(context.Background(), tc.msg(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("subscriber returned %v, want %q", err, tc.want)
			}
			if len(store.items) != 0 {
				t.Fatalf("rejected message reached the store: %+v", store.items)
			}
		})
	}
}

// The outer message is authoritative: an inner envelope may omit topic, key,
// version and op, and the mirror fills them from the transport. An empty
// payload is a delete. These are the shapes older publishers emit.
func TestReplicatorFillsInnerIdentityFromTheOuterMessage(t *testing.T) {
	bus, store, _ := startedReplicator(t)
	raw, err := json.Marshal(Envelope{Payload: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(&fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 5, Data: raw}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(&fsyncbus.SyncMsg{Topic: "topic", Key: 7, Version: 6}); err != nil {
		t.Fatal(err)
	}
	if len(store.items) != 2 {
		t.Fatalf("items = %+v", store.items)
	}
	if got := store.items[0]; got.Topic != "topic" || got.Key != 7 || got.Version != 5 || got.Op != OpUpsert || string(got.Payload) != "p" {
		t.Fatalf("filled envelope = %+v", got)
	}
	if got := store.items[1]; got.Op != OpDelete || got.Version != 6 || got.Key != 7 {
		t.Fatalf("empty payload must be a delete: %+v", got)
	}
}

// Publish validates before anything goes on the bus.
func TestReplicatorPublishRefusesEachMalformedEnvelope(t *testing.T) {
	bus, store, rep := startedReplicator(t)
	cases := []struct {
		name string
		env  Envelope
		want string
	}{
		{"foreign topic", Envelope{Topic: "other", Key: 7, Version: 1}, "envelope topic mismatch"},
		{"zero key", Envelope{Key: 0, Version: 1}, "envelope key is zero"},
		{"unsupported op", Envelope{Key: 7, Version: 1, Op: Op(9)}, "unsupported operation 9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := rep.Publish(context.Background(), tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Publish = %v, want %q", err, tc.want)
			}
		})
	}
	if len(store.items) != 0 || len(bus.handlers["topic"]) != 1 {
		t.Fatalf("a refused publish must not reach the bus: items=%+v", store.items)
	}
	uninitialized := New(nil, "topic", nil)
	if err := uninitialized.Publish(context.Background(), Envelope{Key: 7}); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("Publish on an uninitialized replicator = %v", err)
	}
	if err := uninitialized.Start(); err == nil || !strings.Contains(err.Error(), "bus, store and topic are required") {
		t.Fatalf("Start without bus/store = %v", err)
	}
}

// nil 复制器不能启动；未知操作码拒绝发布；未初始化的复制器 / 键为零的删除拒绝。
func TestReplicatorRefusesNilUninitialisedAndMalformedPublishes(t *testing.T) {
	ctx := context.Background()
	var none *Replicator
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "replicator is nil") {
		t.Fatalf("Start on a nil replicator = %v", err)
	}
	if err := (&Replicator{}).PublishDelete(ctx, 1, 1); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("PublishDelete on an uninitialised replicator = %v", err)
	}
	bus := newFakeBus()
	rep := New(bus, "topic", &fakeStore{})
	if err := rep.Publish(ctx, Envelope{Key: 1, Version: 1, Op: Op(99)}); err == nil || !strings.Contains(err.Error(), "unsupported operation 99") {
		t.Fatalf("Publish with an unknown op = %v", err)
	}
	if err := rep.PublishDelete(ctx, 0, 1); err == nil || !strings.Contains(err.Error(), "delete key is zero") {
		t.Fatalf("PublishDelete with key 0 = %v", err)
	}
	if err := rep.PublishDelete(ctx, 7, 1); err != nil {
		t.Fatalf("PublishDelete with a real key = %v", err)
	}
}
