// TCP regression extraction source
package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

func TestTCPRuntimeBelongsToOneServerBeforeStart(t *testing.T) {
	newServer := func() *TCPServer {
		server, err := NewTCPServer(DefaultTCPConfig(), emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return Principal{}, nil }))
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	first, second := newServer(), newServer()
	runtime := NewTCPRuntime(nil, time.Second)
	if err := first.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := second.ConnectRuntime(runtime); err == nil {
		t.Fatal("two listeners obtained the same runtime")
	}
	if first.transport != runtime || second.transport != nil {
		t.Fatal("rejected connection changed runtime ownership")
	}
}

func TestTCPDefaultRequestBudgetsAreAppliedAtConstruction(t *testing.T) {
	config := DefaultTCPConfig()
	config.DispatchTimeout, config.LoginTimeout = 0, 0
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return Principal{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if server.config.DispatchTimeout != defaultDispatchTimeout || server.config.LoginTimeout != defaultLoginTimeout {
		t.Fatalf("request budgets: %+v", server.config)
	}
}

func TestHeartbeatKeepsAuthenticatedPushConnectionAlive(t *testing.T) {
	server := dispatchServer(t, "1s", func(*tcpTestContext, string) (string, error) {
		t.Error("heartbeat entered business handler")
		return "", nil
	}, func(c *TCPConfig) { c.IdleTimeout = 300 * time.Millisecond })
	conn := dialAuthenticated(t, server, "heartbeat")
	client := &tcpSession{connection: conn, writeTimeout: time.Second}
	for seq := uint32(2); seq <= 5; seq++ {
		time.Sleep(100 * time.Millisecond)
		if err := client.writeFrame(context.Background(), 0, 0, seq, nil); err != nil {
			t.Fatal(err)
		}
		ack, release, err := server.readFrame(conn)
		if err != nil {
			t.Fatalf("authenticated heartbeat was disconnected: %v", err)
		}
		release()
		if ack.messageID != 0 || ack.sequence != seq || len(ack.payload) != 0 {
			t.Fatalf("heartbeat ack: %+v", ack)
		}
	}
	if _, release, err := server.readFrame(conn); err == nil {
		release()
		t.Fatal("silent connection survived idle timeout")
	}
}

func TestPerConnectionTokenBucketIncludesHeartbeats(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			server := dispatchServer(t, "1s", func(*tcpTestContext, string) (string, error) { return "ok", nil }, func(c *TCPConfig) {
				c.RequestRate = 0
				if enabled {
					c.RequestRate = 0.01
				}
				c.RequestBurst = 2
			})
			for _, token := range []string{"rate-a", "rate-b"} {
				conn := dialAuthenticated(t, server, token)
				client := &tcpSession{connection: conn, writeTimeout: time.Second}
				for seq := uint32(2); seq <= 4; seq++ {
					if err := client.writeFrame(context.Background(), 0, 0, seq, nil); err != nil {
						t.Fatal(err)
					}
					_, release, err := server.readFrame(conn)
					release()
					if enabled && seq == 4 {
						if err == nil {
							t.Fatal("third heartbeat exceeded burst but was accepted")
						}
					} else if err != nil {
						t.Fatalf("new connection or disabled limit refused: %v", err)
					}
				}
				_ = conn.Close()
			}
		})
	}
}

