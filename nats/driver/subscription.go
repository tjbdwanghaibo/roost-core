package driver

import (
	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// subscription wraps gonats.Subscription to implement fnats.ISubscription.
type subscription struct {
	sub *gonats.Subscription
}

func (s *subscription) Unsubscribe() error {
	return s.sub.Unsubscribe()
}

func (s *subscription) IsValid() bool {
	return s.sub.IsValid()
}

var _ fnats.ISubscription = (*subscription)(nil)
