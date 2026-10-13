// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"golang.org/x/time/rate"
)

// TCPDispatch 由本地 ProtocolRegistry 或 Gate forwarder 注入。它只接原始完整包；
// 业务解码和 Entity local 锁仍属于 Game 的正式 Sender/Nest 调用链。
type TCPDispatch func(context.Context, Session, uint32, uint32, wire.PayloadKind, []byte) (any, error)

// ConnectionSession 仅供接入基础设施使用，业务端点仍使用最小的 Session。
// 每连接随机 nonce 不来自客户端。写入只接受完整包，复用唯一的 socket 写临界区。
type ConnectionSession interface {
	Session
	ConnectionID() string
	WritePacket(context.Context, uint32, uint32, wire.PayloadKind, bool, []byte) error
}

// TCPHandshake 让独立 Gate 在同一个握手预算内完成 Bind → ACK → Activate。
// Closed 必须快速返回，通知丢失由绑定租期兜底，不能阻塞连接排空。
type TCPHandshake interface {
	BeforeACK(context.Context, ConnectionSession, string) error
	AfterACK(context.Context, ConnectionSession) error
	Closed(ConnectionSession)
}

func (server *TCPServer) ConnectHandshake(hooks TCPHandshake) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("player tcp: handshake connected after start")
	}
	if server.handshake != nil {
		return errors.New("player tcp: handshake already connected")
	}
	if hooks == nil {
		return errors.New("player tcp: handshake is nil")
	}
	server.handshake = hooks
	return nil
}

// TCPResponse 是已经编码的外部 PB 响应；业务类型通过 TCPReply 显式转换。
type TCPResponse struct {
	MessageID uint32
	Sequence  uint32
	Payload   []byte
}

type TCPReply interface{ TCPResponse() *TCPResponse }

func (reply *TCPResponse) TCPResponse() *TCPResponse { return reply }

// ConnectRuntime 仅在监听开始前接线。Server 与 Runtime 在整个连接寿命中配对，
// Stop 超时保留实例，真实排空后才结束关闭通知。
func (server *TCPServer) ConnectRuntime(runtime *TCPRuntime) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("player tcp: runtime connected after start")
	}
	if runtime == nil {
		return errors.New("player tcp: runtime is nil")
	}
	if server.transport != nil && server.transport != runtime {
		return errors.New("player tcp: server already has a runtime")
	}
	if owner := runtime.owner.Load(); owner != server && !runtime.owner.CompareAndSwap(nil, server) {
		return errors.New("player tcp: runtime already belongs to another server")
	}
	server.transport = runtime
	return nil
}

func (server *TCPServer) Addr() net.Addr {
	server.mu.RLock()
	defer server.mu.RUnlock()
	if server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

type Authenticator interface {
	Authenticate(context.Context, string, net.Addr) (Principal, error)
}

type AuthenticatorFunc func(context.Context, string, net.Addr) (Principal, error)

func (fn AuthenticatorFunc) Authenticate(ctx context.Context, token string, remote net.Addr) (Principal, error) {
	return fn(ctx, token, remote)
}

type TCPServer struct {
	config   TCPConfig
	dispatch TCPDispatch
	// transport is the application-facing TCPRuntime this server feeds tcpSession
	// lifecycle events into. It is set by the Mod before the server starts
	// accepting, and nil in a test that builds a TCPServer directly.
	transport     *TCPRuntime
	authenticator Authenticator
	handshake     TCPHandshake

	mu              sync.RWMutex
	listener        net.Listener
	ctx             context.Context
	cancel          context.CancelFunc
	connections     map[net.Conn]struct{}
	ipConnections   map[string]int
	sessions        map[string]*tcpSession
	playerSessions  map[int64]map[string]*tcpSession
	connectionSlots chan struct{}
	handshakeSlots  chan struct{}
	wait            sync.WaitGroup
	payloadPool     sync.Pool
	stopping        bool
	started         bool
}

func NewTCPServer(config TCPConfig, dispatch TCPDispatch, authenticator Authenticator) (*TCPServer, error) {
	if dispatch == nil {
		return nil, errors.New("player tcp: runtime is nil")
	}
	if authenticator == nil {
		return nil, errors.New("player tcp: authenticator is nil")
	}
	// 未显式声明请求预算时采用当前默认值；登录预算不得超过整条 dispatch 预算。
	if config.DispatchTimeout == 0 {
		config.DispatchTimeout = defaultDispatchTimeout
	}
	if config.LoginTimeout == 0 {
		config.LoginTimeout = min(defaultLoginTimeout, config.DispatchTimeout)
	}
	if err := ValidateTCPConfig(config); err != nil {
		return nil, err
	}
	server := &TCPServer{
		config: config, dispatch: dispatch, authenticator: authenticator,
		connections:     make(map[net.Conn]struct{}),
		ipConnections:   make(map[string]int),
		sessions:        make(map[string]*tcpSession),
		playerSessions:  make(map[int64]map[string]*tcpSession),
		connectionSlots: make(chan struct{}, config.MaxConnections),
		handshakeSlots:  make(chan struct{}, config.MaxHandshakes),
	}
	server.payloadPool.New = func() any { return make([]byte, 4096) }
	return server, nil
}

func (server *TCPServer) Start() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("player tcp: server cannot be restarted")
	}
	listener, err := net.Listen("tcp", server.config.Addr)
	if err != nil {
		return fmt.Errorf("player tcp: listen %s: %w", server.config.Addr, err)
	}
	server.ctx, server.cancel = context.WithCancel(context.Background())
	server.listener = listener
	server.stopping = false
	server.started = true
	if server.transport != nil {
		server.transport.startLifecycle()
		server.transport.server.Store(server)
	}
	metrics.SetGauge("player_tcp_connections", nil, 0)
	server.wait.Add(1)
	go server.acceptLoop(listener)
	slog.Info("player tcp listener started", "addr", listener.Addr().String(), "max_connections", server.config.MaxConnections)
	return nil
}

