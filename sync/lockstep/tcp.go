package lockstep

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

// TCPSender将Room两条发送通道接到生成Runtime.PushLockstepSession。
// TCP有队头阻塞；同步发送，不使用会合并/丢帧的异步Sync队列。
// Resolve必须并发安全：可靠追帧页由Room后台发送；会话ID不得跨连接生命周期复用。
// 比赛Tick驱动须在快worker/Entity锁之外；快worker发送会在副作用前明确拒绝。
type TCPSender struct {
	push       func(context.Context, string, uint32, []byte) error
	resolve    func(nettransport.SessionID) (string, bool)
	messageID  uint32
	maxPayload int
}

func NewTCPSender(messageID uint32, maxPayload int,
	resolve func(nettransport.SessionID) (string, bool),
	push func(context.Context, string, uint32, []byte) error) (*TCPSender, error) {
	if messageID == 0 || resolve == nil || push == nil || maxPayload < 0 {
		return nil, fmt.Errorf("%w: TCP sender requires route, resolver, push and nonnegative limit", ErrRoomConfigInvalid)
	}
	if maxPayload == 0 {
		maxPayload = wire.DefaultMaxPayload
	}
	return &TCPSender{push: push, resolve: resolve, messageID: messageID, maxPayload: maxPayload}, nil
}

func (s *TCPSender) MaxDatagramPayload() int { return s.maxPayload }
func (s *TCPSender) SendDatagram(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return s.send(ctx, id, payload)
}
func (s *TCPSender) SendReliable(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return s.send(ctx, id, payload)
}
func (s *TCPSender) send(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	if err := fctx.BlockingError("lockstep.TCPSender.send"); err != nil {
		return err
	}
	if len(payload) > s.maxPayload {
		return wire.ErrPacketTooBig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session, ok := s.resolve(id)
	if !ok || session == "" {
		return ErrPlayerDetached
	}
	return s.push(ctx, session, s.messageID, payload)
}

var _ nettransport.DatagramSender = (*TCPSender)(nil)
var _ nettransport.ReliableSender = (*TCPSender)(nil)
var _ nettransport.DatagramPayloadLimiter = (*TCPSender)(nil)
