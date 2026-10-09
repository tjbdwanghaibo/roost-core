// Package gate 将 App、鉴权、发现和同步能力接到 gateway 接入运行实现。
package gate

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// AsyncQueueFactory 只转换接口与 ID；队列、驻留预算、年龄和错误寿命由既有
// AsyncTransport 唯一维护。Gateway 不依赖 Framework，Wiring 不复制发送循环。
func AsyncQueueFactory(limits gateway.OutboundLimits, send func(context.Context, uint64, []byte) error, onError func(uint64, error)) (gateway.ReliableQueue, error) {
	transport, err := nettransport.NewAsyncTransport(nettransport.TransportFunc{Reliable: func(ctx context.Context, id nettransport.SessionID, payload []byte) error {
		return send(ctx, uint64(id), payload)
	}}, nettransport.AsyncTransportConfig{
		MaxSessions: limits.MaxSessions, ReliableQueueSize: limits.QueueEntries, MaxReliableBytes: limits.MaxPacketBytes,
		MaxQueuedReliableBytes: limits.PerSessionBytes, MaxResidentReliableBytes: limits.ResidentBytes,
		MaxReliableAge: limits.MaxAge, SendTimeout: limits.SendTimeout,
		OnError: func(failure nettransport.SendError) {
			if onError != nil {
				onError(uint64(failure.Session), failure.Err)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return asyncQueue{transport: transport}, nil
}

type asyncQueue struct{ transport *nettransport.AsyncTransport }

func (queue asyncQueue) Register(id uint64, ownerID int64) error {
	return queue.transport.RegisterSession(nettransport.SessionInfo{ID: nettransport.SessionID(id), OwnerID: ownerID})
}
func (queue asyncQueue) Enqueue(ctx context.Context, id uint64, payload []byte) error {
	return queue.transport.SendReliable(ctx, nettransport.SessionID(id), payload)
}
func (queue asyncQueue) Remove(id uint64) bool {
	return queue.transport.RemoveSession(nettransport.SessionID(id))
}
func (queue asyncQueue) Drain(ctx context.Context) error { return queue.transport.Close(ctx) }

var _ gateway.ReliableQueue = asyncQueue{}