// Stop closes the listener and every connection, then waits within ctx for
// the accept loop and connection goroutines to return. The first call does
// the closing; a call after a ctx that ended first only waits again, so it
// reports success only once everything has actually returned. Stop on a
// server that never started returns nil.
func (server *TCPServer) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	server.mu.Lock()
	listener, cancel := server.listener, server.cancel
	server.listener, server.cancel = nil, nil
	server.stopping = true
	if server.transport != nil {
		server.transport.server.CompareAndSwap(server, nil)
	}
	started := server.started
	if cancel != nil {
		cancel()
	}
	connections := make([]net.Conn, 0, len(server.connections))
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	server.mu.Unlock()
	if !started {
		return nil
	}
	if listener != nil {
		_ = listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	done := make(chan struct{})
	go func() { server.wait.Wait(); close(done) }()
	select {
	case <-done:
		if server.transport != nil {
			return server.transport.stopLifecycleContext(ctx)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("player tcp: shutdown: %w", ctx.Err())
	}
}

func (server *TCPServer) acceptLoop(listener net.Listener) {
	defer server.wait.Done()
	backoff := 5 * time.Millisecond
	for {
		connection, err := listener.Accept()
		if err != nil {
			if server.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("player tcp accept", "err", err)
			timer := time.NewTimer(backoff)
			select {
			case <-server.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Millisecond
		select {
		case server.connectionSlots <- struct{}{}:
			server.mu.Lock()
			if server.stopping || server.ctx.Err() != nil {
				server.mu.Unlock()
				<-server.connectionSlots
				_ = connection.Close()
				return
			}
			host := remoteHost(connection.RemoteAddr())
			if server.ipConnections[host] >= server.config.MaxConnectionsPerIP {
				server.mu.Unlock()
				<-server.connectionSlots
				metrics.IncCounter("player_tcp_connection_rejected_total", map[string]string{"reason": "per_ip"}, 1)
				_ = connection.Close()
				continue
			}
			server.connections[connection] = struct{}{}
			server.ipConnections[host]++
			server.wait.Add(1)
			server.mu.Unlock()
			go server.serveConnection(connection)
		default:
			metrics.IncCounter("player_tcp_connection_rejected_total", nil, 1)
			_ = connection.Close()
		}
	}
}

// serveConnection 服务一条已接受的连接：取得握手槽 → 认证 → BeforeACK 与 ack → 注册会话 → AfterACK →
// 归还握手槽 → 读循环按序分发请求。
//
// 资源所有权：连接槽、连接表与按 IP 计数、wait 由 acceptLoop 取得，这里在返回时归还。握手槽从取得起持有到
// AfterACK 完成（handshakeHeld），中途返回由 defer 归还。会话创建后，返回时先注销（已注册时）、再关闭并通知
// handshake.Closed。
func (server *TCPServer) serveConnection(connection net.Conn) {
	defer func() {
		if value := recover(); value != nil {
			_ = connection.Close()
			reportBoundaryPanic(func(context.Context, any) {
				slog.Error("player tcp callback panicked", "panic", value, "stack", string(debug.Stack()))
			}, context.Background(), value)
		}
	}()
	defer server.wait.Done()
	defer func() { <-server.connectionSlots }()
	metrics.AddGauge("player_tcp_connections", nil, 1)
	defer metrics.AddGauge("player_tcp_connections", nil, -1)
	defer server.forgetConnection(connection)
	tuneTCPConnection(connection)
	// connectionCtx is this connection's lifetime: the server stopping or the
	// tcpSession closing (a replacing login, CloseSessions, a network error) ends
	// it, and with it whatever request the connection is still running.
	connectionCtx, cancel := context.WithCancelCause(server.ctx)
	defer cancel(ErrSessionClosed)
	handshakeCtx, handshakeCancel := context.WithTimeout(connectionCtx, server.config.HandshakeTimeout)
	defer handshakeCancel()
	if !server.acquireHandshakeSlot(connectionCtx, handshakeCtx, connection) {
		_ = connection.Close()
		return
	}
	handshakeHeld := true
	defer func() {
		if handshakeHeld {
			<-server.handshakeSlots
		}
	}()
	authFrame, release, principal, ok := server.authenticateConnection(handshakeCtx, connection)
	if !ok {
		return
	}
	session := &tcpSession{connection: connection, principal: clonePrincipal(principal), nonce: newBindingToken(), writeTimeout: server.config.WriteTimeout, maxPayloadBytes: server.config.MaxPayloadBytes, cancel: cancel}
	defer server.closeConnectionSession(session)
	if !server.acknowledgeHandshake(handshakeCtx, session, authFrame, release) {
		return
	}
	server.registerSession(session)
	defer server.unregisterSession(session)
	if server.handshake != nil {
		if err := server.handshake.AfterACK(handshakeCtx, session); err != nil || handshakeCtx.Err() != nil {
			return
		}
	}
	handshakeCancel()
	<-server.handshakeSlots
	handshakeHeld = false
	server.readRequests(connectionCtx, connection, session, principal.PlayerID, authFrame.sequence)
}

// forgetConnection 把连接移出连接表并减少它所属 IP 的计数（acceptLoop 登记的反操作）。
func (server *TCPServer) forgetConnection(connection net.Conn) {
	server.mu.Lock()
	delete(server.connections, connection)
	host := remoteHost(connection.RemoteAddr())
	if server.ipConnections[host] <= 1 {
		delete(server.ipConnections, host)
	} else {
		server.ipConnections[host]--
	}
	server.mu.Unlock()
}

// tuneTCPConnection 为玩家 TCP 连接打开保活并关闭 Nagle。
func tuneTCPConnection(connection net.Conn) {
	if tcpConnection, ok := connection.(*net.TCPConn); ok {
		_ = tcpConnection.SetKeepAlive(true)
		_ = tcpConnection.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConnection.SetNoDelay(true)
	}
}

// acquireHandshakeSlot 把读截止时间设为握手截止时间，并在握手期限内排队取得握手槽。返回 false 时没有取得槽，
// 调用方关闭连接。
func (server *TCPServer) acquireHandshakeSlot(connectionCtx, handshakeCtx context.Context, connection net.Conn) bool {
	handshakeDeadline, _ := handshakeCtx.Deadline()
	if err := connection.SetReadDeadline(handshakeDeadline); err != nil {
		return false
	}
	select {
	case server.handshakeSlots <- struct{}{}:
		return true
	case <-connectionCtx.Done():
		return false
	case <-handshakeCtx.Done():
		metrics.IncCounter("player_tcp_auth_failure_total", map[string]string{"reason": "handshake_capacity"}, 1)
		return false
	}
}

// authenticateConnection 读取并校验认证帧，交给 authenticator 认证。成功时返回认证帧与它的缓冲归还函数（由调用方
// 在 BeforeACK 之后归还）；失败时已在这里关闭连接并归还缓冲，ok 为 false。
func (server *TCPServer) authenticateConnection(handshakeCtx context.Context, connection net.Conn) (authFrame frame, release func(), principal Principal, ok bool) {
	authFrame, release, err := server.readFrameLimit(connection, server.config.MaxHandshakeBytes)
	if err != nil {
		metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
		_ = connection.Close()
		return frame{}, nil, Principal{}, false
	}
	if authFrame.flags != 0 || authFrame.messageID != 0 || authFrame.sequence == 0 || len(authFrame.payload) == 0 {
		metrics.IncCounter("player_tcp_auth_failure_total", nil, 1)
		release()
		_ = connection.Close()
		return frame{}, nil, Principal{}, false
	}
	principal, err = server.authenticator.Authenticate(handshakeCtx, string(authFrame.payload), connection.RemoteAddr())
	if err != nil || !principal.Authenticated() || handshakeCtx.Err() != nil {
		metrics.IncCounter("player_tcp_auth_failure_total", nil, 1)
		_ = connection.Close()
		release()
		return frame{}, nil, Principal{}, false
	}
	return authFrame, release, principal, true
}

// closeConnectionSession 在连接返回时关闭会话并通知 handshake.Closed。
func (server *TCPServer) closeConnectionSession(session *tcpSession) {
	_ = session.Close(ErrSessionClosed)
	if server.handshake != nil {
		server.handshake.Closed(session)
	}
}

// acknowledgeHandshake 执行 BeforeACK、归还认证帧缓冲，然后写回 ack（序号沿用认证帧）。返回 false 表示握手
// 没有完成，调用方结束连接。
func (server *TCPServer) acknowledgeHandshake(handshakeCtx context.Context, session *tcpSession, authFrame frame, release func()) bool {
	var err error
	if server.handshake != nil {
		err = server.handshake.BeforeACK(handshakeCtx, session, string(authFrame.payload))
	}
	release()
	if err != nil || handshakeCtx.Err() != nil {
		return false
	}
	return session.writeFrame(handshakeCtx, 0, 0, authFrame.sequence, nil) == nil
}

// readRequests 是握手完成后的读循环：同一连接上的请求按到达顺序同步处理（不为每个请求开 goroutine）。
// 序号必须严格递增；心跳也消耗限流令牌。任何读、校验、分发或写回失败都结束循环，由 serveConnection 收尾。
func (server *TCPServer) readRequests(connectionCtx context.Context, connection net.Conn, session *tcpSession, playerID int64, lastSequence uint32) {
	var limiter *rate.Limiter
	if server.config.RequestRate > 0 {
		limiter = rate.NewLimiter(rate.Limit(server.config.RequestRate), server.config.RequestBurst)
	}
	for {
		if err := connection.SetReadDeadline(time.Now().Add(server.config.IdleTimeout)); err != nil {
			return
		}
		request, releasePayload, err := server.readFrame(connection)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
			}
			return
		}
		if (request.flags != 0 && request.flags != flagLockstep) || request.sequence == 0 || int32(request.sequence-lastSequence) <= 0 {
			metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
			releasePayload()
			return
		}
		lastSequence = request.sequence
		// 心跳也消耗令牌，防止保活帧绕过每连接限流；同一读循环内保持顺序。
		if limiter != nil && !limiter.Allow() {
			metrics.IncCounter("player_tcp_rate_limited_total", nil, 1)
			releasePayload()
			return
		}
		if request.messageID == 0 {
			if !server.answerHeartbeat(connectionCtx, session, request, releasePayload) {
				return
			}
			continue
		}
		if !server.dispatchRequest(connectionCtx, session, playerID, request, releasePayload) {
			return
		}
	}
}

// answerHeartbeat 校验心跳帧并原序号回一个空帧。返回 false 时调用方结束连接。
func (server *TCPServer) answerHeartbeat(connectionCtx context.Context, session *tcpSession, request frame, releasePayload func()) bool {
	valid := request.flags == 0 && server.config.HeartbeatEnabled && len(request.payload) == 0
	releasePayload()
	if !valid {
		metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
		return false
	}
	return session.writeFrame(connectionCtx, 0, 0, request.sequence, nil) == nil
}

// dispatchRequest 在分发预算内处理一个请求并写回应答。返回 false 时调用方结束连接。
//
// One request, one deadline. Without it a handler
// waiting on something that never finishes — a cold load behind a
// projection that keeps failing — held this goroutine, this socket's
// reads and the connection slot until the process stopped. The read
// loop is synchronous on purpose (per-connection order, no goroutine
// per request); the deadline is what keeps that from being unbounded.
// It bounds waiting, not work: a request the handler already had
// admitted is not undone by it.
func (server *TCPServer) dispatchRequest(connectionCtx context.Context, session *tcpSession, playerID int64, request frame, releasePayload func()) bool {
	dispatchStarted := time.Now()
	dispatchCtx, cancelDispatch := context.WithTimeout(connectionCtx, server.config.DispatchTimeout)
	response, dispatchErr := server.dispatch(dispatchCtx, session, request.messageID, request.sequence, wire.PayloadKind((request.flags>>1)&3), request.payload)
	overBudget := errors.Is(dispatchCtx.Err(), context.DeadlineExceeded)
	cancelDispatch()
	metrics.ObserveDuration("player_tcp_dispatch_duration", nil, time.Since(dispatchStarted))
	releasePayload()
	if overBudget {
		metrics.IncCounter("player_tcp_dispatch_timeout_total", nil, 1)
	}
	if dispatchErr != nil {
		metrics.IncCounter("player_tcp_dispatch_error_total", nil, 1)
		slog.Warn("player tcp dispatch failed", "player_id", playerID, "message_id", request.messageID, "sequence", request.sequence, "over_budget", overBudget, "err", dispatchErr)
		return false
	}
	// The answer goes out on the connection's context, not the request's:
	// a handler that finished at its deadline is reporting what happened,
	// and dropping that report would leave the client guessing.
	if response != nil {
		if err := session.Reply(connectionCtx, response); err != nil {
			return false
		}
	}
	return true
}

func (server *TCPServer) registerSession(current *tcpSession) {
	server.mu.Lock()
	previous := server.sessions[current.principal.SessionID]
	server.sessions[current.principal.SessionID] = current
	byPlayer := server.playerSessions[current.principal.PlayerID]
	if byPlayer == nil {
		byPlayer = make(map[string]*tcpSession)
		server.playerSessions[current.principal.PlayerID] = byPlayer
	}
	byPlayer[current.principal.SessionID] = current
	server.mu.Unlock()
	if previous != nil && previous != current {
		_ = previous.Close(ErrSessionClosed)
	}
}

func (server *TCPServer) unregisterSession(current *tcpSession) {
	server.forgetSession(current)
	// The connection is gone; say so once, asynchronously. Everything that
	// maintains an "online" set needs this, and the alternative — noticing by
	// failing to push — never fires if nothing tries.
	if server.transport != nil {
		server.transport.publishClosed(TCPSessionClosed{PlayerID: current.principal.PlayerID, SessionID: current.principal.SessionID})
	}
}

// forgetSession takes a tcpSession out of the lookup maps, if it is still the
// one registered under its id. It is the only place that does; the tcpSession's
// close event is published by unregisterSession, once, from the connection's
// own teardown.
func (server *TCPServer) forgetSession(current *tcpSession) {
	server.mu.Lock()
	if server.sessions[current.principal.SessionID] == current {
		delete(server.sessions, current.principal.SessionID)
	}
	if byPlayer := server.playerSessions[current.principal.PlayerID]; byPlayer != nil {
		if byPlayer[current.principal.SessionID] == current {
			delete(byPlayer, current.principal.SessionID)
		}
		if len(byPlayer) == 0 {
			delete(server.playerSessions, current.principal.PlayerID)
		}
	}
	server.mu.Unlock()
}

// dropBrokenSession closes a connection a push could not be written to.
// It is forgotten first, so the push's caller already sees
// it gone (ActiveSessions, the next push); closing the socket then ends its
// read loop, whose teardown releases the connection slot and the per-IP count
// and publishes the session-close event — the same single release path as any
// other disconnect.
func (server *TCPServer) dropBrokenSession(current *tcpSession, cause error) {
	server.forgetSession(current)
	metrics.IncCounter("player_tcp_push_closed_total", nil, 1)
	slog.Debug("player tcp: closing a connection that could not take a push", "player_id", current.principal.PlayerID, "session_id", current.principal.SessionID, "err", cause)
	_ = current.Close(fmt.Errorf("%w: push not written: %w", ErrSessionClosed, cause))
}

// pushPlayer sends one frame to every connection of the player.
//
// A connection the frame could not be written to is closed (dropBrokenSession):
// a partly written frame has put its stream out of step, and a connection that
// cannot take frames — typically the old socket of a player who has logged in
// again — would otherwise stay registered until its read loop notices, failing
// every push meanwhile. The push is then a success if another connection took
// it: the player is reachable, and the closed connection's client has to
// reconnect anyway. It fails when no connection took it (all of them are closed
// by then), and when a connection refused it before writing anything (context
// done or its deadline hit before the first byte, payload too large) — that is
// not the connection's fault, so nothing is
// closed and the caller learns the frame did not go out everywhere.
func (server *TCPServer) pushPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error {
	return server.pushPlayerKind(ctx, playerID, messageID, payload, 0)
}

func (server *TCPServer) pushPlayerKind(ctx context.Context, playerID int64, messageID uint32, payload []byte, kind byte) error {
	if messageID == 0 {
		return fmt.Errorf("%w: push message id is zero", errInvalidFrame)
	}
	server.mu.RLock()
	byPlayer := server.playerSessions[playerID]
	sessions := make([]*tcpSession, 0, len(byPlayer))
	for _, current := range byPlayer {
		sessions = append(sessions, current)
	}
	server.mu.RUnlock()
	// Counted apart from push errors: a push to a player with no tcpSession is the
	// "who is still addressing a gone player" signal, and it used to leave no trace.
	if len(sessions) == 0 {
		metrics.IncCounter("player_tcp_push_no_session_total", nil, 1)
		return fmt.Errorf("%w: player %d", ErrSessionNotFound, playerID)
	}
	var joined error
	delivered, refused := 0, false
	for _, current := range sessions {
		err := current.pushKind(ctx, messageID, payload, kind)
		switch {
		case err == nil:
			delivered++
			continue
		case errors.Is(err, errConnectionBroken):
			server.dropBrokenSession(current, err)
		default:
			refused = true
		}
		joined = errors.Join(joined, fmt.Errorf("tcpSession %s: %w", current.principal.SessionID, err))
	}
	if delivered > 0 {
		metrics.IncCounter("player_tcp_push_total", nil, int64(delivered))
	}
	if joined == nil {
		return nil
	}
	metrics.IncCounter("player_tcp_push_error_total", nil, 1)
	if delivered > 0 && !refused {
		return nil
	}
	return joined
}

func (server *TCPServer) pushSession(ctx context.Context, sessionID string, messageID uint32, payload []byte) error {
	return server.pushSessionKind(ctx, sessionID, messageID, payload, 0)
}

func (server *TCPServer) pushSessionKind(ctx context.Context, sessionID string, messageID uint32, payload []byte, kind byte) error {
	if messageID == 0 {
		return fmt.Errorf("%w: push message id is zero", errInvalidFrame)
	}
	server.mu.RLock()
	current := server.sessions[sessionID]
	server.mu.RUnlock()
	if current == nil {
		metrics.IncCounter("player_tcp_push_no_session_total", nil, 1)
		return fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	err := current.pushKind(ctx, messageID, payload, kind)
	if err == nil {
		metrics.IncCounter("player_tcp_push_total", nil, 1)
		return nil
	}
	metrics.IncCounter("player_tcp_push_error_total", nil, 1)
	if errors.Is(err, errConnectionBroken) {
		server.dropBrokenSession(current, err)
	}
	return err
}

func (server *TCPServer) activeSessions(playerID int64) int {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return len(server.playerSessions[playerID])
}

// closeSessions snapshots the player's sessions under the lock and closes
// them outside it: Close runs the connection's teardown, which ends up taking
// this same lock to unregister the tcpSession.
func (server *TCPServer) closeSessions(playerID int64, reason error) int {
	server.mu.RLock()
	byPlayer := server.playerSessions[playerID]
	targets := make([]*tcpSession, 0, len(byPlayer))
	for _, current := range byPlayer {
		targets = append(targets, current)
	}
	server.mu.RUnlock()
	// Counted by whether this call is the one that closed the tcpSession, not
	// by Close returning nil: Close returns the close reason, which is never
	// nil. A tcpSession another path already closed is not counted again.
	closed := 0
	for _, current := range targets {
		if current.close(reason) {
			closed++
		}
	}
	return closed
}

func remoteHost(address net.Addr) string {
	if address == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(address.String())
	if err == nil && host != "" {
		return host
	}
	return address.String()
}