func TestFrameRoundTrip(t *testing.T) {
	client, serverConnection := net.Pipe()
	defer client.Close()
	defer serverConnection.Close()
	session := &tcpSession{connection: client, writeTimeout: time.Second}
	writeResult := make(chan error, 1)
	go func() { writeResult <- session.writeFrame(context.Background(), 0, 10001, 7, []byte("payload")) }()
	server := &TCPServer{config: TCPConfig{MaxPayloadBytes: 1024}}
	server.payloadPool.New = func() any { return make([]byte, 4096) }
	got, release, err := server.readFrame(serverConnection)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got.messageID != 10001 || got.sequence != 7 || !bytes.Equal(got.payload, []byte("payload")) {
		t.Fatalf("unexpected frame: %+v", got)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
}

func TestReadFrameRejectsLengthBeforeAllocation(t *testing.T) {
	var header [headerSize]byte
	header[0], header[1], header[2] = frameMagic[0], frameMagic[1], protocolVersion
	binary.BigEndian.PutUint32(header[12:16], 1025)
	server := &TCPServer{config: TCPConfig{MaxPayloadBytes: 1024}}
	_, _, err := server.readFrame(bytes.NewReader(header[:]))
	if !errors.Is(err, errInvalidFrame) {
		t.Fatalf("error = %v", err)
	}
}

func TestHandshakeFrameUsesIndependentSmallLimit(t *testing.T) {
	var header [headerSize]byte
	header[0], header[1], header[2] = frameMagic[0], frameMagic[1], protocolVersion
	binary.BigEndian.PutUint32(header[12:16], 8193)
	server := &TCPServer{config: TCPConfig{MaxPayloadBytes: 1 << 20}}
	_, _, err := server.readFrameLimit(bytes.NewReader(header[:]), 8192)
	if !errors.Is(err, errInvalidFrame) {
		t.Fatalf("error = %v", err)
	}
}

func TestSessionReplyRejectsUnknownEnvelope(t *testing.T) {
	session := &tcpSession{}
	if err := session.Reply(context.Background(), struct{}{}); err == nil {
		t.Fatal("unknown response envelope unexpectedly accepted")
	}
}

func TestSessionReplyRejectsReservedFields(t *testing.T) {
	session := &tcpSession{}
	if err := session.Reply(context.Background(), &TCPResponse{}); !errors.Is(err, errInvalidFrame) {
		t.Fatalf("error = %v", err)
	}
}

func TestPushRejectsReservedMessageID(t *testing.T) {
	server := &TCPServer{}
	if err := server.pushPlayer(context.Background(), 1, 0, nil); !errors.Is(err, errInvalidFrame) {
		t.Fatalf("error = %v", err)
	}
	if err := server.pushSession(context.Background(), "tcpSession", 0, nil); !errors.Is(err, errInvalidFrame) {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteFrameHonorsEarlierContextDeadline(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	session := &tcpSession{connection: client, writeTimeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := session.writeFrame(ctx, 0, 1, 1, []byte("blocked")); err == nil {
		t.Fatal("blocked write unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("write ignored context deadline: %v", elapsed)
	}
}

func TestSessionPrincipalClaimsAreIsolated(t *testing.T) {
	session := &tcpSession{principal: Principal{PlayerID: 1, SessionID: "tcpSession", Claims: map[string]string{"role": "player"}}}
	principal := session.Principal()
	principal.Claims["role"] = "admin"
	if got := session.Principal().Claims["role"]; got != "player" {
		t.Fatalf("tcpSession claims were mutated through returned principal: %q", got)
	}
}

func TestRuntimePushPlayerEncodesOnceAndMarksServerPush(t *testing.T) {
	encodeCalls := 0
	encode := func(_ uint32, value any) ([]byte, error) { encodeCalls++; return []byte(value.(string)), nil }
	server, err := NewTCPServer(DefaultTCPConfig(), emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return Principal{PlayerID: 1, SessionID: "tcpSession"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	connectionCtx, cancel := context.WithCancelCause(context.Background())
	current := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 1, SessionID: "tcpSession"}, server.config)
	defer server.closeConnectionSession(current)
	server.registerSession(current)
	defer server.unregisterSession(current)
	runtime := &TCPRuntime{encode: encode}
	runtime.server.Store(server)
	pushResult := make(chan error, 1)
	go func() { pushResult <- runtime.PushPlayer(context.Background(), 1, 20001, "notice") }()
	got, release, err := server.readFrame(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got.flags != flagServerPush || got.messageID != 20001 || got.sequence == 0 || string(got.payload) != "notice" {
		t.Fatalf("unexpected push frame: %+v", got)
	}
	if err := <-pushResult; err != nil {
		t.Fatal(err)
	}
	if encodeCalls != 1 {
		t.Fatalf("encoder called %d times", encodeCalls)
	}
}

// 一个订阅者 panic 不能带走进程：分发器在自己的 goroutine 上运行，没人 recover 的 panic 会结束整个进程，把每次断线
// 变成所有玩家的事故。像框架里其他回调边界一样逐个回调隔离，而且 panic 之后的订阅者照样要跑——“谁在线”正是必须收敛的状态。
func TestASubscriberPanicIsIsolated(t *testing.T) {
	runtime := &TCPRuntime{}
	var before, after int
	runtime.OnSessionClosed(func(TCPSessionClosed) { before++ })
	runtime.OnSessionClosed(func(TCPSessionClosed) { panic("subscriber blew up") })
	runtime.OnSessionClosed(func(TCPSessionClosed) { after++ })
	runtime.deliverClosed(TCPSessionClosed{PlayerID: 1, SessionID: "tcpSession"})
	if before != 1 || after != 1 {
		t.Fatalf("healthy subscribers ran before=%d after=%d, want 1 each: a panic ate the rest of the batch", before, after)
	}
	// And the source keeps working: the next event reaches everyone again.
	runtime.deliverClosed(TCPSessionClosed{PlayerID: 2, SessionID: "tcpSession-2"})
	if before != 2 || after != 2 {
		t.Fatalf("after a panic the source stopped delivering: before=%d after=%d", before, after)
	}
}

// The same through the real path: published from the socket cleanup,
// dispatched on the lifecycle goroutine, and the drain at Stop still finishes.
func TestAPanickingSubscriberDoesNotStopTheDispatcher(t *testing.T) {
	runtime := &TCPRuntime{}
	delivered := make(chan int64, 4)
	runtime.OnSessionClosed(func(TCPSessionClosed) { panic("subscriber blew up") })
	runtime.OnSessionClosed(func(event TCPSessionClosed) { delivered <- event.PlayerID })
	runtime.startLifecycle()
	runtime.publishClosed(TCPSessionClosed{PlayerID: 7, SessionID: "a"})
	runtime.publishClosed(TCPSessionClosed{PlayerID: 8, SessionID: "b"})
	for _, want := range []int64{7, 8} {
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("delivered player %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("player %d was never delivered; the dispatcher died with the panic", want)
		}
	}
	runtime.stopLifecycle()
}

const (
	stuckRequestID  uint32 = 10001
	stuckResponseID uint32 = 10002
)

// stuckDispatchServer serves one request type whose work never finishes on
// its own — the shape of a cold login waiting on a projection that keeps
// failing. The handler honors its context the way the projection wait does and
// answers once the context ends.
func stuckDispatchServer(t *testing.T, dispatchTimeout string) (*TCPServer, <-chan context.Context) {
	t.Helper()
	started := make(chan context.Context, 4)
	server := dispatchServer(t, dispatchTimeout, func(ctx *tcpTestContext, _ string) (string, error) {
		started <- ctx.Context()
		<-ctx.Context().Done()
		return "gave up: " + ctx.Context().Err().Error(), nil
	})
	return server, started
}

func dispatchServer(t testing.TB, dispatchTimeout string, handler func(*tcpTestContext, string) (string, error), configure ...func(*TCPConfig)) *TCPServer {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	timeout, err := time.ParseDuration(dispatchTimeout)
	if err != nil {
		t.Fatal(err)
	}
	config.DispatchTimeout, config.LoginTimeout = timeout, timeout
	for _, option := range configure {
		option(&config)
	}
	dispatch := func(ctx context.Context, session Session, msgID, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if msgID != stuckRequestID || kind != wire.PayloadProtobuf {
			return nil, ErrInvalidRequest
		}
		value, err := handler(&tcpTestContext{base: ctx}, string(payload))
		if err != nil {
			return nil, err
		}
		return &TCPResponse{MessageID: stuckResponseID, Sequence: seq, Payload: []byte(value)}, nil
	}
	server, err := NewTCPServer(config, dispatch, AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (Principal, error) {
		return Principal{PlayerID: 7, SessionID: token}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	return server
}

func dialAuthenticated(t testing.TB, server *TCPServer, token string) net.Conn {
	t.Helper()
	server.mu.RLock()
	addr := server.listener.Addr().String()
	server.mu.RUnlock()
	connection, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	if err := client.writeFrame(context.Background(), 0, 0, 1, []byte(token)); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack, release, err := server.readFrame(connection)
	if err != nil {
		t.Fatalf("authentication ack: %v", err)
	}
	release()
	if ack.messageID != 0 || ack.sequence != 1 {
		t.Fatalf("unexpected ack %+v", ack)
	}
	return connection
}

func sendStuckRequest(t *testing.T, connection net.Conn, sequence uint32) {
	t.Helper()
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	if err := client.writeFrame(context.Background(), 0, stuckRequestID, sequence, []byte("login")); err != nil {
		t.Fatal(err)
	}
}

// waitRegistered 有界地等待玩家注册到 want 个会话。服务端先写认证 ack 再注册 tcpSession（serveConnection），客户端拿到 ack
// 还不能证明服务端已计入它：在 dialAuthenticated 之后马上 push、关闭或计数的用例要先等注册。
func waitRegistered(t *testing.T, runtime *TCPRuntime, playerID int64, want int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); runtime.ActiveSessions(playerID) != want && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.ActiveSessions(playerID); got != want {
		t.Fatalf("active sessions = %d, want %d", got, want)
	}
}

// waitReleased waits for the server's connection slots and per-IP counts to
// come down to what the still-open connections hold.
func waitReleased(t *testing.T, server *TCPServer, open int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	counted := func() (slots, perIP int) {
		server.mu.RLock()
		defer server.mu.RUnlock()
		for _, count := range server.ipConnections {
			perIP += count
		}
		return len(server.connectionSlots), perIP
	}
	slots, perIP := counted()
	for (slots != open || perIP != open) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		slots, perIP = counted()
	}
	if slots != open || perIP != open {
		t.Fatalf("connection slots in use = %d, per-IP count = %d, want %d each", slots, perIP, open)
	}
}

// 永远不结束的请求在分发预算处结束并得到应答；客户端离开后连接的槽位与按 IP 计数要归还（handler 的 context 不能是
// 只有服务器停止才结束的连接 context）。
func TestADispatchThatOutlivesItsBudgetAnswersAndFreesTheSlot(t *testing.T) {
	server, started := stuckDispatchServer(t, "200ms")
	connection := dialAuthenticated(t, server, "tcpSession-a")
	began := time.Now()
	sendStuckRequest(t, connection, 2)
	select {
	case ctx := <-started:
		if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(began) > time.Second {
			t.Errorf("the dispatch context deadline is %v (set=%v), want about 200ms after the request", deadline.Sub(began), ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached its handler")
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, release, err := server.readFrame(connection)
	if err != nil {
		t.Fatalf("no answer %v after the request was sent (budget 200ms): %v", time.Since(began).Round(time.Millisecond), err)
	}
	defer release()
	if response.messageID != stuckResponseID || response.sequence != 2 || !bytes.HasPrefix(response.payload, []byte("gave up: context deadline exceeded")) {
		t.Fatalf("unexpected response %+v (%q)", response, response.payload)
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Errorf("answered after %v, budget 200ms", elapsed)
	}
	// The connection is still usable: the budget ended one request, not the tcpSession.
	sendStuckRequest(t, connection, 3)
	<-started
	if _, release, err := server.readFrame(connection); err != nil {
		t.Fatalf("second request on the same connection: %v", err)
	} else {
		release()
	}
	_ = connection.Close()
	waitReleased(t, server, 0)
}

// 同一 SessionID 在新连接上登录（重连）关闭旧 tcpSession 时，取消旧连接仍在执行的请求，旧 goroutine 与槽位立即释放，
// 而不是等服务器停止（demo 的认证通常每个连接分配新 ID，这里覆盖同 ID 替换）。
func TestClosingASessionCancelsItsInFlightDispatch(t *testing.T) {
	server, started := stuckDispatchServer(t, "1m")
	old := dialAuthenticated(t, server, "tcpSession-r")
	sendStuckRequest(t, old, 2)
	var inFlight context.Context
	select {
	case inFlight = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached its handler")
	}
	dialAuthenticated(t, server, "tcpSession-r")
	select {
	case <-inFlight.Done():
		if cause := context.Cause(inFlight); !errors.Is(cause, ErrSessionClosed) {
			t.Errorf("the in-flight request was cancelled by %v, want the tcpSession close", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced tcpSession's in-flight dispatch is still running: closing a tcpSession did not cancel it")
	}
	waitReleased(t, server, 1)
}

// A15 (closing batch 2): a handler that does not honor its context. The
// dispatch budget bounds the wait, not the work: at the deadline the
// handler's context ends, but the read loop is synchronous (per-connection
// order, no goroutine per request), so the handler keeps running until it
// returns on its own. Its answer goes out only then, and it is the handler's
// result, not a timeout. The connection's slot and per-IP count come back
// only after the handler has returned and the connection has ended — a client
// that hangs up first does not free them earlier, because the read loop that
// would notice is still inside the handler.
// TestADispatchThatOutlivesItsBudgetAnswersAndFreesTheSlot cannot pin this:
// its handler returns as soon as its context ends.
func TestADispatchTimeoutBoundsTheWaitNotAnUncooperativeHandler(t *testing.T) {
	releases := make(chan struct{})
	finished := make(chan struct{})
	started := make(chan context.Context, 4)
	returned := make(chan struct{}, 4)
	server := dispatchServer(t, "100ms", func(ctx *tcpTestContext, _ string) (string, error) {
		started <- ctx.Context()
		select { // the context is not consulted: this is the uncooperative handler
		case <-releases:
		case <-finished:
		}
		returned <- struct{}{}
		return "finished after the budget", nil
	})
	// Registered after the server's own cleanup, so it runs first and a failed
	// test does not leave Stop waiting on a handler nobody releases.
	t.Cleanup(func() { close(finished) })
	awaitStart := func() context.Context {
		t.Helper()
		select {
		case ctx := <-started:
			return ctx
		case <-time.After(2 * time.Second):
			t.Fatal("the request never reached its handler")
			return nil
		}
	}
	inUse := func() (slots, perIP int) {
		server.mu.RLock()
		defer server.mu.RUnlock()
		for _, count := range server.ipConnections {
			perIP += count
		}
		return len(server.connectionSlots), perIP
	}

	connection := dialAuthenticated(t, server, "tcpSession-u")
	sendStuckRequest(t, connection, 2)
	dispatchCtx := awaitStart()
	select {
	case <-dispatchCtx.Done():
		if !errors.Is(dispatchCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("the dispatch context ended with %v, want its deadline", dispatchCtx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatch context did not end at its 100ms budget")
	}
	// The budget is over; the handler is not. Nothing is answered meanwhile.
	_ = connection.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if early, releaseFrame, err := server.readFrame(connection); err == nil {
		releaseFrame()
		t.Fatalf("answered %+v (%q) while the handler past its budget was still running", early, early.payload)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("waiting for no answer: %v, want a read timeout", err)
	}
	releases <- struct{}{}
	<-returned
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, releaseFrame, err := server.readFrame(connection)
	if err != nil {
		t.Fatalf("no answer after the handler returned: %v", err)
	}
	if response.messageID != stuckResponseID || response.sequence != 2 || string(response.payload) != "finished after the budget" {
		releaseFrame()
		t.Fatalf("answer %+v (%q), want the handler's own result for sequence 2", response, response.payload)
	}
	releaseFrame()
	if slots, perIP := inUse(); slots != 1 || perIP != 1 {
		t.Fatalf("connection slots = %d, per-IP = %d with the connection open, want 1 each", slots, perIP)
	}

	// The client hangs up while a second uncooperative request runs: the slot
	// stays taken until the handler returns.
	sendStuckRequest(t, connection, 3)
	awaitStart()
	_ = connection.Close()
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if slots, perIP := inUse(); slots != 1 || perIP != 1 {
			t.Fatalf("connection slots = %d, per-IP = %d while its handler still runs, want 1 each: the slot came back before the work ended", slots, perIP)
		}
	}
	releases <- struct{}{}
	<-returned
	waitReleased(t, server, 0)
}

const pushTestMessageID uint32 = 20002

func pushServer(t *testing.T, configure ...func(*TCPConfig)) (*TCPServer, *TCPRuntime, <-chan TCPSessionClosed) {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	config.WriteTimeout = 100 * time.Millisecond
	for _, option := range configure {
		option(&config)
	}
	encode := func(_ uint32, value any) ([]byte, error) { return value.([]byte), nil }
	runtime := NewTCPRuntime(encode, config.LoginTimeout)
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (Principal, error) {
		return Principal{PlayerID: 7, SessionID: token}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	closed := make(chan TCPSessionClosed, 16)
	runtime.OnSessionClosed(func(event TCPSessionClosed) { closed <- event })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	return server, runtime, closed
}

// readPushes drains a client connection and counts the pushes it got.
func readPushes(t *testing.T, server *TCPServer, connection net.Conn) <-chan int {
	t.Helper()
	counts := make(chan int, 1024)
	go func() {
		defer close(counts)
		for {
			_ = connection.SetReadDeadline(time.Time{})
			got, release, err := server.readFrame(connection)
			if err != nil {
				return
			}
			release()
			if got.flags&flagServerPush != 0 {
				counts <- len(got.payload)
			}
		}
	}()
	return counts
}

// stalled is a client that stops reading: once its socket buffers are full,
// every write to it waits out write_timeout and fails.
func stalled(t *testing.T, server *TCPServer, token string) net.Conn {
	t.Helper()
	connection := dialAuthenticated(t, server, token)
	_ = connection.(*net.TCPConn).SetReadBuffer(4 << 10)
	return connection
}

// fillUntilOneFails pushes filler to player 7 until a push to one of its
// connections has failed: the push reports it, or a connection is gone.
func fillUntilOneFails(t *testing.T, runtime *TCPRuntime) (pushes int, err error) {
	t.Helper()
	before := runtime.ActiveSessions(7)
	filler := bytes.Repeat([]byte{1}, 512<<10)
	for pushes < 128 {
		err = runtime.PushPlayer(context.Background(), 7, pushTestMessageID, filler)
		if err != nil {
			return pushes, err
		}
		pushes++
		if runtime.ActiveSessions(7) < before {
			return pushes, nil
		}
	}
	t.Fatal("64MiB of pushes never failed a write to the stalled connection")
	return pushes, nil
}

func waitClosed(t *testing.T, closed <-chan TCPSessionClosed, want ...string) {
	t.Helper()
	seen := map[string]int{}
	deadline := time.After(2 * time.Second)
	for received := 0; received < len(want); received++ {
		select {
		case event := <-closed:
			seen[event.SessionID]++
		case <-deadline:
			t.Fatalf("tcpSession closes seen %v, want one each for %v", seen, want)
		}
	}
	select {
	case event := <-closed:
		seen[event.SessionID]++
	case <-time.After(100 * time.Millisecond):
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Errorf("tcpSession %s closed %d times, want once (seen %v)", id, seen[id], seen)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("tcpSession closes seen %v, want only %v", seen, want)
	}
}

// push 发往玩家的每个连接。收不下的那个（停止读取的旧连接）立即关闭并注销；push 对收下的连接算成功；关闭走 socket 自己的
// 拆除流程，槽位、按 IP 计数与 tcpSession 关闭事件恰好释放一次。否则整次 push 失败、坏连接留到 idle_timeout，之后每次
// push 同样失败，调用方分不清死玩家与带着死旧连接的活玩家。
func TestAConnectionThatCannotTakeAPushIsClosedAndTheOthersKeepIt(t *testing.T) {
	server, runtime, closed := pushServer(t)
	live := dialAuthenticated(t, server, "live")
	received := readPushes(t, server, live)
	stalled(t, server, "stalled")
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)

	pushes, err := fillUntilOneFails(t, runtime)
	if err != nil {
		t.Errorf("a push the live connection took was reported as failed: %v", err)
	}
	if got := runtime.ActiveSessions(7); got != 1 {
		t.Fatalf("after the stalled connection's write failed: active sessions = %d, want 1 (only the live one)", got)
	}
	waitClosed(t, closed, "stalled")
	waitReleased(t, server, 1)

	if err := runtime.PushPlayer(context.Background(), 7, pushTestMessageID, []byte("after")); err != nil {
		t.Fatalf("push to the remaining live connection: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for got := 0; got < pushes+1; got++ {
		select {
		case <-received:
		case <-deadline:
			t.Fatalf("the live connection received %d of %d pushes", got, pushes+1)
		}
	}
}

// 一条停止读取的连接只能拖住它自己。场景 Sync Flush、Lockstep Tick 都是按会话串行推送、ctx 不带截止的循环：
// 对停读连接的推送若要等满 write_timeout，排在它后面的每个正常连接每轮都晚到 write_timeout。推送只能进入该连接的
// 有界出站队列，收不下就关闭那一条，正常连接的帧照常按轮到达。
func TestAStalledConnectionDoesNotHoldPushesToOtherConnections(t *testing.T) {
	const writeTimeout = 30 * time.Second
	server, runtime, closed := pushServer(t, func(c *TCPConfig) { c.WriteTimeout = writeTimeout })
	live := dialAuthenticated(t, server, "live")
	received := readPushes(t, server, live)
	stalled(t, server, "stalled")
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)

	// 128 × 512KiB 远超停读连接的 socket 缓冲；每轮像 Flush 一样先推停读连接，再推正常连接。
	const rounds = 128
	filler := bytes.Repeat([]byte{1}, 512<<10)
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for range rounds {
			_ = runtime.PushSyncSession(context.Background(), "stalled", pushTestMessageID, filler)
			_ = runtime.PushSyncSession(context.Background(), "live", pushTestMessageID, []byte("frame"))
		}
	}()
	deadline := time.After(3 * time.Second)
	for got := 0; got < rounds; got++ {
		select {
		case <-received:
		case <-deadline:
			t.Fatalf("the live connection received %d of %d sync frames in 3s: its pushes waited behind the stalled connection's write_timeout (%v)", got, rounds, writeTimeout)
		}
	}
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("the flush loop is still blocked after the live connection got every frame")
	}
	waitClosed(t, closed, "stalled")
	if got := runtime.ActiveSessions(7); got != 1 {
		t.Fatalf("active sessions = %d, want 1 (the stalled connection is closed)", got)
	}
}

// orderedPushServer 的 handler 收到请求时先从 pushers 个 goroutine 并发推送给请求所在会话，等它们都准入后再回包。
func orderedPushServer(t *testing.T, pushers int) (*TCPServer, *TCPRuntime) {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	runtime := NewTCPRuntime(func(_ uint32, value any) ([]byte, error) { return value.([]byte), nil }, config.LoginTimeout)
	dispatch := func(ctx context.Context, session Session, _, seq uint32, _ wire.PayloadKind, _ []byte) (any, error) {
		sessionID := session.Principal().SessionID
		var producers sync.WaitGroup
		for range pushers {
			producers.Go(func() {
				if err := runtime.PushSession(ctx, sessionID, pushTestMessageID, []byte("push")); err != nil {
					t.Error(err)
				}
			})
		}
		producers.Wait()
		return &TCPResponse{MessageID: stuckResponseID, Sequence: seq, Payload: []byte("reply")}, nil
	}
	server, err := NewTCPServer(config, dispatch, AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (Principal, error) {
		return Principal{PlayerID: 7, SessionID: token}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	return server, runtime
}

// 出站队列不改变同一连接上的顺序契约：handler 里先发的推送先于它的回包写出；多个生产者并发推送时，
// 客户端看到的推送序号就是写出顺序（1、2、3…连续）；回包写出后才读下一个请求，客户端半关闭写端后仍能收到最后一个回包。
func TestOutboundQueueKeepsPushReplyOrderAndPushSequences(t *testing.T) {
	const pushers = 8
	server, runtime := orderedPushServer(t, pushers)
	connection := dialAuthenticated(t, server, "ordered")
	waitRegistered(t, runtime, 7, 1)
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	nextPush := uint32(1)
	const requests = 4
	for request := uint32(2); request < 2+requests; request++ {
		if err := client.writeFrame(context.Background(), 0, stuckRequestID, request, []byte("req")); err != nil {
			t.Fatal(err)
		}
		if request == 1+requests {
			_ = connection.(*net.TCPConn).CloseWrite()
		}
		for pushes := 0; pushes <= pushers; pushes++ {
			_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, release, err := server.readFrame(connection)
			if err != nil {
				t.Fatalf("request %d: frame %d: %v", request, pushes, err)
			}
			release()
			if pushes < pushers {
				if got.flags != flagServerPush || got.sequence != nextPush {
					t.Fatalf("request %d: frame %d = flags %d seq %d, want push seq %d before the reply", request, pushes, got.flags, got.sequence, nextPush)
				}
				nextPush++
				continue
			}
			if got.flags != 0 || got.messageID != stuckResponseID || got.sequence != request {
				t.Fatalf("request %d: after its pushes got flags %d id %d seq %d, want the reply", request, got.flags, got.messageID, got.sequence)
			}
		}
	}
}

// pipeSession 在 net.Pipe 上建一个内嵌接入会话：对端不读时每次写都卡住，writeLoop 停在哪一帧由测试决定。
func pipeSession(t *testing.T, writeTimeout time.Duration) (*tcpSession, net.Conn) {
	t.Helper()
	client, peer := net.Pipe()
	config := DefaultTCPConfig()
	config.WriteTimeout = writeTimeout
	connectionCtx, cancel := context.WithCancelCause(context.Background())
	session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 7, SessionID: "pipe"}, config)
	t.Cleanup(func() {
		_ = session.Close(ErrSessionClosed)
		<-session.writerDone
		_ = peer.Close()
	})
	return session, peer
}

// waitOutbound 等到会话的出站状态满足 ready（在 queueMu 下判断）。
func waitOutbound(t *testing.T, session *tcpSession, what string, ready func(writing bool, queued int) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.queueMu.Lock()
		ok := ready(session.writing, len(session.queue))
		session.queueMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbound state never reached: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitWriterDone(t *testing.T, session *tcpSession) {
	t.Helper()
	select {
	case <-session.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("writeLoop did not exit after the connection closed")
	}
}

// 帧数上限管的是“排队 + 在途”：writeLoop 一次取走整批再逐帧写，取走但还没写出的帧仍占这条连接的出站额度
// （与字节记账同一口径，写出后才扣减）。对端停读让 writeLoop 卡在一整批 1024 帧的第一帧上，此时再入队的
// 第一帧（合计第 1025 帧）就要被拒绝并关闭连接；只数 len(queue) 时还能再收 1024 帧，停读连接的驻留翻倍。
func TestOutboundFrameLimitCountsTheBatchBeingWritten(t *testing.T) {
	session, peer := pipeSession(t, 30*time.Second)
	ctx := context.Background()
	// 读循环正在直写回包（对端还没读），推送只能排队，writeLoop 见有人在写不取。
	replied := make(chan error, 1)
	go func() { replied <- session.writeQueued(ctx, 0, stuckResponseID, 1, []byte("reply")) }()
	waitOutbound(t, session, "the read loop writing its reply directly", func(writing bool, _ int) bool { return writing })
	for frame := range outboundQueueFrames {
		if err := session.pushKind(ctx, pushTestMessageID, []byte{byte(frame)}, 0); err != nil {
			t.Fatalf("frame %d of %d refused while the queue had room: %v", frame+1, outboundQueueFrames, err)
		}
	}
	// 对端读走回包：直写结束，writeLoop 取走全部 1024 帧，卡在第一帧上（对端不再读）。
	if _, err := io.ReadFull(peer, make([]byte, headerSize+len("reply"))); err != nil {
		t.Fatal(err)
	}
	if err := <-replied; err != nil {
		t.Fatal(err)
	}
	waitOutbound(t, session, "writeLoop holding the whole batch", func(writing bool, queued int) bool { return writing && queued == 0 })

	accepted := 0
	var refused error
	for accepted <= outboundQueueFrames {
		if refused = session.pushKind(ctx, pushTestMessageID, []byte("more"), 0); refused != nil {
			break
		}
		accepted++
	}
	if accepted != 0 {
		t.Fatalf("with %d frames taken by the blocked writeLoop, %d more were admitted (frame %d in total was the first refused): queued + in flight exceeds the %d-frame limit",
			outboundQueueFrames, accepted, outboundQueueFrames+accepted+1, outboundQueueFrames)
	}
	if !errors.Is(refused, errConnectionBroken) || !errors.Is(refused, errOutboundQueueFull) {
		t.Fatalf("frame %d = %v, want errConnectionBroken wrapping errOutboundQueueFull", outboundQueueFrames+1, refused)
	}
	if !session.closed.Load() {
		t.Fatal("the connection over its frame limit is still open")
	}
	waitWriterDone(t, session)
}

// writeLoop 因 write_timeout 写不出一帧就关闭连接：排在后面等写出的回包（读循环在等它）得到 errConnectionBroken，
// writeLoop 退出（writerDone 关闭），之后的推送也被拒绝。
func TestAWriteTimeoutInTheWriteLoopFailsTheReplyQueuedBehindIt(t *testing.T) {
	const writeTimeout = 300 * time.Millisecond
	session, _ := pipeSession(t, writeTimeout)
	ctx := context.Background()
	if err := session.pushKind(ctx, pushTestMessageID, []byte("stuck"), 0); err != nil {
		t.Fatal(err)
	}
	waitOutbound(t, session, "writeLoop writing the stuck push", func(writing bool, queued int) bool { return writing && queued == 0 })
	replied := make(chan error, 1)
	go func() { replied <- session.writeQueued(ctx, 0, stuckResponseID, 1, []byte("reply")) }()
	waitOutbound(t, session, "the reply queued behind the stuck push", func(_ bool, queued int) bool { return queued == 1 })
	if session.closed.Load() {
		t.Fatalf("write_timeout (%v) ran out before the reply was queued; the test did not reach the waiting path", writeTimeout)
	}
	select {
	case err := <-replied:
		if !errors.Is(err, errConnectionBroken) {
			t.Fatalf("reply queued behind a timed-out write = %v, want errConnectionBroken", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reply queued behind a timed-out write is still waiting")
	}
	waitWriterDone(t, session)
	if err := session.Close(nil); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("close reason = %v, want the write timeout", err)
	}
	if err := session.pushKind(ctx, pushTestMessageID, []byte("late"), 0); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("push after the write timeout closed the connection = %v, want errConnectionBroken", err)
	}
}

// Close 与多个并发 Push、一个等写出的回包竞争：每轮都要在期限内全部返回（无死锁），
// writeLoop 退出；多轮之后 goroutine 数回到起点（没有写协程或等待方泄漏）。一半轮次对端在读，一半停读。
func TestClosingASessionDuringConcurrentPushesLeavesNoGoroutine(t *testing.T) {
	base := runtime.NumGoroutine()
	const rounds, pushers, pushesEach = 50, 8, 64
	for round := range rounds {
		client, peer := net.Pipe()
		config := DefaultTCPConfig()
		config.WriteTimeout = time.Second
		connectionCtx, cancel := context.WithCancelCause(context.Background())
		session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 7, SessionID: "close-race"}, config)
		drained := make(chan struct{})
		if round%2 == 0 {
			go func() { defer close(drained); _, _ = io.Copy(io.Discard, peer) }()
		} else {
			close(drained)
		}
		start := make(chan struct{})
		var producers sync.WaitGroup
		for range pushers {
			producers.Go(func() {
				<-start
				for range pushesEach {
					if session.pushKind(context.Background(), pushTestMessageID, []byte("frame"), 0) != nil {
						return
					}
				}
			})
		}
		producers.Go(func() {
			<-start
			_ = session.writeQueued(context.Background(), 0, stuckResponseID, 1, []byte("reply"))
		})
		close(start)
		if round%3 == 0 {
			time.Sleep(time.Millisecond)
		}
		_ = session.Close(ErrSessionClosed)
		returned := make(chan struct{})
		go func() { producers.Wait(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: pushes or the waiting reply did not return after Close", round)
		}
		waitWriterDone(t, session)
		_ = peer.Close()
		<-drained
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after %d close rounds, want at most %d", runtime.NumGoroutine(), rounds, base)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// When no connection takes the frame the player is unreachable: the push
// fails and every connection is closed, so nothing stays registered for a
// player nobody can reach.
func TestAPushNoConnectionTakesFailsAndClosesThemAll(t *testing.T) {
	server, runtime, closed := pushServer(t)
	stalled(t, server, "first")
	stalled(t, server, "second")
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)
	var err error
	for range 128 {
		if err = runtime.PushPlayer(context.Background(), 7, pushTestMessageID, bytes.Repeat([]byte{1}, 512<<10)); err != nil || runtime.ActiveSessions(7) == 0 {
			break
		}
	}
	if err == nil {
		t.Fatal("a push no connection took was reported as delivered")
	}
	for range 128 {
		if runtime.ActiveSessions(7) == 0 {
			break
		}
		_ = runtime.PushPlayer(context.Background(), 7, pushTestMessageID, bytes.Repeat([]byte{1}, 512<<10))
	}
	if got := runtime.ActiveSessions(7); got != 0 {
		t.Fatalf("active sessions = %d after every write failed, want 0", got)
	}
	waitClosed(t, closed, "first", "second")
	waitReleased(t, server, 0)
	if err := runtime.PushPlayer(context.Background(), 7, pushTestMessageID, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("push to a player with no connection = %v, want ErrSessionNotFound", err)
	}
}

// Only a WRITE failure says something about a connection. A push the caller
// cancelled before anything was written, or a payload over the limit, is not
// the connection's fault and must not close a live one.
func TestAPushThatFailsBeforeWritingClosesNoConnection(t *testing.T) {
	server, runtime, closed := pushServer(t)
	first := dialAuthenticated(t, server, "first")
	second := dialAuthenticated(t, server, "second")
	readPushes(t, server, first)
	readPushes(t, server, second)
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.PushPlayer(cancelled, 7, pushTestMessageID, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled push = %v, want context.Canceled", err)
	}
	if err := runtime.PushPlayer(context.Background(), 7, pushTestMessageID, bytes.Repeat([]byte{1}, int(server.config.MaxPayloadBytes)+1)); !errors.Is(err, errInvalidFrame) {
		t.Fatalf("oversized push = %v, want errInvalidFrame", err)
	}
	if got := runtime.ActiveSessions(7); got != 2 {
		t.Fatalf("a push that wrote nothing closed a connection: active sessions = %d, want 2", got)
	}
	select {
	case event := <-closed:
		t.Fatalf("a push that wrote nothing closed tcpSession %s", event.SessionID)
	case <-time.After(100 * time.Millisecond):
	}
	if err := runtime.PushPlayer(context.Background(), 7, pushTestMessageID, []byte("ok")); err != nil {
		t.Fatalf("push after the refused ones: %v", err)
	}
}

// PushSession is the same write to one connection and follows the same rule.
func TestASessionPushThatCannotBeWrittenClosesThatSession(t *testing.T) {
	server, runtime, closed := pushServer(t)
	live := dialAuthenticated(t, server, "live")
	readPushes(t, server, live)
	stalled(t, server, "stalled")
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)
	var err error
	for range 128 {
		if err = runtime.PushSession(context.Background(), "stalled", pushTestMessageID, bytes.Repeat([]byte{1}, 512<<10)); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("pushes to a stalled tcpSession never failed")
	}
	if got := runtime.ActiveSessions(7); got != 1 {
		t.Fatalf("active sessions = %d after the tcpSession's write failed, want 1", got)
	}
	waitClosed(t, closed, "stalled")
	waitReleased(t, server, 1)
}

// CloseSessions 报告它关闭了多少个会话：tcpSession.Close 返回关闭原因（从不为 nil，nil 原因变成 ErrSessionClosed），
// 不能按“返回 nil 的调用”计数；已被其他路径关闭的 tcpSession 不重复计数。
func TestCloseSessionsCountsTheSessionsItClosed(t *testing.T) {
	server, runtime, closed := pushServer(t)
	dialAuthenticated(t, server, "first")
	dialAuthenticated(t, server, "second")
	waitReleased(t, server, 2)
	// Both registered, or CloseSessions may not see one.
	waitRegistered(t, runtime, 7, 2)

	if got := runtime.CloseSessions(7, errors.New("fenced")); got != 2 {
		t.Errorf("CloseSessions closed the player's 2 open sessions and reported %d", got)
	}
	// Both are closed now, whether or not their teardown has unregistered them yet.
	if got := runtime.CloseSessions(7, errors.New("fenced again")); got != 0 {
		t.Errorf("CloseSessions on sessions already closed reported %d, want 0", got)
	}
	waitClosed(t, closed, "first", "second")
	waitReleased(t, server, 0)
	if got := runtime.CloseSessions(7, nil); got != 0 {
		t.Errorf("CloseSessions for a player with no tcpSession reported %d, want 0", got)
	}
}

// scriptedConn is a net.Conn whose writes report what the test says: how many
// bytes went out and which error came back.
type scriptedConn struct {
	net.Conn
	mu       sync.Mutex
	written  int
	err      error
	deadline time.Time
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return len(p), nil
	}
	n := min(c.written, len(p))
	c.written -= n
	return n, c.err
}
func (c *scriptedConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
func (c *scriptedConn) Close() error { return nil }

// 调用方自己的期限在一个字节都没写出之前切断的写，流仍然对齐：它像 context 已结束的 push 一样被拒绝，不关闭连接。
// 被 write_timeout 停下的写、写了半帧的写、对端重置的写是连接故障，照样关闭连接。
func TestAWriteCutOffByTheCallerDeadlineBeforeAnyByteIsARefusal(t *testing.T) {
	callerDeadline := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 10*time.Millisecond)
	}
	for _, tc := range []struct {
		name    string
		written int
		err     error
		ctx     func() (context.Context, context.CancelFunc)
		broken  bool
	}{
		{"caller deadline, nothing written", 0, os.ErrDeadlineExceeded, callerDeadline, false},
		{"write_timeout, nothing written", 0, os.ErrDeadlineExceeded, func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, true},
		{"caller deadline, part of the frame written", 5, os.ErrDeadlineExceeded, callerDeadline, true},
		{"caller deadline, peer reset", 0, syscall.ECONNRESET, callerDeadline, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connection := &scriptedConn{written: tc.written, err: tc.err}
			current := &tcpSession{connection: connection, writeTimeout: time.Second}
			ctx, cancel := tc.ctx()
			defer cancel()
			err := current.writeFrame(ctx, flagServerPush, pushTestMessageID, 1, []byte("payload"))
			if err == nil {
				t.Fatal("a failed write reported success")
			}
			if got := errors.Is(err, errConnectionBroken); got != tc.broken {
				t.Fatalf("errConnectionBroken=%v, want %v: %v", got, tc.broken, err)
			}
			if !tc.broken && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a push the caller's deadline refused = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

// The same classification on a real in-memory pipe: nobody reads, so the
// write blocks and the earlier deadline decides.
func TestAPipeWriteRefusedByTheCallerDeadlineIsNotAConnectionFailure(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	current := &tcpSession{connection: client, writeTimeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := current.writeFrame(ctx, flagServerPush, pushTestMessageID, 1, []byte("blocked")); err == nil || errors.Is(err, errConnectionBroken) {
		t.Fatalf("nothing written before the caller's deadline = %v, want a refusal without errConnectionBroken", err)
	}
	stalled := &tcpSession{connection: client, writeTimeout: 20 * time.Millisecond}
	if err := stalled.writeFrame(context.Background(), flagServerPush, pushTestMessageID, 2, []byte("blocked")); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("nothing written within write_timeout = %v, want errConnectionBroken", err)
	}
	go func() { buffer := make([]byte, 5); _, _ = peer.Read(buffer) }()
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := current.writeFrame(ctx, flagServerPush, pushTestMessageID, 3, []byte("partly")); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("a frame cut off after 5 bytes = %v, want errConnectionBroken", err)
	}
}

// 调用方 context 几乎立刻到期（1µs）的 push 被拒绝，但不能关闭玩家两个健康、在读的连接；之后的正常 push 仍到达两个连接。
func TestAnExpiringCallerDeadlineClosesNoHealthyConnection(t *testing.T) {
	server, runtime, closed := pushServer(t)
	var received []<-chan int
	for _, token := range []string{"a", "b"} {
		connection := dialAuthenticated(t, server, token)
		received = append(received, readPushes(t, server, connection))
	}
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)
	failures := 0
	var broken error
	// 连推的帧数留在每连接出站上限（排队 + 在途 1024 帧）以内：一次突发超过上限、生产快过逐帧写 socket 的连接
	// 按设计会被当作慢连接关闭，那不是这里要测的“调用方截止导致误关”。
	for range outboundQueueFrames - 24 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
		if err := runtime.PushPlayer(ctx, 7, pushTestMessageID, []byte("x")); err != nil {
			failures++
			if broken == nil && errors.Is(err, errConnectionBroken) {
				broken = err
			}
		}
		cancel()
		if runtime.ActiveSessions(7) != 2 {
			break
		}
	}
	if broken != nil {
		t.Errorf("an expired caller deadline was reported as a broken connection: %v", broken)
	}
	if got := runtime.ActiveSessions(7); got != 2 {
		t.Fatalf("active sessions = %d after %d refused pushes, want 2", got, failures)
	}
	select {
	case event := <-closed:
		t.Fatalf("healthy reading connection %s was closed by a push that expired before writing", event.SessionID)
	case <-time.After(200 * time.Millisecond):
	}
	if err := runtime.PushPlayer(context.Background(), 7, pushTestMessageID, []byte("after")); err != nil {
		t.Fatalf("push after the refused ones: %v", err)
	}
	for i, counts := range received {
		if !receivedPayloadOfSize(counts, len("after"), 2*time.Second) {
			t.Fatalf("connection %d never received the push after the refused ones", i)
		}
	}
}

func receivedPayloadOfSize(counts <-chan int, size int, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case got, ok := <-counts:
			if !ok {
				return false
			}
			if got == size {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestServerRejectsInvalidConstruction(t *testing.T) {
	if _, err := NewTCPServer(TCPConfig{}, nil, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return Principal{}, nil
	})); err == nil {
		t.Fatal("nil runtime unexpectedly accepted")
	}
	if _, err := NewTCPServer(TCPConfig{MaxConnections: 1, MaxPayloadBytes: 1}, emptyTCPDispatch, nil); err == nil {
		t.Fatal("nil authenticator unexpectedly accepted")
	}
}

// stalledAuthServer starts a server whose authenticator ignores its context:
// it returns only after release is closed, and then still works for a while
// before it does. entered receives once per handshake that reached it.
func stalledAuthServer(t *testing.T) (*TCPServer, chan struct{}, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	config.HandshakeTimeout = 5 * time.Second
	release, entered, returned := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		entered <- struct{}{}
		<-release
		time.Sleep(100 * time.Millisecond)
		close(returned)
		return Principal{}, ErrUnauthenticated
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	server.mu.RLock()
	addr := server.listener.Addr().String()
	server.mu.RUnlock()
	connection, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	if err := client.writeFrame(context.Background(), 0, 0, 1, []byte("token")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handshake never reached the authenticator")
	}
	return server, release, entered, returned
}

// Stop 的 context 先于一个不看 context 的认证器结束时，之后的 Stop 不能报告一次没有发生的排空：重试等到那个 goroutine 退出才成功。
func TestAStopRetryWaitsForAConnectionTheFirstStopCouldNotDrain(t *testing.T) {
	server, release, _, returned := stalledAuthServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	first := server.Stop(ctx)
	cancel()
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Stop = %v, want the deadline", first)
	}
	close(release)
	retry, cancelRetry := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRetry()
	if err := server.Stop(retry); err != nil {
		t.Fatalf("retry Stop = %v", err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("retry Stop reported success while the authenticator was still running")
	}
}

// 阻塞的 tcpSession 关闭订阅者卡住的是分发器，不是 Stop 的预算；它返回之后重试完成。
func TestABlockingCloseSubscriberDoesNotHoldStopPastItsContext(t *testing.T) {
	runtime := &TCPRuntime{}
	block, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	runtime.OnSessionClosed(func(TCPSessionClosed) { once.Do(func() { close(entered) }); <-block })
	runtime.publishClosed(TCPSessionClosed{PlayerID: 1, SessionID: "s"})
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.stopLifecycleContext(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StopWithContext = %v, want the deadline", err)
		}
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("StopWithContext ignored its context while a subscriber blocked")
	}
	close(block)
	if err := runtime.stopLifecycleContext(context.Background()); err != nil {
		t.Fatalf("retry StopWithContext = %v", err)
	}
}

type tcpTestContext struct{ base context.Context }

func (ctx *tcpTestContext) Context() context.Context { return ctx.base }
func emptyTCPDispatch(context.Context, Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
	return nil, nil
}

// benchmarkTCPServer 起一个不限流的回显服务：请求原样作为应答负载返回。限流器保留（速率给到上限），读循环的令牌检查
// 仍在测量路径上。
func benchmarkTCPServer(b *testing.B) *TCPServer {
	b.Helper()
	// 监听日志会插进基准结果行，benchstat 无法解析；基准期间丢弃默认日志。
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(previous) })
	return dispatchServer(b, "5s", func(_ *tcpTestContext, payload string) (string, error) {
		return payload, nil
	}, func(config *TCPConfig) {
		config.RequestRate, config.RequestBurst = 100000, 100000
		config.MaxConnectionsPerIP = config.MaxConnections
	})
}

// BenchmarkTCPRequestRoundTrip 覆盖 serveConnection 的读循环：一个已认证连接上逐个发送请求并读回应答
// （读帧、序号与限流检查、分发、应答写回）。
func BenchmarkTCPRequestRoundTrip(b *testing.B) {
	server := benchmarkTCPServer(b)
	connection := dialAuthenticated(b, server, "bench-session")
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	payload := []byte("ping")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.writeFrame(ctx, 0, stuckRequestID, uint32(i+2), payload); err != nil {
			b.Fatal(err)
		}
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		response, release, err := server.readFrame(connection)
		if err != nil {
			b.Fatal(err)
		}
		if response.messageID != stuckResponseID {
			b.Fatalf("unexpected response %+v", response)
		}
		release()
	}
}

// BenchmarkTCPHandshake 覆盖 serveConnection 的握手段：每轮新建连接、发送认证帧、读回 ack 后关闭。客户端以 RST 关闭
// （linger 0），避免大量 TIME_WAIT 耗尽本机端口。
func BenchmarkTCPHandshake(b *testing.B) {
	server := benchmarkTCPServer(b)
	server.mu.RLock()
	addr := server.listener.Addr().String()
	server.mu.RUnlock()
	token := []byte("bench-session")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		connection, err := net.Dial("tcp", addr)
		if err != nil {
			b.Fatal(err)
		}
		client := &tcpSession{connection: connection, writeTimeout: time.Second}
		if err := client.writeFrame(ctx, 0, 0, 1, token); err != nil {
			b.Fatal(err)
		}
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		ack, release, err := server.readFrame(connection)
		if err != nil {
			b.Fatalf("authentication ack: %v", err)
		}
		release()
		if ack.messageID != 0 || ack.sequence != 1 {
			b.Fatalf("unexpected ack %+v", ack)
		}
		if tcpConnection, ok := connection.(*net.TCPConn); ok {
			_ = tcpConnection.SetLinger(0)
		}
		_ = connection.Close()
	}
}
