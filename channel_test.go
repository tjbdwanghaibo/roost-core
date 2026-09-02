package nettransport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	core "github.com/tjbdwanghaibo/cube-core/statesync"
)

type blockingTransport struct {
	datagramStarted chan struct{}
	datagramRelease chan struct{}
	reliableStarted chan struct{}
	reliableRelease chan struct{}

	mu        sync.Mutex
	datagrams [][][]byte
	reliable  [][]byte
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{
		datagramStarted: make(chan struct{}, 8), datagramRelease: make(chan struct{}, 8),
		reliableStarted: make(chan struct{}, 8), reliableRelease: make(chan struct{}, 8),
	}
}

func (transport *blockingTransport) SendDatagram(ctx context.Context, session core.SessionID, payload []byte) error {
	return transport.SendDatagramBatch(ctx, session, [][]byte{payload})
}

func (transport *blockingTransport) SendDatagramBatch(ctx context.Context, _ core.SessionID, packets [][]byte) error {
	transport.datagramStarted <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-transport.datagramRelease:
	}
	copyOfPackets := make([][]byte, len(packets))
	for index := range packets {
		copyOfPackets[index] = append([]byte(nil), packets[index]...)
	}
	transport.mu.Lock()
	transport.datagrams = append(transport.datagrams, copyOfPackets)
	transport.mu.Unlock()
	return nil
}

