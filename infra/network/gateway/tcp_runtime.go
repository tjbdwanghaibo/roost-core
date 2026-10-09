// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type TCPEncoder func(uint32, any) ([]byte, error)

// NewTCPRuntime 构造唯一的推送与连接生命周期运行实例，encoder 只负责业务 PB。
func NewTCPRuntime(encode TCPEncoder, loginTimeout time.Duration) *TCPRuntime {
	return &TCPRuntime{encode: encode, loginTimeout: loginTimeout}
}

// TCPRuntime is the application-facing active publish boundary. It encodes one
// typed protocol value once, then sends it to one session or every currently
// authenticated session of a player. It never exposes socket or Entity state.
type TCPRuntime struct {
	owner        atomic.Pointer[TCPServer]
	encode       TCPEncoder
	server       atomic.Pointer[TCPServer]
	loginTimeout time.Duration

	lifecycleMu    sync.Mutex
	subscribers    map[uint64]func(TCPSessionClosed)
	nextSubscriber uint64
	closedEvents   chan TCPSessionClosed
	lifecycleOnce  sync.Once
	lifecycleStop  chan struct{}
	lifecycleDone  chan struct{}
}

// TCPSessionClosed says a connection is gone. It carries identity only: a
// subscriber that needs more looks it up, and a lifecycle event that carried
// state would be a second copy of it.
type TCPSessionClosed struct {
	// 独立 Gate 携带完整旧绑定和数值 lifetime，退订不能只按可复用 SessionID。
	Binding    Binding
	ReceiverID uint64
	PlayerID   int64
	SessionID  string
}

// sessionClosedQueue is how many closes may be in flight before the oldest are
// dropped. Bounded on purpose: a subscriber that blocks must not be able to
// hold the socket cleanup path, and a mass disconnect must not be able to grow
// memory without limit. A dropped event is counted, and every consumer of this
// source has to be able to survive missing one — "who is online" is state that
// is reconciled, not a log that is replayed.
const sessionClosedQueue = 256

// OnSessionClosed subscribes to connection closes and returns the
// unsubscribe. It exists because everything that maintains an "online" set —
// the replicated scene, chat presence — otherwise has to notice by failing to
// push to somebody, which never happens if nothing tries (RR-20260918-06).
//
// Subscribers run on this source's own goroutine, not on the socket's: a slow
// one delays other subscribers and nothing else. Do not block in one.
func (runtime *TCPRuntime) OnSessionClosed(fn func(TCPSessionClosed)) func() {
	if runtime == nil || fn == nil {
		return func() {}
	}
	runtime.startLifecycle()
	runtime.lifecycleMu.Lock()
	runtime.nextSubscriber++
	id := runtime.nextSubscriber
	if runtime.subscribers == nil {
		runtime.subscribers = make(map[uint64]func(TCPSessionClosed))
	}
	runtime.subscribers[id] = fn
	runtime.lifecycleMu.Unlock()
	return func() {
		runtime.lifecycleMu.Lock()
		delete(runtime.subscribers, id)
		runtime.lifecycleMu.Unlock()
	}
}

func (runtime *TCPRuntime) startLifecycle() {
	runtime.lifecycleOnce.Do(func() {
		runtime.closedEvents = make(chan TCPSessionClosed, sessionClosedQueue)
		runtime.lifecycleStop = make(chan struct{})
		runtime.lifecycleDone = make(chan struct{})
		go runtime.dispatchClosed()
	})
}

func (runtime *TCPRuntime) dispatchClosed() {
	defer close(runtime.lifecycleDone)
	for {
		select {
		case <-runtime.lifecycleStop:
			// Drain what is already queued: a close that happened before
			// shutdown is still a close, and a subscriber that cleans up
			// external state wants it.
			for {
				select {
				case event := <-runtime.closedEvents:
					runtime.deliverClosed(event)
				default:
					return
				}
			}
		case event := <-runtime.closedEvents:
			runtime.deliverClosed(event)
		}
	}
}

func (runtime *TCPRuntime) deliverClosed(event TCPSessionClosed) {
	type subscriber struct {
		id uint64
		fn func(TCPSessionClosed)
	}
	runtime.lifecycleMu.Lock()
	subscribers := make([]subscriber, 0, len(runtime.subscribers))
	for id, fn := range runtime.subscribers {
		subscribers = append(subscribers, subscriber{id: id, fn: fn})
	}
	runtime.lifecycleMu.Unlock()
	for _, current := range subscribers {
		runtime.callClosedSubscriber(current.id, current.fn, event)
	}
}

