package physicsroom

import (
	"context"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// Pusher 是 Relay 用到的 Game 出站能力；*gateway.GameIngress 实现它。
type Pusher interface {
	PushBound(ctx context.Context, binding gateway.Binding, messageID uint32, kind wire.PayloadKind, payload []byte) error
}

// Relay 在 Game 侧运行，Binding 必须来自已认证连接且与 Join 相同。
// 每成员只能运行一个 Relay。PushBound 成功表示可靠出站准入，不是客户端收包确认。
// 任何出站失败立即结束本轮订阅；重连必须 Leave/Join，不能沿用旧 Cursor。
func Relay(ctx context.Context, rooms domain.Rooms, game Pusher, binding gateway.Binding, member domain.Member, messageID uint32) error {
	if rooms == nil || game == nil || messageID == 0 || binding.Validate() != nil || member.PlayerID != binding.PlayerID {
		return domain.ErrRequestInvalid
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	defer func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = rooms.Leave(leaveCtx, member)
	}()
	var ack uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		frame, err := rooms.Poll(ctx, member, ack)
		// 从 Poll 完成时安排下一轮，避免 RPC 抖动把两次服务端采样挤到 50ms 内。
		timer.Reset(50 * time.Millisecond)
		if err != nil {
			return err
		}
		if frame.Room != member.Room {
			return domain.ErrMissing
		}
		if len(frame.Payload) == 0 {
			continue
		}
		if frame.Sequence != ack+1 {
			return domain.ErrSequence
		}
		sendCtx, cancel := context.WithTimeout(ctx, time.Second)
		err = game.PushBound(sendCtx, binding, messageID, wire.PayloadSync, frame.Payload)
		cancel()
		if err != nil {
			return err
		}
		ack = frame.Sequence
		if ack == 1 || ack%20 == 0 {
			slog.Debug("physics room chunk queued to gate", "player", binding.PlayerID, "sequence", ack, "bytes", len(frame.Payload))
		}
	}
}
