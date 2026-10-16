package lockstep

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// TCPSender将Match两条发送通道接到生成Runtime.PushLockstepSession。
// TCP有队头阻塞；push 进入该连接的有界有序出站队列，连接存活期间不合并、不丢帧（连接关闭时丢弃队列剩余帧），
// 返回 nil 只表示已准入；停读连接队满即被关闭，不会拖住同一 Tick 里其他接收者的发送。
// 准入不等 socket，追帧页因此不再随慢链路放慢，CatchupSendTimeout / CatchupMaxFailures 对本发送器基本不生效，
// 见 docs/maintenance/KNOWN-LIMITS.md（lockstep TCP 追帧失去背压限速，尚未确认）。
// Resolve必须并发安全：可靠追帧页由Match后台发送；会话ID不得跨连接生命周期复用。
// 比赛Tick驱动须在业务 worker/Entity锁之外；业务 worker发送会在副作用前明确拒绝。
type TCPSender struct {
	push       func(context.Context, string, uint32, []byte) error
	resolve    func(nettransport.SessionID) (string, bool)
	messageID  uint32
	maxPayload int
}

// NewTCPSender 创建 TCP 发送器；maxPayload 为 0 时取 wire.DefaultMaxPayload。
func NewTCPSender(messageID uint32, maxPayload int,
	resolve func(nettransport.SessionID) (string, bool),
	push func(context.Context, string, uint32, []byte) error) (*TCPSender, error) {
	if messageID == 0 || resolve == nil || push == nil || maxPayload < 0 {
		return nil, fmt.Errorf("%w: TCP sender requires route, resolver, push and nonnegative limit", ErrMatchConfigInvalid)
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
