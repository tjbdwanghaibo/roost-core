package cellworld

import (
	"errors"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// Transport 是 Cell 与 Base 之间的消息通道：至多一次、可能丢失 / 重复 / 乱序（协议靠版本、重传与去重容忍）。
//
// Subscribe 的 handler 在传输自己的 goroutine 上调用，必须快速返回、不能阻塞；data 只在调用期间有效。
// Node 按来源订阅多个主题（SubjectFromCell / SubjectFromBase），依赖的投递模型是：同一个订阅的回调串行、按到达顺序
// 调用；不同订阅的回调可以并发（NATS 每个订阅一个投递 goroutine 与独立的待处理队列）。前者让同一来源 Cell 的增量
// 按序解码，后者让 Base 的解码随来源 Cell 数并行、一个慢来源不挡别的来源。正确性不依赖前者（协议容忍乱序，乱序只会
// 变成缺口修复），Node 的共享状态也不依赖它（见 receivedReplicas）。
// 返回的 unsubscribe 之后可能还有在途回调结束前的最后几次调用：Node 自己有准入门（CloseIngress 先关门、等在途回调
// 返回再退订），所以传输不必保证退订时回调已排空。
type Transport interface {
	Publish(subject string, data []byte) error
	Subscribe(subject string, handler func(data []byte)) (unsubscribe func() error, err error)
}

// NATSTransport 把 NATS core pub/sub（`infra/network/nats.IClient`）用作 Transport。连接的创建与关闭归调用方；
// 不用 JetStream：空间代理非持久，延迟优先，丢失由协议处理。
func NATSTransport(client fnats.IClient) Transport { return natsTransport{client: client} }

type natsTransport struct{ client fnats.IClient }

func (t natsTransport) Publish(subject string, data []byte) error {
	return t.client.Publish(subject, data)
}

func (t natsTransport) Subscribe(subject string, handler func([]byte)) (func() error, error) {
	if handler == nil {
		return nil, errors.New("cellworld: nil handler")
	}
	sub, err := t.client.Subscribe(subject, func(msg *fnats.Msg) {
		if msg == nil || msg.Status != "" {
			return
		}
		handler(msg.Data)
	})
	if err != nil {
		return nil, err
	}
	return sub.Unsubscribe, nil
}
