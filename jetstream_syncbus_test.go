package room

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/cube-core/syncbus"
)

func TestJetStreamSyncBusPublishesAndAcknowledgesHandlerError(t *testing.T) {
	js := newFakeJetStream()
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{
		LocalSid: 7,
		Prefix:   "cube.sync",
		Stream:   "CUBE_SYNC_TEST",
		Storage:  fnats.JetStreamStorageMemory,
	})
	if err != nil {
		t.Fatalf("NewJetStreamSyncBus error: %v", err)
	}
	if len(js.streams) != 1 {
		t.Fatalf("stream count=%d, want 1", len(js.streams))
	}
	if js.streams[0].Name != "CUBE_SYNC_TEST" || len(js.streams[0].Subjects) != 1 || js.streams[0].Subjects[0] != "cube.sync.>" {
		t.Fatalf("stream config mismatch: %+v", js.streams[0])
	}

	handlerErr := errors.New("apply failed")
	_, err = bus.Subscribe("remote_entity", func(msg *fsyncbus.SyncMsg) error {
		if msg.Topic != "remote_entity" || msg.Key != 101 || msg.Version != 3 || msg.FromSid != 12 {
			t.Fatalf("sync msg mismatch: %+v", msg)
		}
		return handlerErr
	})
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	if len(js.consumers) != 1 {
		t.Fatalf("consumer count=%d, want 1", len(js.consumers))
	}
	if js.consumers[0].Stream != "CUBE_SYNC_TEST" || js.consumers[0].FilterSubject != "cube.sync.remote_entity" {
		t.Fatalf("consumer config mismatch: %+v", js.consumers[0])
	}

	msg := &fsyncbus.SyncMsg{Topic: "remote_entity", Key: 101, Version: 3, Data: []byte("payload"), FromSid: 12}
	expectedMsgID := "sync:remote_entity:101:3:12:0"
	messageID := reflect.ValueOf(msg).Elem().FieldByName("MessageID")
	if messageID.IsValid() && messageID.CanSet() {
		messageID.SetString("delivery-1")
		expectedMsgID = "delivery-1"
	}
	err = bus.Publish(msg)
	if err != nil {
		t.Fatalf("Publish error=%v", err)
	}
	if len(js.publishes) != 1 {
		t.Fatalf("publish count=%d, want 1", len(js.publishes))
	}
	if js.publishes[0].subject != "cube.sync.remote_entity" {
		t.Fatalf("publish subject=%q", js.publishes[0].subject)
	}
	if js.publishes[0].opts.MsgID != expectedMsgID {
		t.Fatalf("publish msg id=%q", js.publishes[0].opts.MsgID)
	}
	if err := js.deliver("cube.sync.remote_entity", js.publishes[0].data); err != nil {
		t.Fatalf("sync handler errors must be acknowledged, got %v", err)
	}
}

func TestJetStreamSyncBusSkipsSelfMessages(t *testing.T) {
	js := newFakeJetStream()
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{
		LocalSid: 9,
		Prefix:   "cube.sync",
		Stream:   "CUBE_SYNC_TEST",
		Storage:  fnats.JetStreamStorageMemory,
	})
	if err != nil {
		t.Fatalf("NewJetStreamSyncBus error: %v", err)
	}
	called := false
	if _, err := bus.Subscribe("player.public", func(*fsyncbus.SyncMsg) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	if err := bus.Publish(&fsyncbus.SyncMsg{Topic: "player.public", Key: 44, Version: 1}); err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if err := js.deliver("cube.sync.player.public", js.publishes[0].data); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("handler called for self message")
	}
}

func TestJetStreamSyncPublishHonorsCanceledContext(t *testing.T) {
	js := newFakeJetStream()
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = bus.PublishContext(ctx, &fsyncbus.SyncMsg{Topic: "player", Key: 1, Version: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishContext error=%v, want context.Canceled", err)
	}
	if len(js.publishes) != 0 {
		t.Fatal("canceled publish reached transport")
	}
}

func TestJetStreamSyncUnsubscribeRemovesTrackedSubscription(t *testing.T) {
	js := newFakeJetStream()
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 1})
	if err != nil {
		t.Fatal(err)
	}
	unsub, err := bus.Subscribe("player", func(*fsyncbus.SyncMsg) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(bus.subs) != 1 {
		t.Fatalf("tracked subscriptions=%d, want 1", len(bus.subs))
	}
	unsub()
	if len(bus.subs) != 0 {
		t.Fatalf("tracked subscriptions=%d, want 0", len(bus.subs))
	}
}

type fakeJetStream struct {
	streams   []fnats.JetStreamConfig
	consumers []fnats.JetStreamConsumerConfig
	handlers  map[string]fnats.JetStreamHandler
	publishes []fakeJetStreamPublish
}

type fakeJetStreamPublish struct {
	subject string
	data    []byte
	opts    fnats.JetStreamPublishOptions
}

func newFakeJetStream() *fakeJetStream {
	return &fakeJetStream{handlers: make(map[string]fnats.JetStreamHandler)}
}

func (f *fakeJetStream) EnsureStream(_ context.Context, cfg fnats.JetStreamConfig) error {
	f.streams = append(f.streams, cfg)
	return nil
}

func (f *fakeJetStream) Publish(ctx context.Context, subject string, data []byte, opts fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	f.publishes = append(f.publishes, fakeJetStreamPublish{subject: subject, data: append([]byte(nil), data...), opts: opts})
	return fnats.JetStreamPublishAck{Stream: "fake", Sequence: uint64(len(f.publishes))}, nil
}

func (f *fakeJetStream) deliver(subject string, data []byte) error {
	handler := f.handlers[subject]
	if handler == nil {
		return nil
	}
	var msg fsyncbus.SyncMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	return handler(context.Background(), &fnats.JetStreamMsg{
		Subject:      subject,
		Data:         data,
		Stream:       "fake",
		Consumer:     "fake",
		StreamSeq:    uint64(len(f.publishes)),
		ConsumerSeq:  uint64(len(f.publishes)),
		NumDelivered: 1,
	})
}

func TestDurableSyncNameAvoidsSanitizationCollision(t *testing.T) {
	left := durableSyncName("player.public", 7)
	right := durableSyncName("player/public", 7)
	if left == right {
		t.Fatalf("durable names collide: %q", left)
	}
}

func (f *fakeJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	f.consumers = append(f.consumers, cfg)
	f.handlers[cfg.FilterSubject] = handler
	return fakeJetStreamSubscription{}, nil
}

type fakeJetStreamSubscription struct{}

func (fakeJetStreamSubscription) Stop()  {}
func (fakeJetStreamSubscription) Drain() {}
func (fakeJetStreamSubscription) Closed() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestJetStreamSyncConfigDefaults(t *testing.T) {
	cfg := normalizeJetStreamSyncConfig(JetStreamSyncConfig{LocalSid: 3})
	if cfg.Prefix != "cube.sync" || cfg.Stream != "CUBE_SYNC" || cfg.AckWait != 10*time.Second || cfg.MaxDeliver != 5 {
		t.Fatalf("defaults mismatch: %+v", cfg)
	}
	if cfg.StreamMaxAge != 30*time.Minute || cfg.SetupTimeout != 5*time.Second || cfg.Duplicates != 2*time.Minute {
		t.Fatalf("duration defaults mismatch: %+v", cfg)
	}
}
