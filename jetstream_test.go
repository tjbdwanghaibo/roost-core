package nats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"

	gojs "github.com/nats-io/nats.go/jetstream"
)

type fakeConsumeContext struct {
	closed        chan struct{}
	drainCalled   chan struct{}
	stopCalled    chan struct{}
	drainObserved func()
}

func TestJetStreamTerminalClassification(t *testing.T) {
	cause := errors.New("bad wire")
	if got := terminalReason(Permanent(cause), 10, 1); got != "permanent" {
		t.Fatalf("permanent reason=%q", got)
	}
	if got := terminalReason(cause, 3, 3); got != "max_deliver" {
		t.Fatalf("max-deliver reason=%q", got)
	}
	if got := terminalReason(cause, 3, 2); got != "" {
		t.Fatalf("transient delivery terminated early: %q", got)
	}
}

func TestInvokeJetStreamHandlerContainsPanic(t *testing.T) {
	err := invokeJetStreamHandler(context.Background(), func(context.Context, *fnats.JetStreamMsg) error {
		panic("boom")
	}, &fnats.JetStreamMsg{})
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("panic was not contained: %v", err)
	}
}

func newFakeConsumeContext() *fakeConsumeContext {
	return &fakeConsumeContext{
		closed:      make(chan struct{}),
		drainCalled: make(chan struct{}),
		stopCalled:  make(chan struct{}),
	}
}

func (c *fakeConsumeContext) Stop() {
	close(c.stopCalled)
}

func (c *fakeConsumeContext) Drain() {
	if c.drainObserved != nil {
		c.drainObserved()
	}
	close(c.drainCalled)
}

func (c *fakeConsumeContext) Closed() <-chan struct{} { return c.closed }

func TestJetStreamStreamConfigMapping(t *testing.T) {
	got := toJetStreamStreamConfig(fnats.JetStreamConfig{
		Name:       "CUBE_DOMAIN_EVENTS",
		Subjects:   []string{"cube.domain.>"},
		Storage:    fnats.JetStreamStorageMemory,
		MaxAge:     time.Hour,
		Duplicates: time.Minute,
		Replicas:   2,
		MaxBytes:   1024,
	})

	if got.Name != "CUBE_DOMAIN_EVENTS" || got.Subjects[0] != "cube.domain.>" {
		t.Fatalf("stream identity mismatch: %+v", got)
	}
	if got.Storage != gojs.MemoryStorage {
		t.Fatalf("storage=%v, want memory", got.Storage)
	}
	if got.MaxAge != time.Hour || got.Duplicates != time.Minute || got.Replicas != 2 || got.MaxBytes != 1024 {
		t.Fatalf("stream limits mismatch: %+v", got)
	}
}

func TestJetStreamConsumerConfigMappingDefaults(t *testing.T) {
	got := toJetStreamConsumerConfig(fnats.JetStreamConsumerConfig{
		Name:          "task-progress",
		Durable:       "task-progress",
		FilterSubject: "cube.domain.battle.settled",
		DeliverPolicy: fnats.JetStreamDeliverNew,
		AckWait:       3 * time.Second,
		MaxDeliver:    5,
		MaxAckPending: 128,
	})

	if got.Name != "task-progress" || got.Durable != "task-progress" {
		t.Fatalf("consumer identity mismatch: %+v", got)
	}
	if got.FilterSubject != "cube.domain.battle.settled" {
		t.Fatalf("filter=%q", got.FilterSubject)
	}
	if got.DeliverPolicy != gojs.DeliverNewPolicy {
		t.Fatalf("deliver policy=%v, want new", got.DeliverPolicy)
	}
	if got.AckPolicy != gojs.AckExplicitPolicy || got.AckWait != 3*time.Second || got.MaxDeliver != 5 || got.MaxAckPending != 128 {
		t.Fatalf("ack config mismatch: %+v", got)
	}
}

func TestJetStreamNakBackoffIsBounded(t *testing.T) {
	config := fnats.JetStreamConsumerConfig{NakBackoffMin: 250 * time.Millisecond, NakBackoffMax: 5 * time.Second}
	for deliveries, want := range map[uint64]time.Duration{1: 250 * time.Millisecond, 2: 500 * time.Millisecond, 3: time.Second, 10: 5 * time.Second} {
		if got := nakBackoff(config, deliveries); got != want {
			t.Fatalf("deliveries=%d got=%s want=%s", deliveries, got, want)
		}
	}
	if got := nakBackoff(fnats.JetStreamConsumerConfig{}, 10); got != 0 {
		t.Fatalf("zero config backoff=%s", got)
	}
}

func TestJetStreamDrainKeepsHandlerContextAliveUntilBufferedMessagesFinish(t *testing.T) {
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc := newFakeConsumeContext()
	cc.drainObserved = func() {
		select {
		case <-handlerCtx.Done():
			t.Fatal("handler context was cancelled before ConsumeContext.Drain")
		default:
		}
	}
	sub := &jetStreamSubscription{cc: cc, cancel: cancel}
	sub.Drain()
	select {
	case <-cc.drainCalled:
	default:
		t.Fatal("ConsumeContext.Drain was not called")
	}
	select {
	case <-handlerCtx.Done():
		t.Fatal("handler context was cancelled before ConsumeContext.Closed")
	default:
	}
	close(cc.closed)
	select {
	case <-handlerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("handler context remained alive after drain completion")
	}
}

func TestJetStreamStopCanForcePendingDrain(t *testing.T) {
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc := newFakeConsumeContext()
	sub := &jetStreamSubscription{cc: cc, cancel: cancel}
	sub.Drain()
	sub.Stop()
	select {
	case <-cc.stopCalled:
	default:
		t.Fatal("Stop did not force a pending drain to stop")
	}
	select {
	case <-handlerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the handler context")
	}
	close(cc.closed)
}
