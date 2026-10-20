// TCP regression extraction source
package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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
	config.DispatchTimeout, config.LoginTimeout, config.OutboundResidentBytes = 0, 0, 0
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return Principal{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if server.config.DispatchTimeout != defaultDispatchTimeout || server.config.LoginTimeout != defaultLoginTimeout {
		t.Fatalf("request budgets: %+v", server.config)
	}
	if server.config.OutboundResidentBytes != defaultOutboundResidentBytes || server.outbound.limit != defaultOutboundResidentBytes {
		t.Fatalf("outbound resident budget = %d (server %d), want the default %d", server.config.OutboundResidentBytes, server.outbound.limit, defaultOutboundResidentBytes)
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

// lockedBuffer 收集服务端读循环 goroutine 写的日志。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 超过每连接令牌桶即断开连接（心跳也计入）；断开前留一条 Debug 日志点名玩家与限额，
// 客户端只看到 EOF 时运维能从日志找到原因。每条连接最多一行、只记 Debug：客户端造成的断开不能刷日志。
func TestPerConnectionTokenBucketIncludesHeartbeats(t *testing.T) {
	logs := &lockedBuffer{}
	previous := slog.Default()
	previousOutput := log.Default().Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous); log.SetOutput(previousOutput) })
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			// 日志缓冲在子测试之间共用：只数本子测试新增的部分，单独 -run 某个子测试时判定不变。
			before := len(logs.String())
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
			written := logs.String()[before:]
			limited := strings.Count(written, "player tcp rate limit exceeded")
			if enabled && (limited != 2 || !strings.Contains(written, "player_id=7") || !strings.Contains(written, "request_burst=2")) {
				t.Fatalf("rate-limited close logged %d times, want 2 naming player_id and request_burst:\n%s", limited, written)
			}
			if !enabled && limited != 0 {
				t.Fatalf("disabled limiter logged %d rate-limit closes, want 0:\n%s", limited, written)
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
	current := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 1, SessionID: "tcpSession"}, server.config, server.outbound)
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
func readPushes(t *testing.T, server *TCPServer, connection net.Conn, progress ...*atomic.Int64) <-chan int {
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
				if len(progress) != 0 {
					progress[0].Add(1)
				}
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
func fillUntilOneFails(t *testing.T, runtime *TCPRuntime, liveProgress *atomic.Int64) (pushes int, err error) {
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
		// 本用例的健康连接持续消费；不要用调度器尚未运行读协程的突发把它也塞满。
		deadline := time.Now().Add(2 * time.Second)
		for liveProgress.Load() < int64(pushes) {
			if time.Now().After(deadline) {
				t.Fatal("the live connection did not consume its accepted push")
			}
			time.Sleep(time.Millisecond)
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
	var progress atomic.Int64
	received := readPushes(t, server, live, &progress)
	stalled(t, server, "stalled")
	waitReleased(t, server, 2)
	waitRegistered(t, runtime, 7, 2)

	pushes, err := fillUntilOneFails(t, runtime, &progress)
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
	session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 7, SessionID: "pipe"}, config, nil)
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

// 等写出的推送（Lockstep 追帧页）在 ctx 结束时还在排队就撤回：不再写出、归还出站额度、不关闭连接，
// 之后的推送序号不留空洞；对端恢复读取后，下一次等写出的推送在真正写出时才返回 nil。
func TestAPushWaitingForItsWriteIsWithdrawnWhenItsContextEnds(t *testing.T) {
	session, peer := pipeSession(t, 30*time.Second)
	if err := session.pushKind(context.Background(), pushTestMessageID, []byte("stuck"), flagLockstep); err != nil {
		t.Fatal(err)
	}
	waitOutbound(t, session, "writeLoop writing the stuck push", func(writing bool, queued int) bool { return writing && queued == 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := session.pushWritten(ctx, pushTestMessageID, []byte("page"), flagLockstep); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a page that could not be written before its deadline = %v, want context.DeadlineExceeded", err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the page waited %v past its 100ms deadline", waited)
	}
	session.queueMu.Lock()
	queued, frames, queuedBytes := len(session.queue), session.queuedFrames, session.queuedBytes
	session.queueMu.Unlock()
	if queued != 0 || frames != 1 || queuedBytes != int64(len("stuck"))+headerSize {
		t.Fatalf("after withdrawing the page: queue %d, frames %d, bytes %d; want only the in-flight push counted", queued, frames, queuedBytes)
	}
	if session.closed.Load() {
		t.Fatal("a page whose wait ended closed the connection")
	}

	written := make(chan error, 1)
	go func() {
		written <- session.pushWritten(context.Background(), pushTestMessageID, []byte("next"), flagLockstep)
	}()
	waitOutbound(t, session, "the next page queued", func(_ bool, queued int) bool { return queued == 1 })
	select {
	case err := <-written:
		t.Fatalf("a page still queued behind a stuck write returned %v before it was written", err)
	default:
	}
	for _, want := range []struct {
		payload  string
		sequence uint32
	}{{"stuck", 1}, {"next", 2}} {
		got, err := wire.Read(peer, 0)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Payload) != want.payload || got.Seq != want.sequence || got.Flags != flagServerPush|flagLockstep {
			t.Fatalf("peer read %q seq %d flags %d, want %q seq %d (the withdrawn page is never written)", got.Payload, got.Seq, got.Flags, want.payload, want.sequence)
		}
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("written page = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the page the peer read never reported written")
	}
}

