package nats

import "context"

// RawClient 是非持久接入通道的可选能力；不改变普通 Bus/JetStream 的传输选择。
// RequestContext 和 PublishOnce 均只尝试一次。失败或取消不证明远端没有执行。
type RawClient interface {
	RequestContext(context.Context, string, []byte) ([]byte, error)
	PublishOnce(string, []byte) error
	MaxPayload() (int64, error)
	FlushContext(context.Context) error
	SubscribeBounded(string, PendingLimits, MsgHandler) (DrainSubscription, error)
}

type PendingLimits struct {
	Messages int
	Bytes    int
	// OnError 必须快速返回；慢消费者或订阅失败意味着链路连续性已不能保证。
	OnError func(error)
}

// DrainSubscription 的成功表示本句柄的实际回调已结束。超时可用新 context 重试。
// 它只拥有订阅，不关闭共享连接。
type DrainSubscription interface {
	ISubscription
	DrainContext(context.Context) error
}
