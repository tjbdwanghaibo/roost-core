package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corenats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

// recordingNatsClient is the smallest nats.IClient that lets the bus reach
// its argument checks; it records what would have gone on the wire.
type recordingNatsClient struct {
	published  int
	subscribed int
}

func (c *recordingNatsClient) Publish(string, []byte) error { c.published++; return nil }
func (c *recordingNatsClient) Request(string, []byte, time.Duration) ([]byte, error) {
	return nil, errors.New("unexpected request")
}
func (c *recordingNatsClient) Subscribe(string, corenats.MsgHandler) (corenats.ISubscription, error) {
	c.subscribed++
	return nil, errors.New("unexpected subscribe")
}
func (c *recordingNatsClient) QueueSubscribe(string, string, corenats.MsgHandler) (corenats.ISubscription, error) {
	c.subscribed++
	return nil, errors.New("unexpected queue subscribe")
}
func (c *recordingNatsClient) Drain() error { return nil }
func (c *recordingNatsClient) Close()       {}

func expectGuardErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// U-0095 (C2): both sync buses refuse an uninitialised bus, a nil message, a
// blank topic and a nil handler before anything reaches the wire.
func TestSyncBusesRefuseEachInvalidArgumentBeforeTheWire(t *testing.T) {
	t.Run("nats", func(t *testing.T) {
		expectGuardErr(t, NewNatsSyncBus(nil, 1, "").Publish(&fsyncbus.SyncMsg{Topic: "room"}), "nats sync: bus is not initialized")
		_, err := NewNatsSyncBus(nil, 1, "").Subscribe("room", func(*fsyncbus.SyncMsg) error { return nil })
		expectGuardErr(t, err, "nats sync: bus is not initialized")
		var nilBus *natsSyncBus
		expectGuardErr(t, nilBus.Publish(&fsyncbus.SyncMsg{Topic: "room"}), "nats sync: bus is not initialized")

		client := &recordingNatsClient{}
		bus := NewNatsSyncBus(client, 1, "")
		expectGuardErr(t, bus.Publish(nil), "nats sync: message is nil")
		expectGuardErr(t, bus.Publish(&fsyncbus.SyncMsg{Topic: "  "}), "nats sync: topic is empty")
		_, err = bus.Subscribe(" ", func(*fsyncbus.SyncMsg) error { return nil })
		expectGuardErr(t, err, "nats sync: topic is empty")
		_, err = bus.Subscribe("room", nil)
		expectGuardErr(t, err, "nats sync: handler is nil")
		if client.published != 0 || client.subscribed != 0 {
			t.Fatalf("refused calls reached the client: published=%d subscribed=%d", client.published, client.subscribed)
		}
	})
	t.Run("jetstream", func(t *testing.T) {
		if _, err := NewJetStreamSyncBus(context.Background(), nil, JetStreamSyncConfig{}); err == nil || !strings.Contains(err.Error(), "jetstream is nil") {
			t.Fatalf("nil jetstream error = %v", err)
		}
		var nilBus *jetStreamSyncBus
		expectGuardErr(t, nilBus.Publish(&fsyncbus.SyncMsg{Topic: "room"}), "jetstream sync: bus is not initialized")
		_, err := nilBus.Subscribe("room", func(*fsyncbus.SyncMsg) error { return nil })
		expectGuardErr(t, err, "jetstream sync: bus is not initialized")

		js := newFakeJetStream()
		bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 1})
		if err != nil {
			t.Fatal(err)
		}
		expectGuardErr(t, bus.Publish(nil), "jetstream sync: message is nil")
		expectGuardErr(t, bus.Publish(&fsyncbus.SyncMsg{Topic: "\t"}), "jetstream sync: topic is empty")
		_, err = bus.Subscribe("", func(*fsyncbus.SyncMsg) error { return nil })
		expectGuardErr(t, err, "jetstream sync: topic is empty")
		_, err = bus.Subscribe("room", nil)
		expectGuardErr(t, err, "jetstream sync: handler is nil")
		if len(js.publishes) != 0 || len(js.consumers) != 0 {
			t.Fatalf("refused calls reached jetstream: publishes=%d consumers=%d", len(js.publishes), len(js.consumers))
		}
	})
}
