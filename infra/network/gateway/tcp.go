// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"golang.org/x/time/rate"
)

// TCPDispatch 由本地 ProtocolRegistry 或 Gate forwarder 注入。它只接原始完整包；
// 业务解码和 Entity local 锁仍属于 Game 的正式 Sender/Nest 调用链。
type TCPDispatch func(context.Context, Session, uint32, uint32, wire.PayloadKind, []byte) (any, error)

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
// reports success only once everything has actually returned
// (RR-20261005-NC-83). Stop on a server that never started returns nil.
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

func (server *TCPServer) serveConnection(connection net.Conn) {
	defer server.wait.Done()
	defer func() { <-server.connectionSlots }()
	metrics.AddGauge("player_tcp_connections", nil, 1)
	defer metrics.AddGauge("player_tcp_connections", nil, -1)
	defer func() {
		server.mu.Lock()
		delete(server.connections, connection)
		host := remoteHost(connection.RemoteAddr())
		if server.ipConnections[host] <= 1 {
			delete(server.ipConnections, host)
		} else {
			server.ipConnections[host]--
		}
		server.mu.Unlock()
	}()
	if tcpConnection, ok := connection.(*net.TCPConn); ok {
		_ = tcpConnection.SetKeepAlive(true)
		_ = tcpConnection.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConnection.SetNoDelay(true)
	}
	// connectionCtx is this connection's lifetime: the server stopping or the
	// tcpSession closing (a replacing login, CloseSessions, a network error) ends
	// it, and with it whatever request the connection is still running.
	connectionCtx, cancel := context.WithCancelCause(server.ctx)
	defer cancel(ErrSessionClosed)
	handshakeCtx, handshakeCancel := context.WithTimeout(connectionCtx, server.config.HandshakeTimeout)
	defer handshakeCancel()
	handshakeDeadline, _ := handshakeCtx.Deadline()
	if err := connection.SetReadDeadline(handshakeDeadline); err != nil {
		_ = connection.Close()
		return
	}
	select {
	case server.handshakeSlots <- struct{}{}:
	case <-connectionCtx.Done():
		_ = connection.Close()
		return
	case <-handshakeCtx.Done():
		metrics.IncCounter("player_tcp_auth_failure_total", map[string]string{"reason": "handshake_capacity"}, 1)
		_ = connection.Close()
		return
	}
	handshakeHeld := true
	defer func() {
		if handshakeHeld {
			<-server.handshakeSlots
		}
	}()
	authFrame, release, err := server.readFrameLimit(connection, server.config.MaxHandshakeBytes)
	if err != nil {
		metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
		_ = connection.Close()
		return
	}
	if authFrame.flags != 0 || authFrame.messageID != 0 || authFrame.sequence == 0 || len(authFrame.payload) == 0 {
		metrics.IncCounter("player_tcp_auth_failure_total", nil, 1)
		release()
		_ = connection.Close()
		return
	}
	principal, err := server.authenticator.Authenticate(handshakeCtx, string(authFrame.payload), connection.RemoteAddr())
	handshakeCancel()
	release()
	<-server.handshakeSlots
	handshakeHeld = false
	if err != nil || !principal.Authenticated() {
		metrics.IncCounter("player_tcp_auth_failure_total", nil, 1)
		_ = connection.Close()
		return
	}
	session := &tcpSession{connection: connection, principal: clonePrincipal(principal), writeTimeout: server.config.WriteTimeout, maxPayloadBytes: server.config.MaxPayloadBytes, cancel: cancel}
	defer func() {
		_ = session.Close(ErrSessionClosed)
	}()
	if err := session.writeFrame(connectionCtx, 0, 0, authFrame.sequence, nil); err != nil {
		return
	}
	server.registerSession(session)
	defer server.unregisterSession(session)
	lastSequence := authFrame.sequence
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
			valid := request.flags == 0 && server.config.HeartbeatEnabled && len(request.payload) == 0
			releasePayload()
			if !valid {
				metrics.IncCounter("player_tcp_frame_error_total", nil, 1)
				return
			}
			if err := session.writeFrame(connectionCtx, 0, 0, request.sequence, nil); err != nil {
				return
			}
			continue
		}
		// One request, one deadline (RR-20260926-36). Without it a handler
		// waiting on something that never finishes — a cold load behind a
		// projection that keeps failing — held this goroutine, this socket's
		// reads and the connection slot until the process stopped. The read
		// loop is synchronous on purpose (per-connection order, no goroutine
		// per request); the deadline is what keeps that from being unbounded.
		// It bounds waiting, not work: a request the handler already had
		// admitted is not undone by it.
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
			slog.Warn("player tcp dispatch failed", "player_id", principal.PlayerID, "message_id", request.messageID, "sequence", request.sequence, "over_budget", overBudget, "err", dispatchErr)
			return
		}
		// The answer goes out on the connection's context, not the request's:
		// a handler that finished at its deadline is reporting what happened,
		// and dropping that report would leave the client guessing.
		if response != nil {
			if err := session.Reply(connectionCtx, response); err != nil {
				return
			}
		}
	}
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
	// failing to push — never fires if nothing tries (RR-20260918-06).
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

// dropBrokenSession closes a connection a push could not be written to
// (RR-20260926-52). It is forgotten first, so the push's caller already sees
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
	// "who is still addressing a gone player" signal, and it used to leave no trace (U-0278).
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
	// Counted by whether this call is the one that closed the tcpSession
	// (RR-20260927-02). It used to count Close returning nil, but Close
	// returns the close reason, which is never nil, so every call reported
	// 0; a tcpSession another path already closed is not counted again.
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
