package lockstep

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// TCPSender 把 Match 的两条发送通道接到嵌入 TCP 接入（生成 Runtime 的 TCPRuntime）。TCP 有队头阻塞；两条通道
// 走同一条连接的有界有序出站队列，已入队的帧在连接存活期间不合并、不丢（连接关闭时丢弃队列剩余帧），区别只在等不等写出。
// 例外是嵌入 TCP 的全进程出站驻留预算耗尽（gateway.ErrResidentBudgetExhausted）：这一帧当场被拒、不入队，连接不关闭；
// 实时帧因此对这个接收者丢失，按数据报丢失处理（下一个广播包的冗余帧或客户端追帧补上），追帧页计一次追帧失败。
//   - 实时通道（SendDatagram，Tick 给每个接收者的当前帧）用 push 只做准入，返回 nil 只表示已入队。
//     Tick 逐接收者串行发送，等写出会让一个停读连接拖住同一 Tick 的所有人；停读连接队满即被关闭。
//   - 可靠通道（SendReliable，追帧页与追帧失败通知）用 pushWritten 入队并等写出。追帧页由 Match 在后台协程里
//     每会话一页在途地发送，等写出让追帧随链路速度放慢，该连接里属于追帧的积压最多是在途那一页
//     （另加至多一页等待超时、但已被写协程取走仍在写的页）；CatchupSendTimeout 经 ctx 限住一页的等待，
//     超时（还在排队的页撤回、不再写出）与写出失败计入 CatchupMaxFailures。
//
// 两个函数都不能为 nil，生产接线是 TCPRuntime.PushLockstepSession 与 TCPRuntime.PushLockstepSessionWritten。
// Resolve 必须并发安全：可靠追帧页由 Match 后台发送；会话 ID 不得跨连接生命周期复用。
// 比赛 Tick 驱动须在业务 worker / Entity 锁之外；业务 worker 上两条通道都在副作用前明确拒绝（fctx.BlockingError）。
type TCPSender struct {
	push        func(context.Context, string, uint32, []byte) error
	pushWritten func(context.Context, string, uint32, []byte) error
	resolve     func(nettransport.SessionID) (string, bool)
	messageID   uint32
	maxPayload  int
}

// NewTCPSender 创建 TCP 发送器；maxPayload 为 0 时取 wire.DefaultMaxPayload。
// push 只准入（实时帧），pushWritten 入队并等写出（追帧页），见 TCPSender。
func NewTCPSender(messageID uint32, maxPayload int,
	resolve func(nettransport.SessionID) (string, bool),
	push, pushWritten func(context.Context, string, uint32, []byte) error) (*TCPSender, error) {
	if messageID == 0 || resolve == nil || push == nil || pushWritten == nil || maxPayload < 0 {
		return nil, fmt.Errorf("%w: TCP sender requires route, resolver, both pushes and nonnegative limit", ErrMatchConfigInvalid)
	}
	if maxPayload == 0 {
		maxPayload = wire.DefaultMaxPayload
	}
	return &TCPSender{push: push, pushWritten: pushWritten, resolve: resolve, messageID: messageID, maxPayload: maxPayload}, nil
}

func (s *TCPSender) MaxDatagramPayload() int { return s.maxPayload }

// SendDatagram 发实时帧：只入队，不等写出。
func (s *TCPSender) SendDatagram(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return s.send(ctx, id, payload, s.push)
}

// SendReliable 发追帧页：入队并等写出，等待受 ctx（Match 给的 CatchupSendTimeout）约束。
func (s *TCPSender) SendReliable(ctx context.Context, id nettransport.SessionID, payload []byte) error {
	return s.send(ctx, id, payload, s.pushWritten)
}

func (s *TCPSender) send(ctx context.Context, id nettransport.SessionID, payload []byte, push func(context.Context, string, uint32, []byte) error) error {
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
	return push(ctx, session, s.messageID, payload)
}

var _ nettransport.DatagramSender = (*TCPSender)(nil)
var _ nettransport.ReliableSender = (*TCPSender)(nil)
var _ nettransport.DatagramPayloadLimiter = (*TCPSender)(nil)
