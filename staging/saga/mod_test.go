package saga

import (
	"context"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

type drainTestSubscription struct {
	drained chan struct{}
	closed  chan struct{}
}

func (s *drainTestSubscription) Stop()                   {}
func (s *drainTestSubscription) Drain()                  { close(s.drained) }
func (s *drainTestSubscription) Closed() <-chan struct{} { return s.closed }

func TestDrainSubscriptionsWaitsForConsumerClosure(t *testing.T) {
	sub := &drainTestSubscription{drained: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- drainSubscriptions(context.Background(), []fnats.IJetStreamSubscription{sub}) }()
	<-sub.drained
	select {
	case err := <-done:
		t.Fatalf("drain returned before closure: %v", err)
	default:
	}
	close(sub.closed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after closure")
	}
}
