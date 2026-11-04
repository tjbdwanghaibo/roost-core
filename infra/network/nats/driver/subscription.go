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

// subscription wraps gonats.Subscription to implement fnats.ISubscription.
// client 是创建它的连接：已关闭判据在 Client 上（见 Client 的类型注释）。
type subscription struct {
	sub    *gonats.Subscription
	client *Client

	// 排空（DrainContext）只发起一次，之后的调用等同一次排空。nativeDrained 在 nativeDrainOnce 里创建。
	nativeDrainOnce sync.Once
	nativeDrained   chan struct{}
	nativeDrainErr  error
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

// DrainContext 排空这个订阅（fnats.DrainSubscription）：退掉服务端的订阅兴趣，服务端处理退订之前已经发出、以及已经
// 在本连接缓冲里的消息照常交给回调；订阅失效后返回 nil。与 Unsubscribe 的区别：Unsubscribe 立刻在本端删掉订阅，
// 之后到达的消息与缓冲里还没交付的消息都被丢掉，请求方只能等到超时（结果未知）。
//
// nats.go 在回调返回之后才扣减待交付数、扣到 0 才删掉订阅，所以订阅失效时不再有本订阅的回调在跑。ctx 先结束返回
// 错误，排空在后台继续，用新 ctx 再调用等同一次排空。只排空本订阅，不关闭共享连接。
func (s *subscription) DrainContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nats: drain context is required")
	}
	s.nativeDrainOnce.Do(func() {
		s.nativeDrained = make(chan struct{})
		err := s.client.admit()
		if err == nil {
			err = s.client.wrapError(s.sub.Drain())
		}
		if err != nil {
			s.nativeDrainErr = err
			close(s.nativeDrained)
			return
		}
		go func() {
			defer close(s.nativeDrained)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for s.sub.IsValid() && !s.client.closed() {
				<-ticker.C
			}
		}()
	})
	// 已经有结论时不论 ctx 是否结束都返回结论（与 operation.Lifetime.Wait 同一规则）。
	select {
	case <-s.nativeDrained:
		return s.nativeDrainErr
	default:
	}
	select {
	case <-s.nativeDrained:
		return s.nativeDrainErr
	case <-ctx.Done():
		return fmt.Errorf("nats: subscription still draining: %w", ctx.Err())
	}
}

var (
	_ fnats.ISubscription     = (*subscription)(nil)
	_ fnats.DrainSubscription = (*subscription)(nil)
)
