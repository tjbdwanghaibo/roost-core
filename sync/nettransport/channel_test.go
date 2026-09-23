package nettransport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type blockingTransport struct {
	reliableStarted chan struct{}
	reliableRelease chan struct{}

	mu       sync.Mutex
	reliable [][]byte
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{reliableStarted: make(chan struct{}, 8), reliableRelease: make(chan struct{}, 8)}
}

func (transport *blockingTransport) SendReliable(ctx context.Context, _ SessionID, payload []byte) error {
	transport.reliableStarted <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-transport.reliableRelease:
	}
	transport.mu.Lock()
	transport.reliable = append(transport.reliable, append([]byte(nil), payload...))
	transport.mu.Unlock()
	return nil
}

// The queue is bounded per session: with one slot, the message in flight
// plus one queued fill it, the third is refused with ErrReliableBackpressure
// and nothing already admitted is lost.
func TestAsyncTransportReliableBackpressureKeepsAdmittedOrder(t *testing.T) {
	downstream := newBlockingTransport()
	config := DefaultAsyncTransportConfig()
	config.ReliableQueueSize = 1
	config.SendTimeout = time.Second
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 8}); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendReliable(context.Background(), 8, []byte("one")); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.reliableStarted)
	if err := transport.SendReliable(context.Background(), 8, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendReliable(context.Background(), 8, []byte("three")); !errors.Is(err, ErrReliableBackpressure) {
		t.Fatalf("expected backpressure, got %v", err)
	}
	downstream.reliableRelease <- struct{}{}
	await(t, downstream.reliableStarted)
	downstream.reliableRelease <- struct{}{}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	stats := transport.Stats()
	if stats.ReliableBackpressure != 1 || stats.ReliableSent != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	downstream.mu.Lock()
	defer downstream.mu.Unlock()
	if len(downstream.reliable) != 2 || string(downstream.reliable[0]) != "one" || string(downstream.reliable[1]) != "two" {
		t.Fatalf("admitted messages did not arrive in order: %q", downstream.reliable)
	}
}

func TestAsyncTransportReliableFailureIsTerminalAndHandlerPanicIsContained(t *testing.T) {
	downstream := &orderedFailureTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.SendTimeout = time.Second
	config.OnError = func(SendError) { panic("metrics sink panic") }
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 31}); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendReliable(context.Background(), 31, []byte("first")); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.started)
	if err := transport.SendReliable(context.Background(), 31, []byte("must-not-overtake")); err != nil {
		t.Fatal(err)
	}
	close(downstream.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	stats := transport.Stats()
	if downstream.Calls() != 1 || stats.ReliableAbandoned != 1 || stats.SendErrors != 1 || stats.ErrorHandlerPanics != 1 {
		t.Fatalf("reliable lane did not fail closed: calls=%d stats=%+v", downstream.Calls(), stats)
	}
}

type orderedFailureTransport struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (transport *orderedFailureTransport) SendReliable(context.Context, SessionID, []byte) error {
	transport.mu.Lock()
	transport.calls++
	call := transport.calls
	transport.mu.Unlock()
	if call == 1 {
		close(transport.started)
		<-transport.release
		return errors.New("ordered stream failed")
	}
	return nil
}

func (transport *orderedFailureTransport) Calls() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.calls
}

func TestAsyncTransportPreventsSessionIDReuseWhileOldSendDrains(t *testing.T) {
	downstream := &stubbornReliableTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.SendTimeout = time.Second
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	info := SessionInfo{ID: 41}
	if err := transport.RegisterSession(info); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendReliable(context.Background(), info.ID, []byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.started)
	if !transport.RemoveSession(info.ID) {
		t.Fatal("session should begin draining")
	}
	if err := transport.RegisterSession(info); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("session ID was reused while an old packet was in flight: %v", err)
	}
	close(downstream.release)
	deadline := time.Now().Add(time.Second)
	for {
		err = transport.RegisterSession(info)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrSessionAlreadyExists) || time.Now().After(deadline) {
			t.Fatalf("drained session ID was not released: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	transport.RemoveSession(info.ID)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

// stubbornReliableTransport ignores the send context once, so a session can
// be observed while its old send is still in flight.
type stubbornReliableTransport struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (transport *stubbornReliableTransport) SendReliable(context.Context, SessionID, []byte) error {
	transport.once.Do(func() { close(transport.started) })
	<-transport.release
	return nil
}

func TestTransportRejectsTypedNilDependencies(t *testing.T) {
	var nilDownstream *blockingTransport
	if _, err := NewAsyncTransport(nilDownstream, DefaultAsyncTransportConfig()); !errors.Is(err, ErrTransportRequired) {
		t.Fatalf("typed nil downstream was accepted: %v", err)
	}
	var nilDatagrams *UDPTransport
	composite := CompositeTransport{Datagrams: nilDatagrams, Reliable: nilDownstream}
	if err := composite.SendDatagram(context.Background(), 1, []byte{1}); !errors.Is(err, ErrTransportRequired) {
		t.Fatalf("typed nil datagram sender was accepted: %v", err)
	}
	if err := composite.SendReliable(context.Background(), 1, []byte{1}); !errors.Is(err, ErrTransportRequired) {
		t.Fatalf("typed nil reliable sender was accepted: %v", err)
	}
	if got := (SendError{}).Error(); got == "" {
		t.Fatal("nil SendError should remain safe and descriptive")
	}
}

func TestAsyncTransportCascadesSessionLifecycle(t *testing.T) {
	downstream := &lifecycleTransport{sessions: make(map[SessionID]bool)}
	transport, err := NewAsyncTransport(downstream, DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 61}); err != nil {
		t.Fatal(err)
	}
	downstream.mu.Lock()
	registered := downstream.sessions[61]
	downstream.mu.Unlock()
	if !registered {
		t.Fatal("downstream protocol route was not registered")
	}
	if !transport.RemoveSession(61) {
		t.Fatal("async session was not removed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		downstream.mu.Lock()
		removed := !downstream.sessions[61]
		downstream.mu.Unlock()
		if removed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downstream protocol route was not removed after drain")
		}
		time.Sleep(time.Millisecond)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

type lifecycleTransport struct {
	mu       sync.Mutex
	sessions map[SessionID]bool
}

func (*lifecycleTransport) SendReliable(context.Context, SessionID, []byte) error { return nil }
func (transport *lifecycleTransport) RegisterSession(info SessionInfo) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.sessions[info.ID] {
		return ErrSessionAlreadyExists
	}
	transport.sessions[info.ID] = true
	return nil
}
func (transport *lifecycleTransport) RemoveSession(id SessionID) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	exists := transport.sessions[id]
	delete(transport.sessions, id)
	return exists
}

func await(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport")
	}
}
