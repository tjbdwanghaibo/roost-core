package entitysync

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

// SessionID names one receiver of frames. What it identifies — a connection,
// a player, a bot — is the policy's business; the manager only needs it to be
// stable for the session's lifetime and unique among open sessions.
type SessionID = nettransport.SessionID

// Transport is the wire end. One call is one complete, self-describing frame
// for one session; the manager never splits a frame across calls and never
// batches sessions together, so a failure is exactly one session's failure.
//
// Return ErrRetryLater (wrapped or bare) when the transport itself is
// unavailable and the whole tick should be tried again; return anything else
// to have the session closed and its subscriptions dropped.
// Push 返回 nil 即表示这一帧已准入，返回错误表示这一帧未准入。
// 后续 Push 的失败不会撤销此前成功的帧；Manager 保留已交付帧的会话时钟，
// 并在部分成功后重试时用全量内容恢复受影响订阅的基线。
type Transport interface {
	Push(ctx context.Context, session SessionID, frame []byte) error
}

// TransportFunc adapts a function to Transport.
type TransportFunc func(ctx context.Context, session SessionID, frame []byte) error

func (f TransportFunc) Push(ctx context.Context, session SessionID, frame []byte) error {
	if f == nil {
		return ErrTransportRequired
	}
	return f(ctx, session, frame)
}

// SessionLifecycle is optional on a Transport: a transport that keeps its own
// per-session state (queues, connections) learns here when the manager opens
// and closes a session.
type SessionLifecycle interface {
	SessionOpened(session SessionID) error
	SessionClosed(session SessionID)
}

// AsyncTransport puts frames on nettransport.AsyncTransport's reliable lane.
// Backpressure there means the receiver is not keeping up; that is the
// session's failure and it is closed — the slow-consumer policy, in one place.
// A closed async transport is nobody's failure and asks for a retry.
type AsyncTransport struct {
	async *nettransport.AsyncTransport
}

func NewAsyncTransport(async *nettransport.AsyncTransport) (*AsyncTransport, error) {
	if async == nil {
		return nil, ErrTransportRequired
	}
	return &AsyncTransport{async: async}, nil
}

func (t *AsyncTransport) Push(ctx context.Context, session SessionID, frame []byte) error {
	if t == nil || t.async == nil {
		return fmt.Errorf("%w: no async transport", ErrRetryLater)
	}
	err := t.async.SendReliable(ctx, session, frame)
	if errors.Is(err, nettransport.ErrTransportClosed) {
		return fmt.Errorf("%w: %w", ErrRetryLater, err)
	}
	return err
}

func (t *AsyncTransport) SessionOpened(session SessionID) error {
	if t == nil || t.async == nil {
		return ErrTransportRequired
	}
	// 旧发送尚未退出时必须明确拒绝；不能把新 lifetime 绑定到旧队列。
	return t.async.RegisterSession(nettransport.SessionInfo{ID: session})
}

func (t *AsyncTransport) SessionClosed(session SessionID) {
	if t == nil || t.async == nil {
		return
	}
	t.async.RemoveSession(session)
}

var _ Transport = (*AsyncTransport)(nil)
var _ SessionLifecycle = (*AsyncTransport)(nil)

// FrameSizeLimiter 声明一次 Push 可接收的完整帧上限；0 表示不另加限制。
// 包装 Transport 时应转发此能力，并扣除包装自身的协议头。
type FrameSizeLimiter interface{ MaxFrameBytes() int }

func (t *AsyncTransport) MaxFrameBytes() int {
	if t == nil || t.async == nil {
		return 0
	}
	return t.async.MaxReliableBytes()
}