// 等写出的推送在连接关闭时立即返回 errConnectionBroken，不等 write_timeout 或自己的 ctx。
func TestAPushWaitingForItsWriteFailsAtOnceWhenTheConnectionCloses(t *testing.T) {
	session, _ := pipeSession(t, 30*time.Second)
	if err := session.pushKind(context.Background(), pushTestMessageID, []byte("stuck"), flagLockstep); err != nil {
		t.Fatal(err)
	}
	waitOutbound(t, session, "writeLoop writing the stuck push", func(writing bool, queued int) bool { return writing && queued == 0 })
	written := make(chan error, 1)
	go func() {
		written <- session.pushWritten(context.Background(), pushTestMessageID, []byte("page"), flagLockstep)
	}()
	waitOutbound(t, session, "the page queued behind the stuck push", func(_ bool, queued int) bool { return queued == 1 })
	_ = session.Close(ErrSessionClosed)
	select {
	case err := <-written:
		if !errors.Is(err, errConnectionBroken) {
			t.Fatalf("page on a closed connection = %v, want errConnectionBroken", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a page waiting for its write is still waiting after the connection closed")
	}
	if err := session.pushWritten(context.Background(), pushTestMessageID, []byte("late"), flagLockstep); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("page after close = %v, want errConnectionBroken", err)
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
		session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 7, SessionID: "close-race"}, config, nil)
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

// budgetPipeSession 在 net.Pipe 上建一个登记到 server 的内嵌接入会话，共用 server 的全进程出站驻留预算；
// 对端不读时 writeLoop 卡在第一帧上（net.Pipe 没有内核缓冲），后续推送全部留在队列里。
func budgetPipeSession(t *testing.T, server *TCPServer, playerID int64) (*tcpSession, net.Conn) {
	t.Helper()
	client, peer := net.Pipe()
	connectionCtx, cancel := context.WithCancelCause(context.Background())
	session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: playerID, SessionID: fmt.Sprintf("budget-%d", playerID)}, server.config, server.outbound)
	server.registerSession(session)
	t.Cleanup(func() {
		_ = session.Close(ErrSessionClosed)
		<-session.writerDone
		server.forgetSession(session)
		_ = peer.Close()
	})
	return session, peer
}

