package nettransport

import (
	"context"
	"errors"
	"testing"

	core "github.com/tjbdwanghaibo/cube-core/statesync"
)

func TestAsyncTransportAdmitBatchIsAtomicOnReliableBackpressure(t *testing.T) {
	transport, err := NewAsyncTransport(core.TransportFunc{
		Datagram: func(context.Context, core.SessionID, []byte) error { return nil },
		Reliable: func(context.Context, core.SessionID, []byte) error { return nil },
	}, AsyncTransportConfig{ReliableQueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []core.SessionID{1, 2} {
		if err := transport.RegisterSession(core.SessionInfo{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	transport.mu.RLock()
	first := transport.sessions[1]
	second := transport.sessions[2]
	first.mu.Lock()
	first.reliable = append(first.reliable, []byte("full"))
	first.mu.Unlock()
	transport.mu.RUnlock()

	err = transport.AdmitBatch(context.Background(), []OutboundFrame{
		{Session: 1, Reliable: []byte("one")},
		{Session: 2, Reliable: []byte("two")},
	})
	if !errors.Is(err, ErrReliableBackpressure) {
		t.Fatalf("expected reliable backpressure, got %v", err)
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.reliable) != 0 {
		t.Fatalf("second session was partially admitted: %d", len(second.reliable))
	}
}

func TestAsyncTransportKeepsLatestPerRoom(t *testing.T) {
	transport, err := NewAsyncTransport(core.TransportFunc{
		Datagram: func(context.Context, core.SessionID, []byte) error { return nil },
		Reliable: func(context.Context, core.SessionID, []byte) error { return nil },
	}, DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 1}); err != nil {
		t.Fatal(err)
	}
	transport.mu.RLock()
	queue := transport.sessions[1]
	queue.mu.Lock()
	queue.latest[10] = [][]byte{{1}}
	queue.latest[20] = [][]byte{{2}}
	queue.latestOrder = []uint64{10, 20}
	queue.latest[10] = [][]byte{{3}}
	if len(queue.latest) != 2 || len(queue.latestOrder) != 2 {
		t.Fatalf("latest queues were not isolated by room: %d/%d", len(queue.latest), len(queue.latestOrder))
	}
	queue.mu.Unlock()
	transport.mu.RUnlock()
}
