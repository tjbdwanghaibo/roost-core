package mirror

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

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
			err := bus.handlers["topic"][0](tc.msg(t))
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
