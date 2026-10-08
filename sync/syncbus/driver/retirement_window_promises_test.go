// RR-20261008-25：空窗不 ACK、旧 consumer 未退出不重建、有限次数退避。
package driver

import (
	"context"
	"errors"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestLastUnsubscribeWindowDoesNotAcknowledgeUndelivered(t *testing.T) {
	js := newFakeJetStream()
	b, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = js.deliver("roost.sync.state", fanoutMsg(t, 1)); err == nil {
		t.Fatal("empty retired fanout acknowledged a message no handler received")
	}
}

type heldRetirement struct {
	stopped chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (s *heldRetirement) Stop()                   { s.once.Do(func() { close(s.stopped) }) }
func (s *heldRetirement) Drain()                  { s.Stop() }
func (s *heldRetirement) Closed() <-chan struct{} { return s.closed }

type retirementJS struct {
	*fakeJetStream
	old     *heldRetirement
	created chan struct{}
}

func (j *retirementJS) Subscribe(ctx context.Context, c fnats.JetStreamConsumerConfig, h fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	_, err := j.fakeJetStream.Subscribe(ctx, c, h)
	if err != nil {
		return nil, err
	}
	if len(j.consumers) == 1 {
		return j.old, nil
	}
	close(j.created)
	return fakeJetStreamSubscription{}, nil
}
func TestRecreateWaitsForPreviousConsumerClosed(t *testing.T) {
	old := &heldRetirement{stopped: make(chan struct{}), closed: make(chan struct{})}
	js := &retirementJS{fakeJetStream: newFakeJetStream(), old: old, created: make(chan struct{})}
	b, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-old.stopped
	_, err = b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
	if err == nil {
		t.Error("new Subscribe succeeded while old consumer remains open")
	}
	if len(js.consumers) != 1 {
		t.Error("new Consume started before old consumer Closed")
	}
	close(old.closed)
	b.Stop()
}
func TestStoppingWindowKeepsDeliveryLimitAndBackoff(t *testing.T) {
	js := newFakeJetStream()
	b, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	b.Stop()
	c := js.consumers[0]
	if c.MaxDeliver != 5 || c.NakBackoffMin <= 0 || c.NakBackoffMax <= 0 {
		t.Fatalf("finite delivery limit or lifecycle backoff missing: %+v", c)
	}
	if err = js.deliver("roost.sync.state", fanoutMsg(t, 2)); err == nil {
		t.Fatal("stopping callback acknowledged instead of returning delivery")
	}
}

func waitRetired(t *testing.T, b *jetStreamSyncBus, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		absent := b.topics[key] == nil
		b.mu.Unlock()
		if absent {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("consumer did not retire")
}

func TestRR25HandlerOutcomeIsNotAdmission(t *testing.T) {
	for _, mode := range []string{"unsubscribed_error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			js := newFakeJetStream()
			b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
			defer b.Stop()
			_, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error {
				if mode == "panic" {
					panic("business")
				}
				return fsyncbus.ErrUnsubscribed
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.deliver("roost.sync.state", fanoutMsg(t, 1)); err != nil {
				t.Fatalf("business invocation was classified as no receiver: %v", err)
			}
		})
	}
}
func TestRR25SelfUnsubscribeAndResubscribeDoesNotWaitForItself(t *testing.T) {
	js := newFakeJetStream()
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	defer b.Stop()
	var local *fsyncbus.Subscription
	local, _ = b.Subscribe("state", func(ctx context.Context, _ *fsyncbus.SyncMsg) error {
		if err := local.Unsubscribe(ctx); err != nil {
			t.Error(err)
		}
		_, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
		if !errors.Is(err, fsyncbus.ErrSubscriptionBusy) {
			t.Errorf("self restart = %v", err)
		}
		return nil
	})
	if err := js.deliver("roost.sync.state", fanoutMsg(t, 1)); err != nil {
		t.Fatal(err)
	}
	waitRetired(t, b, "state")
	if _, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

type creatingJS struct {
	*fakeJetStream
	entered chan struct{}
	fail    bool
}

func (j *creatingJS) Subscribe(ctx context.Context, c fnats.JetStreamConsumerConfig, h fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if c.FilterSubject == "roost.sync.blocked" {
		close(j.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if j.fail {
		return nil, errors.New("create failed")
	}
	return j.fakeJetStream.Subscribe(ctx, c, h)
}
func TestRR25CreationCancellationAndIndependentTopics(t *testing.T) {
	js := &creatingJS{fakeJetStream: newFakeJetStream(), entered: make(chan struct{})}
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	result := make(chan error, 1)
	handler := func(context.Context, *fsyncbus.SyncMsg) error { return nil }
	go func() { _, err := b.Subscribe("blocked", handler); result <- err }()
	<-js.entered
	if _, err := b.Subscribe("blocked", handler); !errors.Is(err, fsyncbus.ErrSubscriptionBusy) {
		t.Fatalf("creating = %v", err)
	}
	if _, err := b.Subscribe("other", handler); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.StopWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("create = %v", err)
	}
}
func TestRR25CreationFailureCanRetry(t *testing.T) {
	js := &creatingJS{fakeJetStream: newFakeJetStream(), fail: true}
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	defer b.Stop()
	handler := func(context.Context, *fsyncbus.SyncMsg) error { return nil }
	if _, err := b.Subscribe("state", handler); err == nil {
		t.Fatal("expected create error")
	}
	waitRetired(t, b, "state")
	js.fail = false
	if _, err := b.Subscribe("state", handler); err != nil {
		t.Fatal(err)
	}
}
func TestRR25SnapshotDoesNotMeanAnyHandlerAccepted(t *testing.T) {
	js := newFakeJetStream()
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	defer b.Stop()
	local, _ := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { t.Fatal("retired handler invoked"); return nil })
	b.mu.Lock()
	f := b.topics["state"]
	stale := f.snapshot()
	b.mu.Unlock()
	if err := local.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.invoke(stale[0], &fsyncbus.SyncMsg{}) {
		t.Fatal("stale snapshot counted as delivery")
	}
}

type immediateJS struct {
	*fakeJetStream
	data []byte
}

func (j *immediateJS) Subscribe(ctx context.Context, c fnats.JetStreamConsumerConfig, h fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if err := h(ctx, &fnats.JetStreamMsg{Data: j.data}); err != nil {
		return nil, err
	}
	return j.fakeJetStream.Subscribe(ctx, c, h)
}
func TestRR25DeliveryDuringCreationHasAReceiver(t *testing.T) {
	js := &immediateJS{fakeJetStream: newFakeJetStream(), data: fanoutMsg(t, 1)}
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	defer b.Stop()
	calls := 0
	if _, err := b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("early callback calls=%d", calls)
	}
}
func TestRR25StopWaitsForClosedAndCanRetry(t *testing.T) {
	old := &heldRetirement{stopped: make(chan struct{}), closed: make(chan struct{})}
	js := &retirementJS{fakeJetStream: newFakeJetStream(), old: old, created: make(chan struct{})}
	b, _ := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	_, _ = b.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.StopWithContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop before Closed = %v", err)
	}
	close(old.closed)
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}
