package nats

import (
	"errors"
	"strings"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"

	gonats "github.com/nats-io/nats.go"
)

// Every gonats failure that callers branch on is translated to a roost-core
// sentinel; a translation that stops matching would make callers fall into
// their generic-error path (retry a timeout that was a closed connection, or
// give up on a "no responders" that a retry would have served).
func TestClientTranslatesEachTransportErrorToItsSentinel(t *testing.T) {
	c := &natsClient{}
	cases := map[error]error{
		gonats.ErrTimeout:            fnats.ErrTimeout,
		gonats.ErrNoResponders:       fnats.ErrNoResponders,
		gonats.ErrConnectionClosed:   fnats.ErrClosed,
		gonats.ErrConnectionDraining: fnats.ErrClosed,
	}
	for in, want := range cases {
		if got := c.wrapError(in); !errors.Is(got, want) {
			t.Fatalf("wrapError(%v) = %v, want %v", in, got, want)
		}
	}
	other := errors.New("something else")
	if got := c.wrapError(other); !errors.Is(got, other) {
		t.Fatalf("unknown errors must pass through: %v", got)
	}
}

// Subjects and queues are validated before touching the connection: a padded
// or empty subject would silently subscribe to nothing, and a client with no
// connection reports ErrClosed rather than panicking.
func TestClientRefusesInvalidSubjectsQueuesAndHandlers(t *testing.T) {
	var closed *natsClient
	if err := closed.validateSubject("roost.x"); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("nil client = %v", err)
	}
	if err := (&natsClient{}).validateSubject("roost.x"); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("client without connection = %v", err)
	}
	c := &natsClient{conn: &gonats.Conn{}}
	for _, subject := range []string{"", "  ", " roost.x", "roost.x "} {
		if err := c.validateSubject(subject); err == nil || !strings.Contains(err.Error(), "invalid subject") {
			t.Fatalf("subject %q = %v", subject, err)
		}
	}
	handler := func(*fnats.Msg) {}
	if err := c.validateSubscription("roost.x", "", nil); err == nil || !strings.Contains(err.Error(), "handler is nil") {
		t.Fatalf("nil handler = %v", err)
	}
	for _, queue := range []string{" ", " workers", "workers "} {
		if err := c.validateSubscription("roost.x", queue, handler); err == nil || !strings.Contains(err.Error(), "invalid queue") {
			t.Fatalf("queue %q = %v", queue, err)
		}
	}
	if err := c.validateSubscription("roost.x", "workers", handler); err != nil {
		t.Fatalf("valid subscription refused: %v", err)
	}
	if _, err := c.Request("roost.x", nil, 0); err == nil || !strings.Contains(err.Error(), "request timeout must be positive") {
		t.Fatalf("non-positive request timeout = %v", err)
	}
}
