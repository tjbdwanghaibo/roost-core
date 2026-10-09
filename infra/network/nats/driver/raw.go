package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

func (c *Client) RequestContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	return c.requestWithContext(ctx, subject, data)
}

func (c *Client) PublishOnce(subject string, data []byte) error {
	if err := c.validateSubject(subject); err != nil {
		return err
	}
	return c.wrapError(c.conn.Publish(subject, data))
}

func (c *Client) MaxPayload() (int64, error) {
	if err := c.admit(); err != nil {
		return 0, err
	}
	limit := c.conn.MaxPayload()
	if c.closed() {
		return 0, fnats.ErrClosed
	}
	return limit, nil
}

func (c *Client) FlushContext(ctx context.Context) error {
	if err := c.admit(); err != nil {
		return err
	}
	if ctx == nil {
		return errors.New("nats: flush context is required")
	}
	return c.wrapError(c.conn.FlushWithContext(ctx))
}

// rawSubscription 拥有该订阅的回调寿命。排空时继续交付已接纳消息；
// native 订阅失效且最后一个回调返回之后才宣告完成，不提前释放共享依赖。
type rawSubscription struct {
	*subscription
	onError   func(error)
	mu        sync.Mutex
	closing   bool
	wait      sync.WaitGroup
	drainOnce sync.Once
	drainDone chan struct{}
	drainErr  error
}

func (c *Client) SubscribeBounded(subject string, limits fnats.PendingLimits, handler fnats.MsgHandler) (fnats.DrainSubscription, error) {
	if err := c.validateSubscription(subject, "", handler); err != nil {
		return nil, err
	}
	if limits.Messages <= 0 || limits.Bytes <= 0 {
		return nil, errors.New("nats: pending messages and bytes must be positive")
	}
	owned := &rawSubscription{onError: limits.OnError, drainDone: make(chan struct{})}
	// 注册与异步错误路由使用同一把锁，避免 native 订阅已公布而错误回调尚无拥有者。
	c.state.rawMu.Lock()
	defer c.state.rawMu.Unlock()
	sub, err := c.conn.Subscribe(subject, func(msg *gonats.Msg) {
		owned.mu.Lock()
		if owned.closing {
			owned.mu.Unlock()
			return
		}
		owned.wait.Add(1)
		owned.mu.Unlock()
		defer owned.wait.Done()
		invokeNatsHandler(handler, &fnats.Msg{Subject: msg.Subject, Reply: msg.Reply, Data: append([]byte(nil), msg.Data...)})
	})
	if err != nil {
		return nil, c.wrapError(err)
	}
	if err := sub.SetPendingLimits(limits.Messages, limits.Bytes); err != nil {
		_ = sub.Unsubscribe()
		return nil, c.wrapError(err)
	}
	owned.subscription = &subscription{sub: sub, client: c}
	if c.state.raw == nil {
		c.state.raw = make(map[*gonats.Subscription]*rawSubscription)
	}
	c.state.raw[sub] = owned
	return owned, nil
}

func (s *rawSubscription) forget() {
	s.client.state.rawMu.Lock()
	delete(s.client.state.raw, s.sub)
	s.client.state.rawMu.Unlock()
}

func (s *rawSubscription) Unsubscribe() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	err := s.subscription.Unsubscribe()
	s.forget()
	return err
}

func (s *rawSubscription) DrainContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nats: drain context is required")
	}
	s.drainOnce.Do(func() {
		err := s.sub.Drain()
		go func() {
			defer close(s.drainDone)
			if err != nil {
				s.drainErr = s.client.wrapError(err)
			}
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for s.sub.IsValid() && !s.client.closed() {
				<-ticker.C
			}
			s.mu.Lock()
			s.closing = true
			s.mu.Unlock()
			s.wait.Wait()
			s.forget()
		}()
	})
	select {
	case <-s.drainDone:
		return s.drainErr
	case <-ctx.Done():
		return fmt.Errorf("nats: subscription still draining: %w", ctx.Err())
	}
}

func (state *natsLifecycleState) rawError(sub *gonats.Subscription, err error) {
	state.rawMu.Lock()
	owned := state.raw[sub]
	state.rawMu.Unlock()
	if owned != nil && owned.onError != nil {
		// 观察回调与普通消息回调使用相同 panic 隔离边界。
		invokeNatsHandler(func(*fnats.Msg) { owned.onError(err) }, nil)
	}
}

var _ fnats.RawClient = (*Client)(nil)
var _ fnats.DrainSubscription = (*rawSubscription)(nil)