// residentOf 是这些会话“排队 + 在途”的字节合计（每连接各自的记账）。
func residentOf(sessions []*tcpSession) int64 {
	var total int64
	for _, session := range sessions {
		session.queueMu.Lock()
		total += session.queuedBytes
		session.queueMu.Unlock()
	}
	return total
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

// 很多客户端同时停读（恶意连接或网络抖动）时，每连接上限各自都没到，进程却已驻留 连接数 × 每连接上限：
// 全进程出站驻留预算（outbound.resident_bytes）把所有连接“排队 + 在途”的字节合计限在预算内，超出的入队当场被拒
// （ErrResidentBudgetExhausted，不带 errConnectionBroken，连接不关闭），计 player_tcp_outbound_budget_exhausted_total；
// 字节在写出与连接关闭时归还，归还后同一连接又能入队。
// 规模：8 个停读连接各推 64 帧 × 16 KiB ≈ 1 MiB（低于每连接 2 MiB 上限，不会因单连接溢出被关），合计约 8 MiB，预算 1 MiB。
func TestStalledConnectionsTogetherStayWithinTheResidentBudget(t *testing.T) {
	const connections, framesEach, frameBytes = 8, 64, 16 << 10
	const budget = 1 << 20
	config := DefaultTCPConfig()
	config.MaxPayloadBytes = 64 << 10
	config.OutboundResidentBytes = budget
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return Principal{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	sessions := make([]*tcpSession, connections)
	peers := make([]net.Conn, connections)
	for index := range sessions {
		sessions[index], peers[index] = budgetPipeSession(t, server, int64(index+1))
	}
	before := counterValue("player_tcp_outbound_budget_exhausted_total")
	ctx := context.Background()
	payload := make([]byte, frameBytes)
	refused := 0
	for range framesEach {
		for _, session := range sessions {
			err := server.pushPlayerKind(ctx, session.principal.PlayerID, pushTestMessageID, payload, flagSync)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrResidentBudgetExhausted) || errors.Is(err, errConnectionBroken) {
				t.Fatalf("push refused with %v, want ErrResidentBudgetExhausted without errConnectionBroken", err)
			}
			refused++
		}
	}
	resident := residentOf(sessions)
	if resident > budget || refused == 0 {
		t.Fatalf("%d stalled connections hold %d bytes queued + in flight together, over the %d-byte process budget; %d of %d pushes were refused",
			connections, resident, budget, refused, connections*framesEach)
	}
	if got := server.outbound.resident(); got != resident {
		t.Fatalf("process budget counts %d resident bytes, connections hold %d", got, resident)
	}
	if got := counterValue("player_tcp_outbound_budget_exhausted_total") - before; got != int64(refused) {
		t.Fatalf("player_tcp_outbound_budget_exhausted_total grew by %d, want %d (one per refused enqueue)", got, refused)
	}
	for _, session := range sessions {
		if session.closed.Load() {
			t.Fatalf("connection %s was closed for an exhausted process budget", session.principal.SessionID)
		}
	}

	// 写出归还：前一半连接的客户端开始读，它们的驻留归零、预算随之归还，同一连接又能入队。
	half := connections / 2
	for _, peer := range peers[:half] {
		go func() { _, _ = io.Copy(io.Discard, peer) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for residentOf(sessions[:half]) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the reading connections still hold %d bytes", residentOf(sessions[:half]))
		}
		time.Sleep(time.Millisecond)
	}
	if got, want := server.outbound.resident(), residentOf(sessions[half:]); got != want {
		t.Fatalf("after half the clients read everything the budget counts %d bytes, the stalled half holds %d", got, want)
	}
	if err := server.pushPlayerKind(ctx, sessions[0].principal.PlayerID, pushTestMessageID, payload, flagSync); err != nil {
		t.Fatalf("push after the budget was released by writes = %v", err)
	}
	// 关闭归还：停读的一半关闭后，它们排队与卡在写协程里的字节一次归还。
	for _, session := range sessions[half:] {
		_ = session.Close(ErrSessionClosed)
		waitWriterDone(t, session)
	}
	deadline = time.Now().Add(5 * time.Second)
	for server.outbound.resident() != residentOf(sessions[:half]) || residentOf(sessions[half:]) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after the stalled connections closed the budget counts %d bytes, the open ones hold %d", server.outbound.resident(), residentOf(sessions[:half]))
		}
		time.Sleep(time.Millisecond)
	}
	if err := sessions[half].pushKind(ctx, pushTestMessageID, payload, flagSync); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("push to a closed connection = %v, want errConnectionBroken", err)
	}
}