func (transport *blockingTransport) SendReliable(ctx context.Context, _ core.SessionID, payload []byte) error {
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

func TestAsyncTransportKeepsLatestCompleteFrame(t *testing.T) {
	downstream := newBlockingTransport()
	config := DefaultAsyncTransportConfig()
	config.AllowOpaqueDatagrams = true
	config.SendTimeout = time.Second
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendDatagramBatch(context.Background(), 7, [][]byte{{1, 1}, {1, 2}}); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.datagramStarted)
	if err := transport.SendDatagramBatch(context.Background(), 7, [][]byte{{2, 1}, {2, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendDatagramBatch(context.Background(), 7, [][]byte{{3, 1}, {3, 2}}); err != nil {
		t.Fatal(err)
	}
	downstream.datagramRelease <- struct{}{}
	await(t, downstream.datagramStarted)
	downstream.datagramRelease <- struct{}{}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	downstream.mu.Lock()
	defer downstream.mu.Unlock()
	if len(downstream.datagrams) != 2 || downstream.datagrams[1][0][0] != 3 || len(downstream.datagrams[1]) != 2 {
		t.Fatalf("latest frame was not replaced atomically: %#v", downstream.datagrams)
	}
	stats := transport.Stats()
	if stats.DatagramFramesDropped != 1 || stats.DatagramFramesSent != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAsyncTransportReliableBackpressureAndIndependentLane(t *testing.T) {
	downstream := newBlockingTransport()
	config := DefaultAsyncTransportConfig()
	config.AllowOpaqueDatagrams = true
	config.ReliableQueueSize = 1
	config.SendTimeout = time.Second
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 8}); err != nil {
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
	if err := transport.SendDatagramBatch(context.Background(), 8, [][]byte{{9}}); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.datagramStarted)
	downstream.datagramRelease <- struct{}{}
	downstream.reliableRelease <- struct{}{}
	await(t, downstream.reliableStarted)
	downstream.reliableRelease <- struct{}{}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	stats := transport.Stats()
	if stats.ReliableBackpressure != 1 || stats.ReliableSent != 2 || stats.DatagramFramesSent != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestCoreReplicatorDrivesAsyncSessionLifecycle(t *testing.T) {
	downstream := core.TransportFunc{
		Datagram: func(context.Context, core.SessionID, []byte) error { return nil },
		Reliable: func(context.Context, core.SessionID, []byte) error { return nil },
	}
	transport, err := NewAsyncTransport(downstream, DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	replicator := core.NewReplicator(core.ReplicatorConfig{Transport: transport})
	if err := replicator.RegisterSession(core.SessionInfo{ID: 10}); err != nil {
		t.Fatal(err)
	}
	if transport.Stats().ActiveSessions != 1 {
		t.Fatal("transport session was not registered by core")
	}
	if !replicator.RemoveSession(10) || transport.Stats().ActiveSessions != 0 {
		t.Fatal("transport session was not removed by core")
	}
	replicator.Close()
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncTransportReliableFailureIsTerminalAndHandlerPanicIsContained(t *testing.T) {
	downstream := &orderedFailureTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.AllowOpaqueDatagrams = true
	config.SendTimeout = time.Second
	config.OnError = func(SendError) { panic("metrics sink panic") }
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 31}); err != nil {
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

func (*orderedFailureTransport) SendDatagram(context.Context, core.SessionID, []byte) error {
	return nil
}

func (transport *orderedFailureTransport) SendReliable(context.Context, core.SessionID, []byte) error {
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
	downstream := &stubbornDatagramTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.AllowOpaqueDatagrams = true
	config.SendTimeout = time.Second
	transport, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	info := core.SessionInfo{ID: 41}
	if err := transport.RegisterSession(info); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendDatagram(context.Background(), info.ID, []byte{1}); err != nil {
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

type stubbornDatagramTransport struct {
	started chan struct{}
	release chan struct{}
}

func TestAsyncTransportRejectsIncompleteOrMixedFrameBatch(t *testing.T) {
	downstream := core.TransportFunc{
		Datagram: func(context.Context, core.SessionID, []byte) error { return nil },
		Reliable: func(context.Context, core.SessionID, []byte) error { return nil },
	}
	transport, err := NewAsyncTransport(downstream, DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 51}); err != nil {
		t.Fatal(err)
	}
	limits := core.DefaultLimits()
	frame := core.DeltaFrame{SnapshotMeta: core.SnapshotMeta{RoomID: 1, Epoch: 1, Tick: 1, SchemaVersion: 1}, Kind: core.FrameFull}
	packets, err := core.FragmentFrame(frame, 1, make([]byte, 300), 100, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.SendDatagramBatch(context.Background(), 51, packets[:len(packets)-1]); !errors.Is(err, ErrInvalidDatagramBatch) {
		t.Fatalf("incomplete frame was accepted: %v", err)
	}
	other := frame
	other.Tick = 2
	otherPackets, err := core.FragmentFrame(other, 2, make([]byte, 300), 100, limits)
	if err != nil {
		t.Fatal(err)
	}
	mixed := append([][]byte(nil), packets...)
	mixed[len(mixed)-1] = otherPackets[len(otherPackets)-1]
	if err := transport.SendDatagramBatch(context.Background(), 51, mixed); !errors.Is(err, ErrInvalidDatagramBatch) {
		t.Fatalf("mixed frame was accepted: %v", err)
	}
	if err := transport.SendDatagramBatch(context.Background(), 51, packets); err != nil {
		t.Fatalf("valid frame was rejected: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transport.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestTransportRejectsTypedNilDependencies(t *testing.T) {
	var nilDownstream *blockingTransport
	if _, err := NewAsyncTransport(nilDownstream, DefaultAsyncTransportConfig()); !errors.Is(err, ErrTransportRequired) {
		t.Fatalf("typed nil downstream was accepted: %v", err)
	}
	composite := CompositeTransport{Datagrams: nilDownstream, Reliable: nilDownstream}
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
	downstream := &lifecycleTransport{sessions: make(map[core.SessionID]bool)}
	transport, err := NewAsyncTransport(downstream, DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(core.SessionInfo{ID: 61}); err != nil {
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
	sessions map[core.SessionID]bool
}

func (*lifecycleTransport) SendDatagram(context.Context, core.SessionID, []byte) error { return nil }
func (*lifecycleTransport) SendReliable(context.Context, core.SessionID, []byte) error { return nil }
func (transport *lifecycleTransport) RegisterSession(info core.SessionInfo) error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.sessions[info.ID] {
		return ErrSessionAlreadyExists
	}
	transport.sessions[info.ID] = true
	return nil
}
func (transport *lifecycleTransport) RemoveSession(id core.SessionID) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	exists := transport.sessions[id]
	delete(transport.sessions, id)
	return exists
}

func (transport *stubbornDatagramTransport) SendDatagram(context.Context, core.SessionID, []byte) error {
	close(transport.started)
	<-transport.release
	return nil
}

func (*stubbornDatagramTransport) SendReliable(context.Context, core.SessionID, []byte) error {
	return nil
}

func await(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport")
	}
}
