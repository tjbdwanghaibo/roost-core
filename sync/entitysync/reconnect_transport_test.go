package entitysync

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
	"testing"
)

type drainingSender struct{ entered, release chan struct{} }

func (s *drainingSender) SendReliable(context.Context, nettransport.SessionID, []byte) error {
	close(s.entered)
	<-s.release
	return nil
}
func TestReopenRefusesDrainingTransportLifetime(t *testing.T) {
	sender := &drainingSender{make(chan struct{}), make(chan struct{})}
	transport, err := nettransport.NewAsyncTransport(sender, nettransport.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	adapter, _ := NewAsyncTransport(transport)
	manager, err := NewManager(ManagerConfig{Transport: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = adapter.Push(context.Background(), 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	<-sender.entered
	defer close(sender.release)
	manager.CloseSession(1)
	if err = manager.OpenSession(1); !errors.Is(err, nettransport.ErrSessionAlreadyExists) {
		t.Fatalf("open=%v; must refuse old queue", err)
	}
	if manager.Stats().Sessions != 0 {
		t.Fatal("failed open created a fake session")
	}
}