// 关连接与写协程退出并发时预算恰好归还一次：每轮几条共用一个预算的连接，有的对端在读（写协程边写边扣减），
// 有的停读、写超时很短（写协程在一批中途因写失败退出），同时有并发推送（部分因预算被拒）、等写出后超时撤回的追帧页、
// 读循环回包（只计入不拒）与重复 Close。全部返回、写协程退出后预算必须正好回到 0：重复扣减会变负，少扣会留正数；
// 过程中采样也不得为负。
func TestTheResidentBudgetIsReturnedExactlyOnceWhenClosesRaceTheWriteLoop(t *testing.T) {
	const rounds, connections, pushers, pushesEach = 30, 4, 4, 64
	for round := range rounds {
		budget := &outboundBudget{limit: 96 << 10}
		sessions := make([]*tcpSession, connections)
		peers := make([]net.Conn, connections)
		var readers sync.WaitGroup
		for index := range sessions {
			client, peer := net.Pipe()
			config := DefaultTCPConfig()
			config.WriteTimeout = time.Second
			if index == connections-1 {
				config.WriteTimeout = 20 * time.Millisecond // 停读且写超时短：写协程在一批中途写失败退出
			}
			connectionCtx, cancel := context.WithCancelCause(context.Background())
			sessions[index] = newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: int64(index + 1), SessionID: fmt.Sprintf("race-%d", index)}, config, budget)
			peers[index] = peer
			if index%2 == 0 {
				readers.Go(func() { _, _ = io.Copy(io.Discard, peer) })
			}
		}
		stop := make(chan struct{})
		var negative atomic.Int64
		var sampler sync.WaitGroup
		sampler.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if used := budget.resident(); used < 0 {
					negative.Store(used)
				}
				runtime.Gosched()
			}
		})
		start := make(chan struct{})
		var producers sync.WaitGroup
		for index, session := range sessions {
			for pusher := range pushers {
				producers.Go(func() {
					<-start
					payload := make([]byte, 1<<10*(1+pusher))
					for range pushesEach {
						if err := session.pushKind(context.Background(), pushTestMessageID, payload, flagSync); err != nil && !errors.Is(err, ErrResidentBudgetExhausted) {
							return
						}
					}
				})
			}
			producers.Go(func() {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				_ = session.pushWritten(ctx, pushTestMessageID, make([]byte, 2<<10), flagLockstep)
			})
			producers.Go(func() {
				<-start
				_ = session.writeQueued(context.Background(), 0, stuckResponseID, 1, make([]byte, 512))
			})
			producers.Go(func() {
				<-start
				time.Sleep(time.Duration(index+round%3) * time.Millisecond)
				_ = session.Close(ErrSessionClosed)
				_ = session.Close(ErrSessionClosed)
			})
		}
		close(start)
		returned := make(chan struct{})
		go func() { producers.Wait(); close(returned) }()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: pushes did not return after the connections closed", round)
		}
		for _, session := range sessions {
			waitWriterDone(t, session)
		}
		close(stop)
		sampler.Wait()
		for _, peer := range peers {
			_ = peer.Close()
		}
		readers.Wait()
		if used := negative.Load(); used < 0 {
			t.Fatalf("round %d: the process budget went negative (%d): a frame's bytes were returned twice", round, used)
		}
		if used := budget.resident(); used != 0 {
			t.Fatalf("round %d: %d bytes still counted after every connection closed and its writer exited (want 0: returned twice < 0, never returned > 0)", round, used)
		}
		for _, session := range sessions {
			if err := session.pushKind(context.Background(), pushTestMessageID, []byte("late"), 0); !errors.Is(err, errConnectionBroken) {
				t.Fatalf("round %d: push after the writer exited = %v, want errConnectionBroken", round, err)
			}
		}
		if used := budget.resident(); used != 0 {
			t.Fatalf("round %d: a push refused after close left %d bytes counted", round, used)
		}
	}
}

