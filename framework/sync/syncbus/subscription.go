package syncbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

// ErrUnsubscribed 是 Subscription.Deliver 在退订开始之后的返回值：这条消息没有交给 handler。
// 传输据此静默跳过（等同于退订先一步生效），不按 handler 错误记日志。
var ErrUnsubscribed = errors.New("syncbus: subscription is unsubscribed")

// Subscription 是一次 Subscribe / SubscribeLive 的句柄（A3 ②，docs/feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md）。
//
// 排空在这里实现一次，所有传输共用：传输用 NewSubscription 建句柄、把每条消息交给 Deliver，
// 并提供 release（从本地分发表删除 / 停消费者 / 发 UNSUB，不等待）。订阅者不再各自维护
// “准入 + 在途计数 + 等待”：Unsubscribe(ctx) 返回 nil 就是这个订阅已经静止。
type Subscription struct {
	topic   string
	handler Handler
	release func()
	once    sync.Once
	cleared sync.Once
	// calls 是这次订阅的回调准入与在途计数（共用的 operation.Lifetime，A3 ①）。
	calls operation.Lifetime
}

// NewSubscription 给传输实现用：handler 是订阅者的回调，release 在第一次 Unsubscribe 时调用一次，
// 负责撤掉传输侧的登记，不能等待在途回调（等待由 Unsubscribe 完成）。release 可为 nil。
func NewSubscription(topic string, handler Handler, release func()) *Subscription {
	return &Subscription{topic: topic, handler: handler, release: release}
}

// Topic 返回订阅的主题。
func (s *Subscription) Topic() string {
	if s == nil {
		return ""
	}
	return s.topic
}

// deliveryKey 是投递 ctx 里“正在执行 sub 的回调”标记的键；每个订阅一个键，嵌套投递互不遮挡。
type deliveryKey struct{ sub *Subscription }

// deliveryMark 是一次已准入的回调调用。ended 保证这次准入恰好归还一次：通常由 Deliver 在 handler
// 返回时归还，handler 里退订自己时由 Unsubscribe 提前归还。outer 是同一订阅在同一调用链上更外层的
// 调用（handler 同步发布又同步投递回自己时）。
type deliveryMark struct {
	outer *deliveryMark
	ended atomic.Bool
}

func (s *Subscription) end(mark *deliveryMark) {
	if mark.ended.CompareAndSwap(false, true) {
		s.calls.End()
	}
}

// Deliver 把一条消息交给 handler，在 handler 返回之后才返回。退订开始之后不调用 handler，返回
// ErrUnsubscribed。ctx 是传输给出的投递 ctx（nil 按 Background），交给 handler 时加上本订阅的标记。
// handler panic 时照样归还准入，panic 继续向上传给传输。
func (s *Subscription) Deliver(ctx context.Context, msg *SyncMsg) error {
	_, err := s.TryDeliver(ctx, msg)
	return err
}

// TryDeliver 供传输区分“退订拒绝准入”和“handler 已执行但返回错误”。
// true 不表示业务成功；handler 自己返回 ErrUnsubscribed 仍为 true。
// panic 与 Deliver 一样归还准入后向上传播，由传输保持既有隔离策略。
func (s *Subscription) TryDeliver(ctx context.Context, msg *SyncMsg) (bool, error) {
	if s == nil || !s.calls.Begin() {
		return false, ErrUnsubscribed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := deliveryKey{s}
	mark := &deliveryMark{}
	mark.outer, _ = ctx.Value(key).(*deliveryMark)
	defer s.end(mark)
	return true, s.handler(context.WithValue(ctx, key, mark), msg)
}

// Unsubscribe 按三步停机退订（roost-coding；契约骨架 internal/stopcontract）：
//
//  1. 发起（幂等）：关闭投递准入，之后到达的消息不再调用 handler；release 只调用一次。
//  2. 在 ctx 内等这次订阅已准入的回调全部返回。ctx 先结束返回 ctx 错误，订阅保持“退订中”，
//     用新 ctx 重试继续等同一批。
//  3. 返回 nil 之后没有在途回调、也不会再有新回调，订阅者可以释放 handler 用到的依赖；之后再调用
//     （任何 ctx）都返回 nil。排空后丢掉 handler 引用，订阅者闭包里的状态可被回收。
//
// handler 里退订自己：传入 handler 收到的投递 ctx（或其派生），本次调用（及同一调用链上同一订阅更外层
// 的调用）先归还准入，只等其他在途回调；返回 nil 时调用者自己在 handler 返回时结束。传入无关的 ctx
// 会等自己：带期限的到期返回 ctx 错误，Background 会一直等。
//
// 不配合的 handler 不会被终止；退订不取消在途回调的投递 ctx。nil Subscription 返回 nil。
func (s *Subscription) Unsubscribe(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.calls.Stop()
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
	for mark, _ := ctx.Value(deliveryKey{s}).(*deliveryMark); mark != nil; mark = mark.outer {
		s.end(mark)
	}
	if err := s.calls.Wait(ctx); err != nil {
		return err
	}
	// 排空之后不会再有 Begin 成功，读 handler 的调用都已 End（Lifetime 的锁与通道给出先后关系）。
	s.cleared.Do(func() { s.handler = nil })
	return nil
}
