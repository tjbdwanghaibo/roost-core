// TCP regression extraction source
package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
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

func TestWriteFrameRejectsOversizedServerPayload(t *testing.T) {
	session := &tcpSession{maxPayloadBytes: 4}
	if err := session.writeFrame(context.Background(), flagServerPush, 1, 1, []byte("12345")); !errors.Is(err, errInvalidFrame) {
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
	current := &tcpSession{connection: client, principal: Principal{PlayerID: 1, SessionID: "tcpSession"}, writeTimeout: time.Second}
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

// One subscriber's panic must not take the process with it.
//
// The dispatcher runs on its own goroutine, and a panic nobody recovers there
// ends the whole process — so a scene bridge with a nil map would turn every
// disconnect into an outage for every player (RR-20260919-03). Every other
// callback boundary in this framework isolates per callback; this one has to
// as well, and the subscribers after the panicking one still have to run,
// because "who is online" is exactly the state that must converge.
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

func dispatchServer(t *testing.T, dispatchTimeout string, handler func(*tcpTestContext, string) (string, error), configure ...func(*TCPConfig)) *TCPServer {
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

func dialAuthenticated(t *testing.T, server *TCPServer, token string) net.Conn {
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

// waitRegistered waits, bounded, until the player has want registered
// sessions. The server writes the authentication ack before it registers the
// tcpSession (serveConnection), so a client holding its ack does not yet prove
// the server counts it: a test that pushes, closes or counts right after
// dialAuthenticated has to wait for the registration first (RR-20260927-02,
// RR-20260928-15).
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

// RR-20260926-36: a request whose work never finishes must end at the
// dispatch budget and be answered, and the connection's slot and per-IP count
// must come back when the client goes. Before, the handler's context was the
// connection's, which only the server stopping ended.
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

// RR-20260926-36: same-ID replacement (demo auth normally assigns a new ID per connection):
// closing a tcpSession — here the same SessionID logging in on a
// new connection, which is what a reconnect does — must cancel the request its
// old connection is still running, so the old goroutine and slot are released
// now rather than when the server stops.
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

func pushServer(t *testing.T) (*TCPServer, *TCPRuntime, <-chan TCPSessionClosed) {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	config.WriteTimeout = 100 * time.Millisecond
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
			if got.flags == flagServerPush {
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

// RR-20260926-52: a push goes to every connection of the player. One that
// cannot take it — an old connection that stopped reading — used to fail the
// whole push while staying registered until its read loop noticed (up to
// idle_timeout), so every later push failed the same way and the caller
// could not tell a dead player from a live one with a dead old socket.
//
// The connection whose write failed is now closed and unregistered at once;
// the push is a success for the connections that took it; the close goes
// through the socket's own teardown, so its slot, its per-IP count and its
// tcpSession-close event are released exactly once.
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

// RR-20260927-02: CloseSessions reports how many sessions it closed. A
// tcpSession's Close returns the close reason — never nil: a nil reason becomes
// ErrSessionClosed — so counting the calls that returned nil counted
// none, and every fail-closed decision logged sessions_closed=0 however many
// sockets it cut. A tcpSession some other path already closed is not counted
// again.
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

// RR-20260926-68: a write the caller's own deadline cut off before a single
// byte went out left the stream in step; it is refused like a push whose
// context was already done, and must not close the connection. The same
// write stopped by write_timeout, one that wrote part of the frame, and one
// the peer reset are connection failures and still close it (RR-52).
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

// REPRO-2026-09-26-06 §4: pushes whose caller context expires almost at once
// (1µs) are refused, but must not close the player's two healthy, reading
// connections; a normal push afterwards still reaches both.
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
	for range 2000 {
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

// RR-20261005-NC-83: a Stop whose context ends before an authenticator that
// ignores its context returns must not leave a later Stop to report a drain
// that did not happen. The retry waits for the goroutine and only then
// succeeds.
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

// RR-20261005-NC-83: a tcpSession-close subscriber that blocks holds the
// dispatcher, not the Stop budget; once it returns a retry completes.
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