// callClosedSubscriber isolates one subscriber. This dispatcher owns a
// goroutine of its own, so a panic nobody recovers here does not fail one
// subscriber — it ends the PROCESS, turning a nil map in a presence bridge
// into an outage for every connected player (RR-20260919-03). Every other
// callback boundary in this framework recovers per callback; so does this one,
// and the subscribers after the failing one still run, because the state they
// maintain — who is online — is exactly what has to converge.
func (runtime *TCPRuntime) callClosedSubscriber(id uint64, fn func(TCPSessionClosed), event TCPSessionClosed) {
	defer func() {
		if recovered := recover(); recovered != nil {
			metrics.IncCounter("player_tcp_session_closed_callback_panics_total", nil, 1)
			slog.Error("player tcp: session-close subscriber panicked",
				"subscriber", id, "player_id", event.PlayerID, "session_id", event.SessionID,
				"panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	fn(event)
}

// publishClosed is called from the socket cleanup path, so it never blocks:
// a full queue drops the event and counts it rather than holding a connection
// teardown behind a subscriber.
func (runtime *TCPRuntime) publishClosed(event TCPSessionClosed) {
	if runtime == nil || runtime.closedEvents == nil {
		return
	}
	select {
	case runtime.closedEvents <- event:
	default:
		metrics.IncCounter("player_tcp_session_closed_dropped_total", nil, 1)
	}
}

func (runtime *TCPRuntime) stopLifecycle() { _ = runtime.stopLifecycleContext(context.Background()) }

// stopLifecycleContext ends the session-close dispatcher after it delivers
// what is queued, waiting at most until ctx ends: a subscriber that blocks
// must not hold shutdown past its budget. Calling it again waits again.
func (runtime *TCPRuntime) stopLifecycleContext(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	runtime.lifecycleMu.Lock()
	stop, done := runtime.lifecycleStop, runtime.lifecycleDone
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	runtime.lifecycleMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("player tcp: session-close subscribers still running: %w", ctx.Err())
	}
}

func (runtime *TCPRuntime) PushPlayer(ctx context.Context, playerID int64, messageID uint32, value any) error {
	if runtime == nil || runtime.encode == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	payload, err := runtime.encode(messageID, value)
	if err != nil {
		return err
	}
	return server.pushPlayer(ctx, playerID, messageID, payload)
}

// PushSyncPlayer 发送冻结的原始Sync帧，复用PB推送的发送与关闭责任。
func (runtime *TCPRuntime) PushSyncPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error {
	if runtime == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	return server.pushPlayerKind(ctx, playerID, messageID, payload, flagSync)
}

func (runtime *TCPRuntime) PushSyncSession(ctx context.Context, sessionID string, messageID uint32, payload []byte) error {
	if runtime == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	return server.pushSessionKind(ctx, sessionID, messageID, payload, flagSync)
}

// PushLockstepSession发送既有C7广播或追帧页，不能与PB encoder混用。
func (runtime *TCPRuntime) PushLockstepSession(ctx context.Context, sessionID string, messageID uint32, payload []byte) error {
	if runtime == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	return server.pushSessionKind(ctx, sessionID, messageID, payload, flagLockstep)
}

func (runtime *TCPRuntime) PushLockstepPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error {
	if runtime == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	return server.pushPlayerKind(ctx, playerID, messageID, payload, flagLockstep)
}

func (runtime *TCPRuntime) PushSession(ctx context.Context, sessionID string, messageID uint32, value any) error {
	if runtime == nil || runtime.encode == nil {
		return ErrTransportUnavailable
	}
	server := runtime.server.Load()
	if server == nil {
		return ErrTransportUnavailable
	}
	payload, err := runtime.encode(messageID, value)
	if err != nil {
		return err
	}
	return server.pushSession(ctx, sessionID, messageID, payload)
}

// LoginTimeout is player_access.tcp.login_timeout: how much of a login
// request's dispatch budget the login endpoint gives to taking the player into
// service. Zero means no separate budget (the dispatch deadline still holds).
func (runtime *TCPRuntime) LoginTimeout() time.Duration {
	if runtime == nil {
		return 0
	}
	return runtime.loginTimeout
}

func (runtime *TCPRuntime) ActiveSessions(playerID int64) int {
	if runtime == nil {
		return 0
	}
	server := runtime.server.Load()
	if server == nil {
		return 0
	}
	return server.activeSessions(playerID)
}

// CloseSessions disconnects every tcpSession of one player and reports how many
// it closed. It is the transport half of a fail-closed decision: the process
// has concluded it may no longer serve this player, and the honest way to act
// on that is to stop reading their socket rather than to keep accepting work
// it is not entitled to do.
//
// The closes go through the same path a network error takes, so the usual
// lifecycle event reaches every subscriber and the scene, chat presence and
// ownership cleanups run exactly as they would for a disconnect.
func (runtime *TCPRuntime) CloseSessions(playerID int64, reason error) int {
	if runtime == nil {
		return 0
	}
	server := runtime.server.Load()
	if server == nil {
		return 0
	}
	return server.closeSessions(playerID, reason)
}

// StopNotifications 供未启动监听的接线排空关闭订阅；已启动时必须先等 Server.Stop 完成。
func (runtime *TCPRuntime) StopNotifications(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return runtime.stopLifecycleContext(ctx)
}