// 服务器停止时连接 context 先结束：写协程退出并归还驻留，而会话要等读循环返回才被 Close（closed 仍为 false）。
// 这个窗口里到达的推送必须被拒，否则它的字节进了一条再没有写协程的队列，永远不归还。
func TestAPushAfterTheWriterExitedIsRefusedAndNotCounted(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	budget := &outboundBudget{limit: 1 << 20}
	connectionCtx, cancel := context.WithCancelCause(context.Background())
	session := newTCPSession(connectionCtx, cancel, client, Principal{PlayerID: 7, SessionID: "stopping"}, DefaultTCPConfig(), budget)
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	if err := session.pushKind(context.Background(), pushTestMessageID, []byte("written"), 0); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); budget.resident() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the written push is still counted: %d bytes", budget.resident())
		}
		time.Sleep(time.Millisecond)
	}
	cancel(errors.New("server stopping")) // 只结束连接 context，不经 session.close：空闲的写协程随之退出
	waitWriterDone(t, session)
	if session.closed.Load() {
		t.Fatal("the session was closed; the test did not reach the window before the read loop closes it")
	}
	if used := budget.resident(); used != 0 {
		t.Fatalf("after the writer exited the budget counts %d bytes, want 0", used)
	}
	if err := session.pushKind(context.Background(), pushTestMessageID, []byte("late"), 0); !errors.Is(err, errConnectionBroken) {
		t.Fatalf("push after the writer exited = %v, want errConnectionBroken", err)
	}
	if used := budget.resident(); used != 0 {
		t.Fatalf("a push after the writer exited left %d bytes counted that nobody will return", used)
	}
	_ = session.Close(ErrSessionClosed)
}

// 读循环自己的回包与心跳 ACK 计入驻留但不因预算被拒：预算耗尽时正常读的客户端发请求仍得到回包，不被断开；
// 业务异步 Session.Reply 与推送一样受预算拒绝。
func TestReadLoopRepliesAreCountedButNotRefusedByTheResidentBudget(t *testing.T) {
	session, peer := pipeSession(t, 30*time.Second)
	session.budget = &outboundBudget{limit: 4 << 10}
	ctx := context.Background()
	// 填满预算：对端不读，第一帧卡在写协程里，其余排队。
	for session.pushKind(ctx, pushTestMessageID, make([]byte, 1<<10), 0) == nil {
	}
	if err := session.pushKind(ctx, pushTestMessageID, make([]byte, 1<<10), 0); !errors.Is(err, ErrResidentBudgetExhausted) {
		t.Fatalf("push with the budget used up = %v, want ErrResidentBudgetExhausted", err)
	}
	if err := session.Reply(ctx, &TCPResponse{MessageID: stuckResponseID, Sequence: 9, Payload: make([]byte, 1<<10)}); !errors.Is(err, ErrResidentBudgetExhausted) {
		t.Fatalf("business Reply with the budget used up = %v, want ErrResidentBudgetExhausted", err)
	}
	full := session.budget.resident()
	replied := make(chan error, 1)
	go func() { replied <- session.writeQueued(ctx, 0, stuckResponseID, 1, []byte("reply")) }()
	// 回包排在卡住的推送后面：预算已满仍被收下，驻留多出这一帧。
	want := full + headerSize + int64(len("reply"))
	for deadline := time.Now().Add(2 * time.Second); session.budget.resident() != want; {
		if time.Now().After(deadline) {
			t.Fatalf("read-loop reply not counted: resident %d, want %d", session.budget.resident(), want)
		}
		time.Sleep(time.Millisecond)
	}
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	if err := <-replied; err != nil {
		t.Fatalf("read-loop reply with the budget used up = %v, want it written", err)
	}
	if session.closed.Load() {
		t.Fatal("the connection was closed for an exhausted process budget")
	}
	deadline := time.Now().Add(2 * time.Second)
	for session.budget.resident() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after the client read everything the budget still counts %d bytes", session.budget.resident())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOutboundResidentBytesIsValidatedByItsKey(t *testing.T) {
	for _, value := range []int64{-1, 64<<10 + headerSize - 1, maxOutboundResidentBytes + 1} {
		config := DefaultTCPConfig()
		config.MaxPayloadBytes = 64 << 10
		config.OutboundResidentBytes = value
		if err := ValidateTCPConfig(config); err == nil || !strings.Contains(err.Error(), "player_access.tcp.outbound.resident_bytes") {
			t.Errorf("outbound.resident_bytes = %d: %v, want a refusal naming the key", value, err)
		}
	}
	config := DefaultTCPConfig()
	config.MaxPayloadBytes = 64 << 10
	for _, value := range []int64{64<<10 + headerSize, 0} { // 0：代码构造的配置没设置，NewTCPServer 取缺省
		config.OutboundResidentBytes = value
		if err := ValidateTCPConfig(config); err != nil {
			t.Fatalf("outbound.resident_bytes = %d was refused: %v", value, err)
		}
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
