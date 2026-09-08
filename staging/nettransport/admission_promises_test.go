package nettransport

import (
	"context"
	"errors"
	"testing"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

func admissionTransport(t *testing.T, mutate func(*AsyncTransportConfig)) *AsyncTransport {
	t.Helper()
	downstream := core.TransportFunc{
		Datagram: func(context.Context, core.SessionID, []byte) error { return nil },
		Reliable: func(context.Context, core.SessionID, []byte) error { return nil },
	}
	cfg := DefaultAsyncTransportConfig()
	cfg.AllowOpaqueDatagrams = true // isolate the transport's own limits from frame inspection
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
	if err := transport.RegisterSession(core.SessionInfo{ID: 0}); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("zero session id = %v", err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 7}); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("duplicate session = %v", err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 8}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("over MaxSessions = %v", err)
	}
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 9}); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("register after Close = %v", err)
	}
}

// AdmitBatch validates every frame before touching any queue: a frame must
// name a session and carry exactly one of datagrams / reliable, a reliable
// message is bounded by MaxReliableBytes, and a datagram batch by count and
// per-packet size.
func TestAdmitBatchRefusesEachMalformedFrame(t *testing.T) {
	transport := admissionTransport(t, func(c *AsyncTransportConfig) {
		c.MaxReliableBytes = 8
		c.MaxDatagramsPerFrame = 2
		c.MaxDatagramBytes = 4
	})
	if err := transport.RegisterSession(core.SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		name  string
		frame OutboundFrame
		want  error
	}{
		{"no session", OutboundFrame{Reliable: []byte("x")}, ErrProtocolConfig},
		{"neither lane", OutboundFrame{Session: 7}, ErrProtocolConfig},
		{"both lanes", OutboundFrame{Session: 7, Datagrams: [][]byte{{1}}, Reliable: []byte("x")}, ErrProtocolConfig},
		{"reliable too big", OutboundFrame{Session: 7, Reliable: make([]byte, 9)}, ErrReliableMessageTooBig},
		{"too many datagrams", OutboundFrame{Session: 7, Datagrams: [][]byte{{1}, {2}, {3}}}, ErrInvalidDatagramBatch},
		{"empty datagram", OutboundFrame{Session: 7, Datagrams: [][]byte{{}}}, ErrInvalidDatagramBatch},
		{"oversized datagram", OutboundFrame{Session: 7, Datagrams: [][]byte{make([]byte, 5)}}, ErrInvalidDatagramBatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := transport.AdmitBatch(ctx, []OutboundFrame{tc.frame}); !errors.Is(err, tc.want) {
				t.Fatalf("AdmitBatch = %v, want %v", err, tc.want)
			}
		})
	}
	if err := transport.AdmitBatch(ctx, []OutboundFrame{{Session: 7, Reliable: []byte("12345678")}, {Session: 7, Datagrams: [][]byte{{1, 2, 3, 4}, {5}}}}); err != nil {
		t.Fatalf("frames at the limits must be admitted: %v", err)
	}
}
