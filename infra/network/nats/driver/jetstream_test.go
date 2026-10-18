package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type fakeConsumeContext struct {
	closed        chan struct{}
	drainCalled   chan struct{}
	stopCalled    chan struct{}
	drainObserved func()
}

func TestJetStreamTerminalClassification(t *testing.T) {
	cause := errors.New("bad wire")
	if got := terminalReason(fnats.Permanent(cause), 10, 1); got != "permanent" {
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
		Name:       "ROOST_DOMAIN_EVENTS",
		Subjects:   []string{"roost.domain.>"},
		Storage:    fnats.JetStreamStorageMemory,
		MaxAge:     time.Hour,
		Duplicates: time.Minute,
		Replicas:   2,
		MaxBytes:   1024,
	})

	if got.Name != "ROOST_DOMAIN_EVENTS" || got.Subjects[0] != "roost.domain.>" {
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
		FilterSubject: "roost.domain.battle.settled",
		DeliverPolicy: fnats.JetStreamDeliverNew,
		AckWait:       3 * time.Second,
		MaxDeliver:    5,
		MaxAckPending: 128,
	})

	if got.Name != "task-progress" || got.Durable != "task-progress" {
		t.Fatalf("consumer identity mismatch: %+v", got)
	}
	if got.FilterSubject != "roost.domain.battle.settled" {
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
	sub := newJetStreamSubscription(cc, cancel)
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
	sub := newJetStreamSubscription(cc, cancel)
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

// settleMsg is a JetStream message whose acknowledgement path can be made to
// fail. Only the methods the settle path touches are implemented; the embedded
// interface panics on anything else, which is what we want in a unit test.
type settleMsg struct {
	gojs.Msg
	ackErr  error
	nakErr  error
	termErr error
	calls   []string
}

func (m *settleMsg) Subject() string                      { return "orders.created" }
func (m *settleMsg) Data() []byte                         { return []byte("x") }
func (m *settleMsg) Metadata() (*gojs.MsgMetadata, error) { return nil, errors.New("no metadata") }
func (m *settleMsg) Ack() error                           { m.calls = append(m.calls, "ack"); return m.ackErr }
func (m *settleMsg) Nak() error                           { m.calls = append(m.calls, "nak"); return m.nakErr }
func (m *settleMsg) NakWithDelay(delay time.Duration) error {
	m.calls = append(m.calls, "nak_delay")
	return m.nakErr
}
func (m *settleMsg) Term() error { m.calls = append(m.calls, "term"); return m.termErr }

func counterValue(t *testing.T, reg *metrics.Registry, name string, labels metrics.Labels) (int64, bool) {
	t.Helper()
	for _, metric := range reg.Snapshot() {
		if metric.Name != name {
			continue
		}
		match := true
		for key, want := range labels {
			if metric.Labels[key] != want {
				match = false
			}
		}
		if match {
			return metric.Value, true
		}
	}
	return 0, false
}

func swapRegistry(t *testing.T) *metrics.Registry {
	t.Helper()
	previous := metrics.DefaultRegistry()
	registry := metrics.NewRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
	return registry
}

// A failed Ack means the broker will redeliver a message the handler already
// processed. That is allowed (at-least-once), but it must not be invisible:
// the operator has to be able to tell "handler keeps failing" from "ack path
// is broken".
func TestJetStreamSettleFailureIsCountedPerOperation(t *testing.T) {
	registry := swapRegistry(t)
	cfg := fnats.JetStreamConsumerConfig{MaxDeliver: 3}

	ackFail := &settleMsg{ackErr: errors.New("nats: connection closed")}
	settleJetStreamDelivery(ackFail, jetStreamMsg(ackFail), cfg, nil)
	if got, ok := counterValue(t, registry, "nats.jetstream.settle_failures.total", metrics.Labels{"op": "ack"}); !ok || got != 1 {
		t.Fatalf("ack failure counter = %d (%v), want 1", got, ok)
	}

	nakFail := &settleMsg{nakErr: errors.New("nats: timeout")}
	settleJetStreamDelivery(nakFail, jetStreamMsg(nakFail), cfg, errors.New("handler failed"))
	if got, ok := counterValue(t, registry, "nats.jetstream.settle_failures.total", metrics.Labels{"op": "nak"}); !ok || got != 1 {
		t.Fatalf("nak failure counter = %d (%v), want 1", got, ok)
	}
	if len(nakFail.calls) != 1 || nakFail.calls[0] != "nak" {
		t.Fatalf("nak path calls = %v, want [nak]", nakFail.calls)
	}

	termFail := &settleMsg{termErr: errors.New("nats: timeout")}
	settleJetStreamDelivery(termFail, jetStreamMsg(termFail), cfg, fnats.Permanent(errors.New("poison")))
	if got, ok := counterValue(t, registry, "nats.jetstream.settle_failures.total", metrics.Labels{"op": "term"}); !ok || got != 1 {
		t.Fatalf("term failure counter = %d (%v), want 1", got, ok)
	}
	if len(termFail.calls) != 1 || termFail.calls[0] != "term" {
		t.Fatalf("term path calls = %v, want [term]", termFail.calls)
	}
}

// The counter is a failure counter: a clean settle must not touch it.
func TestJetStreamSettleSuccessLeavesFailureCounterUntouched(t *testing.T) {
	registry := swapRegistry(t)
	cfg := fnats.JetStreamConsumerConfig{}
	ok := &settleMsg{}
	settleJetStreamDelivery(ok, jetStreamMsg(ok), cfg, nil)
	settleJetStreamDelivery(ok, jetStreamMsg(ok), cfg, errors.New("transient"))
	if _, found := counterValue(t, registry, "nats.jetstream.settle_failures.total", nil); found {
		t.Fatalf("settle failure counter must stay absent when Ack/Nak succeed")
	}
	if len(ok.calls) != 2 || ok.calls[0] != "ack" || ok.calls[1] != "nak" {
		t.Fatalf("calls = %v, want [ack nak]", ok.calls)
	}
}

// 推迟结算（JetStreamMsg.DeferSettle）：handler 推迟后返回 nil，回调返回时不 ack；settle(nil) 才 ack。Drain 之后
// ConsumeContext 关闭，订阅的 Closed 与 handler 的 ctx 仍等着推迟中的结算——Drain 的调用方等到的是“后台结算完成”。
func TestJetStreamDeferredSettleAcksLaterAndDrainWaitsForIt(t *testing.T) {
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc := newFakeConsumeContext()
	sub := newJetStreamSubscription(cc, cancel)
	msg := &settleMsg{}
	var settle func(error)
	sub.deliver(handlerCtx, func(_ context.Context, m *fnats.JetStreamMsg) error {
		settle = m.DeferSettle()
		if m.DeferSettle() != nil {
			t.Error("a second DeferSettle on the same delivery must return nil")
		}
		return nil
	}, msg, fnats.JetStreamConsumerConfig{})
	if settle == nil {
		t.Fatal("DeferSettle returned nil on a live subscription")
	}
	if len(msg.calls) != 0 {
		t.Fatalf("deferred delivery settled when the handler returned: %v", msg.calls)
	}
	sub.Drain()
	close(cc.closed)
	select {
	case <-sub.Closed():
		t.Fatal("Closed fired while a deferred settle was pending")
	case <-handlerCtx.Done():
		t.Fatal("handler context cancelled while a deferred settle was pending")
	case <-time.After(50 * time.Millisecond):
	}
	settle(nil)
	settle(errors.New("late")) // 幂等：不再结算
	select {
	case <-sub.Closed():
	case <-time.After(time.Second):
		t.Fatal("Closed did not fire after the deferred settle")
	}
	select {
	case <-handlerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("handler context outlived the drained subscription")
	}
	if len(msg.calls) != 1 || msg.calls[0] != "ack" {
		t.Fatalf("settle calls = %v, want [ack]", msg.calls)
	}
}

// handler 推迟之后仍返回错误：按错误立即结算（nak / term 规则不变），之后的 settle 不再生效；ConsumeContext 关闭后
// 不再接受推迟（返回 nil，handler 同步处理）。
func TestJetStreamDeferredSettleYieldsToAHandlerErrorAndClosedSubscriptions(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	cc := newFakeConsumeContext()
	sub := newJetStreamSubscription(cc, cancel)
	failed := &settleMsg{}
	var settle func(error)
	sub.deliver(context.Background(), func(_ context.Context, m *fnats.JetStreamMsg) error {
		settle = m.DeferSettle()
		return fnats.Permanent(errors.New("poison"))
	}, failed, fnats.JetStreamConsumerConfig{})
	settle(nil)
	if len(failed.calls) != 1 || failed.calls[0] != "term" {
		t.Fatalf("settle calls = %v, want [term] from the handler error", failed.calls)
	}
	panicked := &settleMsg{}
	sub.deliver(context.Background(), func(_ context.Context, m *fnats.JetStreamMsg) error {
		m.DeferSettle()
		panic("boom")
	}, panicked, fnats.JetStreamConsumerConfig{})
	if len(panicked.calls) != 1 || panicked.calls[0] != "nak" {
		t.Fatalf("settle calls after a panic = %v, want [nak]", panicked.calls)
	}
	close(cc.closed)
	select {
	case <-sub.Closed():
	case <-time.After(time.Second):
		t.Fatal("Closed did not fire with nothing pending")
	}
	late := &settleMsg{}
	sub.deliver(context.Background(), func(_ context.Context, m *fnats.JetStreamMsg) error {
		if m.DeferSettle() != nil {
			t.Error("DeferSettle accepted after the subscription ended")
		}
		return nil
	}, late, fnats.JetStreamConsumerConfig{})
	if len(late.calls) != 1 || late.calls[0] != "ack" {
		t.Fatalf("settle calls = %v, want a synchronous [ack]", late.calls)
	}
}

// Stop 取消 handler 的 ctx（后台工作据此尽快结算），Closed 仍等推迟中的结算完成。
func TestJetStreamStopCancelsDeferredWorkButClosedWaitsForTheSettle(t *testing.T) {
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc := newFakeConsumeContext()
	sub := newJetStreamSubscription(cc, cancel)
	msg := &settleMsg{}
	var settle func(error)
	sub.deliver(handlerCtx, func(_ context.Context, m *fnats.JetStreamMsg) error {
		settle = m.DeferSettle()
		return nil
	}, msg, fnats.JetStreamConsumerConfig{})
	sub.Stop()
	close(cc.closed)
	select {
	case <-handlerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the handler context")
	}
	select {
	case <-sub.Closed():
		t.Fatal("Closed fired before the deferred settle")
	case <-time.After(50 * time.Millisecond):
	}
	settle(handlerCtx.Err())
	select {
	case <-sub.Closed():
	case <-time.After(time.Second):
		t.Fatal("Closed did not fire after the settle")
	}
	if len(msg.calls) != 1 || msg.calls[0] != "nak" {
		t.Fatalf("settle calls = %v, want [nak]", msg.calls)
	}
}
