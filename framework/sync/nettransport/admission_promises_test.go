package nettransport

import (
	"context"
	"errors"
	"testing"
)

func admissionTransport(t *testing.T, mutate func(*AsyncTransportConfig)) *AsyncTransport {
	t.Helper()
	downstream := TransportFunc{
		Datagram: func(context.Context, SessionID, []byte) error { return nil },
		Reliable: func(context.Context, SessionID, []byte) error { return nil },
	}
	cfg := DefaultAsyncTransportConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	transport, err := NewAsyncTransport(downstream, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close(context.Background()) })
	return transport
}

// Session registration is bounded and exclusive: a zero id, a second
// registration of the same id, more sessions than configured, and any
// registration after Close are refused with their own sentinel.
func TestRegisterSessionRefusesZeroDuplicateOverLimitAndClosed(t *testing.T) {
	transport := admissionTransport(t, func(c *AsyncTransportConfig) { c.MaxSessions = 1 })
	if err := transport.RegisterSession(SessionInfo{ID: 0}); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("zero session id = %v", err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 7}); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("duplicate session = %v", err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 8}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("over MaxSessions = %v", err)
	}
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 9}); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("register after Close = %v", err)
	}
}

// SendReliable validates before touching the queue: a session id, a
// non-empty payload, and a payload within MaxReliableBytes; a message exactly
// at the limit is admitted.
func TestSendReliableRefusesEachMalformedMessage(t *testing.T) {
	transport := admissionTransport(t, func(c *AsyncTransportConfig) { c.MaxReliableBytes = 8 })
	if err := transport.RegisterSession(SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		name    string
		session SessionID
		payload []byte
		want    error
	}{
		{"no session", 0, []byte("x"), ErrSessionNotRegistered},
		{"empty payload", 7, nil, ErrProtocolConfig},
		{"too big", 7, make([]byte, 9), ErrReliableMessageTooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := transport.SendReliable(ctx, tc.session, tc.payload); !errors.Is(err, tc.want) {
				t.Fatalf("SendReliable = %v, want %v", err, tc.want)
			}
		})
	}
	if err := transport.SendReliable(ctx, 7, []byte("12345678")); err != nil {
		t.Fatalf("a message at the limit must be admitted: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := transport.SendReliable(cancelled, 7, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendReliable with a cancelled context = %v", err)
	}
}
