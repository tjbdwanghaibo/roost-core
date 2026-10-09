package gate

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// Transport 将 Sync 和 Lockstep 的数值 receiver 接到同一个 Game 出站。
// 每次发送冻结完整绑定，SessionID 即使被重用也不会把旧 lifetime 的数据送给新连接。
// Room 的 Attach/Detach/Input/Tick 仍由业务的单一 Room owner 调度，此适配器不运行 Room。
type Transport struct {
	Game                             *gateway.GameIngress
	SyncMessageID, LockstepMessageID uint32
	MaxPayloadBytes                  int
}

func (transport Transport) send(ctx context.Context, id nettransport.SessionID, messageID uint32, kind wire.PayloadKind, payload []byte) error {
	if transport.Game == nil || messageID == 0 {
		return gateway.ErrTransportUnavailable
	}
	binding, ok := transport.Game.ReceiverBinding(uint64(id))
	if !ok {
		return gateway.ErrBindingStale
	}
	return transport.Game.PushBound(ctx, binding, messageID, kind, payload)
}
func (transport Transport) Push(ctx context.Context, id entitysync.SessionID, payload []byte) error {
	return transport.send(ctx, id, transport.SyncMessageID, wire.PayloadSync, payload)
}
func (transport Transport) SessionOpened(id entitysync.SessionID) error {
	if transport.Game == nil {
		return gateway.ErrTransportUnavailable
	}
	if _, ok := transport.Game.ReceiverBinding(uint64(id)); !ok {
		return gateway.ErrBindingStale
	}
	return nil
}

// SessionClosed 只结束 Sync 订阅；PB/Room 仍可能使用连接，不拆除 Game 统一发送队列。
func (transport Transport) SessionClosed(entitysync.SessionID) {}
func (transport Transport) MaxFrameBytes() int {
	if transport.Game == nil {
		return 0
	}
	limit := transport.Game.MaxPayloadBytes()
	if transport.MaxPayloadBytes > 0 {
		return min(limit, transport.MaxPayloadBytes)
	}
	return limit
}
func (transport Transport) SendReliable(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return transport.send(ctx, id, transport.LockstepMessageID, wire.PayloadLockstep, payload)
}
func (transport Transport) SendDatagram(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return transport.SendReliable(ctx, id, payload)
}
func (transport Transport) MaxDatagramPayload() int { return transport.MaxFrameBytes() }

var _ entitysync.Transport = Transport{}
var _ entitysync.SessionLifecycle = Transport{}
var _ entitysync.FrameSizeLimiter = Transport{}
var _ nettransport.ReliableSender = Transport{}
var _ nettransport.DatagramSender = Transport{}
