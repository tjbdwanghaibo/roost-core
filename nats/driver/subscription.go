package driver

import (
	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// subscription wraps gonats.Subscription to implement fnats.ISubscription.
// client 是创建它的连接：已关闭判据在 Client 上（见 Client 的类型注释）。
type subscription struct {
	sub    *gonats.Subscription
	client *Client
}

func (s *subscription) Unsubscribe() error {
	if err := s.client.admit(); err != nil {
		return err
	}
	return s.client.wrapError(s.sub.Unsubscribe())
}

func (s *subscription) IsValid() bool {
	return !s.client.closed() && s.sub.IsValid()
}

var _ fnats.ISubscription = (*subscription)(nil)
