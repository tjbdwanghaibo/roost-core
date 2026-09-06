package nats

import (
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"

	gojs "github.com/nats-io/nats.go/jetstream"
)

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
