package nats

import (
	"errors"
	"strings"
	"testing"

	gonats "github.com/nats-io/nats.go"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func TestNatsClientNilBoundaryFailsClosed(t *testing.T) {
	var client *natsClient
	if err := client.Publish("topic", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("Publish error=%v, want ErrClosed", err)
	}
	client.Close()
}

// FEATURE_LOGIC M8 item 2: a handler panic is contained AND reported. The
// previous version of this test set a flag after the call and asserted the
// flag — an assertion that cannot fail once the line is reached, so it proved
// containment only by not crashing and proved reporting not at all. Reverting
// the recover branch to swallow the panic silently left it green. The counter
// is the observable.
func TestInvokeNatsHandlerContainsAndReportsPanic(t *testing.T) {
	before := panicCounter()
	invokeNatsHandler(func(*fnats.Msg) { panic("boom") }, &fnats.Msg{Subject: "test"})
	if got := panicCounter() - before; got != 1 {
		t.Fatalf("handler_panic counter moved by %d, want 1: the panic was swallowed without being reported", got)
	}
}

func panicCounter() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nats.subscription.handler_panic.total" && len(metric.Labels) == 0 {
			return metric.Value
		}
	}
	return 0
}

// M8 item 1: a subscription with no handler is refused before anything is
// registered with the server. Reverting the check made no test red before
// this one existed — the nil-boundary test above covers a nil client, not a
// nil callback.
func TestSubscriptionValidationRejectsANilHandler(t *testing.T) {
	client := &natsClient{conn: &gonats.Conn{}}
	if err := client.validateSubscription("subject", "", nil); err == nil || !strings.Contains(err.Error(), "handler") {
		t.Fatalf("nil handler accepted: %v", err)
	}
	if err := client.validateSubscription("", "", func(*fnats.Msg) {}); err == nil {
		t.Fatal("empty subject accepted")
	}
	if err := client.validateSubscription("subject", "queue", func(*fnats.Msg) {}); err != nil {
		t.Fatalf("a valid subscription was refused: %v", err)
	}
}
