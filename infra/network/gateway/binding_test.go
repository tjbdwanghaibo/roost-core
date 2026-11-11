package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

func bindRequest(binding Binding) ControlRequest {
	binding.BindID = ""
	return ControlRequest{Version: InternalVersion, Operation: Bind, Binding: binding, Ticket: "ticket", Admission: 1}
}

func TestConcurrentBindSharesOneOwnerAndOneIdentity(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	request := bindRequest(identity)
	var owners atomic.Int32
	var wait sync.WaitGroup
	records := make(chan *bindingRecord, 32)
	for range 32 {
		wait.Go(func() {
			record, owner, err := table.reserve(request, time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			if owner {
				owners.Add(1)
			}
			records <- record
		})
	}
	wait.Wait()
	close(records)
	if owners.Load() != 1 {
		t.Fatalf("owners=%d", owners.Load())
	}
	var first *bindingRecord
	for record := range records {
		if first == nil {
			first = record
		}
		if first != record {
			t.Fatal("duplicate Bind allocated another record")
		}
	}
	_, result := table.finish(first, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := table.await(ctx, first)
	if err != nil || got.Binding != result.Binding || got.Outcome != Completed {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	altered := request
	altered.Ticket = "another-ticket"
	if _, _, err := table.reserve(altered, time.Now()); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("changed proof=%v", err)
	}
}

func TestBindingActivationReplacementAndOldCloseAreIsolated(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	old, _, err := table.reserve(bindRequest(identity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, result := table.finish(old, true)
	renew := ControlRequest{Version: InternalVersion, Operation: Renew, Binding: result.Binding}
	if _, _, err := table.control(renew); !errors.Is(err, ErrBindingHeld) {
		t.Fatalf("held renewal=%v", err)
	}
	activate := renew
	activate.Operation = Activate
	_, activated, err := table.control(activate)
	if err != nil {
		t.Fatal(err)
	}
	_, duplicate, err := table.control(activate)
	if err != nil || duplicate.LeaseUntilUnixNano != activated.LeaseUntilUnixNano {
		t.Fatalf("duplicate activation extended lease: %+v %v", duplicate, err)
	}
	nextIdentity := identity
	nextIdentity.ConnectionNonce = newBindingToken()
	next, _, err := table.reserve(bindRequest(nextIdentity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	previous, result := table.finish(next, true)
	if previous != old || old.state != bindingClosed || result.Binding.BindID == activated.Binding.BindID {
		t.Fatal("replacement did not terminate exact old binding")
	}
	if _, changed := table.close(activated.Binding); changed {
		t.Fatal("old close changed replacement")
	}
	if table.bySession[identity.SessionID] != next {
		t.Fatal("old close removed current session")
	}
	unbind := ControlRequest{Version: InternalVersion, Operation: Unbind, Binding: result.Binding}
	for range 2 {
		if _, result, err := table.control(unbind); err != nil || result.Outcome != Completed {
			t.Fatalf("idempotent unbind=%+v %v", result, err)
		}
	}
	if _, _, err := table.reserve(bindRequest(nextIdentity), time.Now()); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("late Bind resurrected closed connection: %v", err)
	}
}

func TestBindingTerminalCapacityIsReservedBeforeAdmission(t *testing.T) {
	identity := packetTestBinding()
	config := DefaultConfig()
	config.MaxBindings = 2
	config.MaxTombstones = 2
	table := newBindingTable(config, identity.Game)
	first, _, err := table.reserve(bindRequest(identity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	table.finish(first, false)
	secondIdentity := identity
	secondIdentity.ConnectionNonce = newBindingToken()
	second, _, err := table.reserve(bindRequest(secondIdentity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	table.finish(second, false)
	thirdIdentity := identity
	thirdIdentity.ConnectionNonce = newBindingToken()
	if _, _, err := table.reserve(bindRequest(thirdIdentity), time.Now()); !errors.Is(err, ErrAdmissionFull) {
		t.Fatalf("active terminal evicted: %v", err)
	}
	first.terminalUntil = time.Now().Add(-time.Second)
	third, _, err := table.reserve(bindRequest(thirdIdentity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if third.receiverID <= second.receiverID {
		t.Fatal("receiver ID reused")
	}
	if _, _, err := table.reserve(bindRequest(secondIdentity), time.Now()); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("retained terminal resurrected: %v", err)
	}
}

func TestSessionReplacementDoesNotCrossPlayerIdentity(t *testing.T) {
	identity := packetTestBinding()
	table := newBindingTable(DefaultConfig(), identity.Game)
	first, _, err := table.reserve(bindRequest(identity), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	table.finish(first, true)
	other := identity
	other.PlayerID++
	other.ConnectionNonce = newBindingToken()
	second, _, err := table.reserve(bindRequest(other), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	previous, result := table.finish(second, true)
	if previous != nil || result.Outcome != NotAdmitted || table.bySession[identity.SessionID] != first {
		t.Fatal("another player replaced existing session")
	}
}

// Game Ingress 的激活回调（OnActive）执行期间，这个玩家已经可以推送：Gate 写出鉴权 ACK、准备好接收出站之后
// 才发 Activate。回调里登记连接的推送方（自身视图会立刻唤醒同步）若在回调返回前推送，不能得到
// “玩家没有连接”，否则同步会话被丢、连接却还在，没有任何东西再打开它。激活失败的绑定随即不再可推送。
func TestAnIngressBindingIsPushableWhileItsActivationCallbackRuns(t *testing.T) {
	identity := packetTestBinding()
	var game *GameIngress
	var duringActivation error
	connectionsDuringActivation := -1
	refuse := false
	newGame := func() {
		var err error
		game, err = NewGameIngress(DefaultConfig(), identity.Game, GameDependencies{
			Client: payloadClient{limit: 1 << 20}, Authenticator: AuthenticatorFunc(nil), Ready: func() bool { return true },
			Dispatch: func(context.Context, Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) { return nil, nil },
			QueueFactory: func(OutboundLimits, func(context.Context, uint64, []byte) error, func(uint64, error)) (ReliableQueue, error) {
				return benchGateQueue{}, nil
			},
			Matches: func(context.Context, string, ProcessIdentity) (bool, error) { return true, nil },
			OnActive: func(binding Binding, _ uint64) error {
				duringActivation = game.PushSyncPlayer(context.Background(), binding.PlayerID, 9, []byte("first full"))
				connectionsDuringActivation = game.ActiveSessions(binding.PlayerID)
				if refuse {
					return errors.New("activation refused")
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	activate := func() *bindingRecord {
		t.Helper()
		request := bindRequest(identity)
		request.Binding.ConnectionNonce = newBindingToken()
		record, _, err := game.bindings.reserve(request, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		_, result := game.bindings.finish(record, true)
		if err := game.register(record, Principal{PlayerID: identity.PlayerID, SessionID: identity.SessionID}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := game.bindings.control(ControlRequest{Version: InternalVersion, Operation: Activate, Binding: result.Binding}); err != nil {
			t.Fatal(err)
		}
		_ = game.activate(context.Background(), record)
		return record
	}

	newGame()
	activate()
	if duringActivation != nil || connectionsDuringActivation != 1 {
		t.Fatalf("inside OnActive: push = %v, connections = %d; want the push admitted and 1 connection", duringActivation, connectionsDuringActivation)
	}
	if got := game.ActiveSessions(identity.PlayerID); got != 1 {
		t.Fatalf("after activation: connections = %d, want 1", got)
	}

	refuse = true
	newGame()
	activate()
	if got := game.ActiveSessions(identity.PlayerID); got != 0 {
		t.Fatalf("after a refused activation: connections = %d, want 0", got)
	}
	if err := game.PushSyncPlayer(context.Background(), identity.PlayerID, 9, []byte("late")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("push after a refused activation = %v, want ErrSessionNotFound", err)
	}
}

// standaloneTestNamespace 是包内独立 Gate 用例的主题命名空间（走 loopbackBroker，不连真实 NATS）。
const standaloneTestNamespace = "roost.gatebind.test"

// startStandaloneGame 在 broker 上起一个 Game 接入：Bind 用 auth 鉴权，业务请求原样回显（stuckResponseID），
// onActive 收到每次激活的完整绑定。
func startStandaloneGame(t *testing.T, broker *loopbackBroker, identity ProcessIdentity, auth Authenticator, onActive func(Binding), inspect ...func(Session)) *GameIngress {
	t.Helper()
	config := DefaultConfig()
	config.Namespace = standaloneTestNamespace
	inbox, err := InboxPrefix(config.Namespace, "game", identity)
	if err != nil {
		t.Fatal(err)
	}
	game, err := NewGameIngress(config, identity, GameDependencies{
		Client: broker.client(inbox), Authenticator: auth, QueueFactory: serialTestQueueFactory, Ready: func() bool { return true },
		Matches: func(context.Context, string, ProcessIdentity) (bool, error) { return true, nil },
		Dispatch: func(_ context.Context, session Session, _, seq uint32, _ wire.PayloadKind, payload []byte) (any, error) {
			for _, check := range inspect {
				check(session)
			}
			return &TCPResponse{MessageID: stuckResponseID, Sequence: seq, Payload: payload}, nil
		},
		OnActive: func(binding Binding, _ uint64) error {
			if onActive != nil {
				onActive(binding)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := game.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := game.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return game
}

// 同一账号跨物理连接重连时，Game 端看到各自的可信 nonce；它不能退化成可复用的 SessionID。
func TestGateForwardExposesTheBoundConnectionIdentity(t *testing.T) {
	broker := newLoopbackBroker()
	gateID := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	auth := AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same-account-session"}, nil
	})
	activated := make(chan Binding, 2)
	observed := make(chan string, 2)
	startStandaloneGame(t, broker, gameID, auth, func(binding Binding) { activated <- binding }, func(session Session) {
		identified, ok := session.(interface{ ConnectionID() string })
		if !ok {
			observed <- ""
			return
		}
		observed <- identified.ConnectionID()
	})
	server := startStandaloneGate(t, broker, gateID, gameID, auth)
	var previous string
	for _, generation := range []uint64{100, 200} {
		connection := dialPendingGeneration(t, server, "ticket", generation)
		if !handshakeOutcome(t, server, connection) {
			t.Fatal("handshake failed")
		}
		if err := echoRequest(server, connection, 2); err != nil {
			t.Fatal(err)
		}
		// Echo 返回前 Dispatch 与 OnActive 都已经执行；通道各有一条消息。
		binding, key := <-activated, <-observed
		if key == "" || key != binding.ConnectionNonce || key == previous || key == binding.SessionID {
			t.Fatalf("connection key %q, previous %q, binding %+v", key, previous, binding)
		}
		previous = key
	}
}

// startStandaloneGate 在 broker 上起一个独立 Gate 及它的 TCP 监听，所有连接都路由到 game。
func startStandaloneGate(t *testing.T, broker *loopbackBroker, identity, game ProcessIdentity, auth Authenticator) *TCPServer {
	t.Helper()
	config := DefaultConfig()
	config.Namespace = standaloneTestNamespace
	inbox, err := InboxPrefix(config.Namespace, "gate", identity)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := NewGate(config, identity, GateDependencies{
		Client: broker.client(inbox), QueueFactory: serialTestQueueFactory, Ready: func() bool { return true },
		ResolveGame: func(context.Context, int32) (ProcessIdentity, error) { return game, nil },
		Matches:     func(context.Context, string, ProcessIdentity) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := DefaultTCPConfig()
	tcp.Enabled = true
	tcp.Addr = "127.0.0.1:0"
	tcp.HandshakeTimeout = 5 * time.Second
	tcp.MaxPayloadBytes = uint32(config.MaxPayloadBytes)
	server, err := NewTCPServer(tcp, gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectHandshake(gate); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := gate.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := server.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return server
}

// echoRequest 在连接上发一个请求，要求得到 Game 的回显；连接已被关闭或应答不是回显时返回错误。
func echoRequest(server *TCPServer, connection net.Conn, sequence uint32) error {
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	payload := []byte("ping-" + strconv.Itoa(int(sequence)))
	if err := client.writeFrame(context.Background(), 0, stuckRequestID, sequence, payload); err != nil {
		return err
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, release, err := server.readFrame(connection)
	if err != nil {
		return err
	}
	defer release()
	if response.messageID != stuckResponseID || response.sequence != sequence || !bytes.Equal(response.payload, payload) {
		return fmt.Errorf("answer %d/%d %q, want the echo of sequence %d", response.messageID, response.sequence, response.payload, sequence)
	}
	return nil
}

// dialPending 连上 server 并发出认证帧（票据原文，不带连接代号），不等 ack（握手停在服务端的哪一步由用例控制）。
func dialPending(t *testing.T, server *TCPServer, token string) net.Conn {
	t.Helper()
	return dialPendingGeneration(t, server, token, 0)
}

// dialPendingGeneration 同 dialPending，认证帧带客户端连接代号 generation（0 为不带）。
func dialPendingGeneration(t *testing.T, server *TCPServer, token string, generation uint64) net.Conn {
	t.Helper()
	payload, err := wire.EncodeAuth(token, generation)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := (&tcpSession{connection: connection, writeTimeout: time.Second}).writeFrame(context.Background(), 0, 0, 1, payload); err != nil {
		t.Fatal(err)
	}
	return connection
}

// handshakeOutcome 读一条已发认证帧的连接的第一帧：认证 ack 返回 true（连接留着），服务端关闭返回 false；2 秒内
// 两者都没有判失败。
func handshakeOutcome(t *testing.T, server *TCPServer, connection net.Conn) (acked bool) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, release, err := server.readFrame(connection)
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("the handshake neither finished nor closed")
		}
		return false
	}
	release()
	if got.messageID != 0 || got.sequence != 1 {
		t.Fatalf("first frame %d/%d, want the authentication ack", got.messageID, got.sequence)
	}
	return true
}

// finishPending 读完一条已发认证帧的连接直到服务端关闭它，返回是否收到过认证 ack；2 秒内未关闭判失败。
func finishPending(t *testing.T, server *TCPServer, connection net.Conn) (acked bool) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		got, release, err := server.readFrame(connection)
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatal("the connection is still open")
			}
			return acked
		}
		release()
		if got.messageID == 0 && got.sequence == 1 {
			acked = true
		}
	}
}

// 独立 Gate 下同一 SessionID、同一个 Gate 上先后两条连接：先接受的旧连接的 Bind 停在 Game 的鉴权里，后接受的
// 新连接走完 Bind → ACK → Activate 并答完一个请求，再放行旧连接的 Bind。迟到完成的旧 Bind 不能退役新连接的绑定
// （否则 Game 通知 Gate 关掉新连接，旧连接又因 Gate 本地按接受先后被拒，两条都不剩），Gate 收到拒绝后关掉旧连接。
// 客户端放弃一次握手立即重连（Robot 的 Reconnect、玩家网络切换）就是这个交错，与嵌入 TCP 的
// TestALoginThatFinishesLateDoesNotDisplaceANewerConnectionOfTheSameSession 同一规则。
func TestALateBindOfAnOlderConnectionDoesNotRetireTheNewerBindingOfTheSameSession(t *testing.T) {
	broker := newLoopbackBroker()
	gateID := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same"}
	oldEntered, releaseOld := make(chan struct{}), make(chan struct{})
	gameAuth := AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (Principal, error) {
		if ticket == "old" {
			close(oldEntered)
			<-releaseOld
		}
		return principal, nil
	})
	activated := make(chan Binding, 4)
	game := startStandaloneGame(t, broker, gameID, gameAuth, func(binding Binding) { activated <- binding })
	server := startStandaloneGate(t, broker, gateID, gameID, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return principal, nil
	}))

	old := dialPending(t, server, "old")
	select {
	case <-oldEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the old connection's Bind never reached the Game authenticator")
	}
	newer := dialAuthenticated(t, server, "new")
	if err := echoRequest(server, newer, 2); err != nil {
		t.Fatalf("the newer connection could not serve a request before the old Bind finished: %v", err)
	}
	var newerBinding Binding
	select {
	case newerBinding = <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("the newer connection was never activated")
	}

	close(releaseOld)
	// Game 在回复旧 Bind 之前已经定下绑定表；Gate 收到回复后才结束旧连接的握手，所以旧连接关闭时 Game 侧已定。
	if acked := finishPending(t, server, old); acked {
		t.Error("the old connection whose Bind finished late was acknowledged; the Gate must close it on the refused Bind")
	}
	game.bindings.mu.Lock()
	current := game.bindings.bySession[principal.SessionID]
	var currentBinding Binding
	var currentState bindingState
	if current != nil {
		currentBinding, currentState = current.identity, current.state
	}
	game.bindings.mu.Unlock()
	if currentBinding != newerBinding || currentState != bindingActive {
		t.Errorf("the newer binding was retired by an older connection's Bind that finished after it: current=%+v state=%d, want the newer %+v active", currentBinding, currentState, newerBinding)
	}
	if err := echoRequest(server, newer, 3); err != nil {
		t.Fatalf("the newer connection was closed by an older connection's Bind that finished after it: %v", err)
	}
}

// 同一个 Gate 上同一 SessionID 的两次 Bind 都在途、按接受先后完成（另一个方向的交错）：先完成的旧连接已激活并
// 服务请求，新连接的 Bind 随后完成时照常替换它——旧绑定被退役、Gate 关掉旧连接，新连接服务。与迟到旧 Bind 的方向
// 合起来，无论怎样交错，留下的都是接受得更晚的那条。
func TestTheNewerBindOfASessionReplacesTheOlderThatFinishedFirst(t *testing.T) {
	broker := newLoopbackBroker()
	gateID := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same"}
	entered := map[string]chan struct{}{"old": make(chan struct{}), "new": make(chan struct{})}
	release := map[string]chan struct{}{"old": make(chan struct{}), "new": make(chan struct{})}
	gameAuth := AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (Principal, error) {
		close(entered[ticket])
		<-release[ticket]
		return principal, nil
	})
	activated := make(chan Binding, 4)
	game := startStandaloneGame(t, broker, gameID, gameAuth, func(binding Binding) { activated <- binding })
	server := startStandaloneGate(t, broker, gateID, gameID, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return principal, nil
	}))
	waitEntered := func(ticket string) {
		t.Helper()
		select {
		case <-entered[ticket]:
		case <-time.After(2 * time.Second):
			t.Fatalf("the %s connection's Bind never reached the Game authenticator", ticket)
		}
	}
	waitAck := func(connection net.Conn) {
		t.Helper()
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		ack, done, err := server.readFrame(connection)
		if err != nil || ack.messageID != 0 || ack.sequence != 1 {
			t.Fatalf("authentication ack %+v: %v", ack, err)
		}
		done()
	}

	old := dialPending(t, server, "old")
	waitEntered("old")
	newer := dialPending(t, server, "new")
	waitEntered("new")
	close(release["old"])
	waitAck(old)
	if err := echoRequest(server, old, 2); err != nil {
		t.Fatalf("the old connection could not serve a request before the newer Bind finished: %v", err)
	}
	close(release["new"])
	waitAck(newer)
	if err := echoRequest(server, newer, 2); err != nil {
		t.Fatalf("the newer connection could not serve a request: %v", err)
	}
	// 被替换的旧连接由 Gate 关闭（Gate 本地按接受先后，也由 Game 的退役通知）。
	finishPending(t, server, old)
	var newerBinding Binding
	for range 2 {
		select {
		case newerBinding = <-activated:
		case <-time.After(2 * time.Second):
			t.Fatal("both connections should have been activated")
		}
	}
	game.bindings.mu.Lock()
	current := game.bindings.bySession[principal.SessionID]
	ok := current != nil && current.identity == newerBinding && current.state == bindingActive
	game.bindings.mu.Unlock()
	if !ok {
		t.Fatal("the newer binding does not hold the session after replacing the older one")
	}
}

// 跨 Gate 实例的同一 SessionID：客户端在 Gate A 上放弃握手（旧连接，代号 100）、立即连到 Gate B（新连接，代号
// 200），A 上的旧 Bind 停在 Game 的鉴权里；B 上的新连接走完 Bind → ACK → Activate 并答完一个请求，再放行旧 Bind。
// 两边的接受序号不可比较，Game 按客户端连接代号判新旧：迟到的旧 Bind 被拒、不退役新绑定，Gate A 不发 ack、关掉
// 旧连接，新连接继续服务。修前 Game 跨 Gate 不比较、后完成者胜出：新绑定被退役、Gate B 关掉新连接。
func TestAcrossGatesALateBindWithAnOlderConnectionGenerationDoesNotRetireTheNewerBinding(t *testing.T) {
	broker := newLoopbackBroker()
	gateA := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gateB := ProcessIdentity{ServerID: 12, Incarnation: "gate000000000002"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same"}
	oldEntered, releaseOld := make(chan struct{}), make(chan struct{})
	gameAuth := AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (Principal, error) {
		if ticket == "old" {
			close(oldEntered)
			<-releaseOld
		}
		return principal, nil
	})
	activated := make(chan Binding, 4)
	game := startStandaloneGame(t, broker, gameID, gameAuth, func(binding Binding) { activated <- binding })
	gatePrincipal := AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return principal, nil })
	serverA := startStandaloneGate(t, broker, gateA, gameID, gatePrincipal)
	serverB := startStandaloneGate(t, broker, gateB, gameID, gatePrincipal)

	old := dialPendingGeneration(t, serverA, "old", 100)
	select {
	case <-oldEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the old connection's Bind never reached the Game authenticator")
	}
	newer := dialPendingGeneration(t, serverB, "new", 200)
	if !handshakeOutcome(t, serverB, newer) {
		t.Fatal("the newer connection on gate B was not acknowledged")
	}
	if err := echoRequest(serverB, newer, 2); err != nil {
		t.Fatalf("the newer connection could not serve a request before the old Bind finished: %v", err)
	}
	var newerBinding Binding
	select {
	case newerBinding = <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("the newer connection was never activated")
	}

	close(releaseOld)
	// Game 在回复旧 Bind 之前已经定下绑定表；Gate A 收到回复后才结束旧连接的握手（ack 或关闭）。
	oldAcked := handshakeOutcome(t, serverA, old)
	game.bindings.mu.Lock()
	current := game.bindings.bySession[principal.SessionID]
	var currentBinding Binding
	var currentState bindingState
	if current != nil {
		currentBinding, currentState = current.identity, current.state
	}
	game.bindings.mu.Unlock()
	if currentBinding != newerBinding || currentState != bindingActive {
		t.Errorf("the newer binding on gate B was retired by an older connection's Bind on gate A that finished after it: current=%+v state=%d, want the newer %+v active", currentBinding, currentState, newerBinding)
	}
	if err := echoRequest(serverB, newer, 3); err != nil {
		t.Errorf("the newer connection on gate B was closed by an older connection's Bind that finished after it: %v", err)
	}
	if oldAcked {
		t.Error("the old connection on gate A whose Bind finished late was acknowledged; the Gate must close it on the refused Bind")
	}
}

// C# 运行库（client/dotnet，Unity 托管适配同用）跨 Gate 放弃握手：客户端在 Gate A 上发出认证帧，它的 Bind 停在
// Game 的鉴权里；客户端放弃这次握手（取消 ConnectAsync，关掉连接），立即连到 Gate B 走完握手并答完一个请求；
// 之后才放行 Gate A 上的旧 Bind。运行库在认证帧里带连接代号（越晚发起的连接代号越大），Game 认出迟到的旧 Bind
// 并拒掉它，Gate B 上的新连接不被断开（GATEWAY §3、client/README“鉴权首包的连接代号”）。认证帧不带代号时 Game
// 让后完成的旧 Bind 胜出、退役新绑定（修前行为，本用例对它变红）。
//
// 客户端是真实进程：ROOST_CLIENT_TEST_DLL 指向构建好的 Roost.Client.Tests.dll（scripts/test-client-dotnet.sh 构建后
// 设置），没设时跳过。两边的先后用屏障控制：C# 发起旧握手后等标准输入的 abandon（Go 确认旧 Bind 已进入 Game 鉴权
// 才发），新连接答完请求后打印 new-ready，Go 收到后才放行旧 Bind；绑定表定下结果后 Go 发 check，C# 在新连接上
// 再请求一次。
func TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection(t *testing.T) {
	dll := os.Getenv("ROOST_CLIENT_TEST_DLL")
	if dll == "" {
		t.Skip("ROOST_CLIENT_TEST_DLL not set (scripts/test-client-dotnet.sh builds the C# client and runs this test)")
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip(".NET SDK unavailable")
	}
	broker := newLoopbackBroker()
	gateA := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gateB := ProcessIdentity{ServerID: 12, Incarnation: "gate000000000002"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same"}
	oldEntered, releaseOld := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseOld) }) })
	gameAuth := AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (Principal, error) {
		if ticket == "old" {
			close(oldEntered)
			<-releaseOld
		}
		return principal, nil
	})
	activated := make(chan Binding, 4)
	game := startStandaloneGame(t, broker, gameID, gameAuth, func(binding Binding) { activated <- binding })
	gatePrincipal := AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return principal, nil })
	serverA := startStandaloneGate(t, broker, gateA, gameID, gatePrincipal)
	serverB := startStandaloneGate(t, broker, gateB, gameID, gatePrincipal)
	port := func(server *TCPServer) string { return strconv.Itoa(server.Addr().(*net.TCPAddr).Port) }

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := exec.CommandContext(ctx, "dotnet", dll, "../../../client/spec/packets.json", "cross-gate", port(serverA), port(serverB),
		strconv.FormatUint(uint64(stuckRequestID), 10), strconv.FormatUint(uint64(stuckResponseID), 10))
	client.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1")
	stdin, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	client.Stderr = &stderr
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	var transcript []string
	// fail 先结束 C# 进程并等它退出，再读 stderr（exec 的拷贝协程写完之后读，不与它竞争）。
	fail := func(format string, args ...any) {
		t.Helper()
		_ = client.Process.Kill()
		_ = client.Wait()
		t.Fatalf(format+"\ntranscript: %s\nstderr: %s", append(args, strings.Join(transcript, " | "), stderr.String())...)
	}
	expect := func(want string) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					fail("the C# client exited before %q", want)
				}
				transcript = append(transcript, line)
				if line == want {
					return
				}
			case <-time.After(10 * time.Second):
				fail("the C# client never printed %q", want)
			}
		}
	}
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			fail("write %q to the C# client: %v", line, err)
		}
	}

	select {
	case <-oldEntered:
	case <-time.After(10 * time.Second):
		fail("the C# client's first Bind never reached the Game authenticator")
	}
	send("abandon")
	expect("new-ready")
	var newerBinding Binding
	select {
	case newerBinding = <-activated:
	case <-time.After(2 * time.Second):
		fail("the newer connection on gate B was never activated")
	}
	if newerBinding.Gate != gateB {
		fail("the first activated binding is on %+v, want gate B", newerBinding.Gate)
	}
	releaseOnce.Do(func() { close(releaseOld) })
	// 等 Game 定下旧 Bind 的结果（拒绝，或确认并退役新绑定）：gate A 上的记录离开 pending。
	deadline := time.Now().Add(2 * time.Second)
	for {
		game.bindings.mu.Lock()
		settled := false
		for _, record := range game.bindings.byConnection {
			if record.identity.Gate == gateA && record.state != bindingPending {
				settled = true
			}
		}
		current := game.bindings.bySession[principal.SessionID]
		var currentBinding Binding
		var currentState bindingState
		if current != nil {
			currentBinding, currentState = current.identity, current.state
		}
		game.bindings.mu.Unlock()
		if settled {
			if currentBinding != newerBinding || currentState != bindingActive {
				t.Errorf("the newer binding on gate B was retired by the abandoned handshake's Bind on gate A that finished after it: current=%+v state=%d, want the newer %+v active", currentBinding, currentState, newerBinding)
			}
			break
		}
		if time.Now().After(deadline) {
			fail("the Game never settled the late Bind on gate A")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 补充：Game 若退役了新绑定，Gate B 异步收到通知后才关掉连接，留出时间再让 C# 在新连接上请求。
	time.Sleep(200 * time.Millisecond)
	send("check")
	_ = stdin.Close()
	for line := range lines { // 读完标准输出（进程退出时关闭）再 Wait
		transcript = append(transcript, line)
	}
	err = client.Wait()
	if err != nil || !slices.Contains(transcript, "new-alive") {
		t.Errorf("the C# client's newer connection on gate B did not survive: %v: %s\nstderr: %s", err, strings.Join(transcript, " | "), stderr.String())
	}
}

// 跨 Gate 实例、认证帧都没带连接代号（老客户端、非 Go 客户端没实现）：没有可比较的先后，仍是后完成的 Bind 胜出。
// 用例让另一个 Gate 上先接受（本进程的序号更小）的连接迟到完成：若 Game 跨 Gate 比较接受序号，它会被当成迟到的
// 旧登录拒掉；正确行为是它替换先完成的那条，那条连接被所在的 Gate 关掉。
func TestAcrossGatesWithoutGenerationsTheLaterFinishedBindOfASessionWins(t *testing.T) {
	broker := newLoopbackBroker()
	gateA := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gateB := ProcessIdentity{ServerID: 12, Incarnation: "gate000000000002"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "same"}
	lateEntered, releaseLate := make(chan struct{}), make(chan struct{})
	gameAuth := AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (Principal, error) {
		if ticket == "late" {
			close(lateEntered)
			<-releaseLate
		}
		return principal, nil
	})
	activated := make(chan Binding, 4)
	game := startStandaloneGame(t, broker, gameID, gameAuth, func(binding Binding) { activated <- binding })
	gatePrincipal := AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return principal, nil })
	serverA := startStandaloneGate(t, broker, gateA, gameID, gatePrincipal)
	serverB := startStandaloneGate(t, broker, gateB, gameID, gatePrincipal)

	late := dialPending(t, serverB, "late")
	select {
	case <-lateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the late connection's Bind never reached the Game authenticator")
	}
	early := dialAuthenticated(t, serverA, "early")
	if err := echoRequest(serverA, early, 2); err != nil {
		t.Fatalf("the connection on gate A could not serve a request: %v", err)
	}
	<-activated
	close(releaseLate)
	_ = late.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack, done, err := serverB.readFrame(late)
	if err != nil || ack.messageID != 0 || ack.sequence != 1 {
		t.Fatalf("the later-finished Bind on gate B was refused: ack %+v: %v", ack, err)
	}
	done()
	if err := echoRequest(serverB, late, 2); err != nil {
		t.Fatalf("the connection on gate B could not serve a request: %v", err)
	}
	var lateBinding Binding
	select {
	case lateBinding = <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("the connection on gate B was never activated")
	}
	// Game 退役 A 上的绑定后异步通知 Gate A 关连接（通知只加速关闭，租期兜底）。
	finishPending(t, serverA, early)
	game.bindings.mu.Lock()
	current := game.bindings.bySession[principal.SessionID]
	ok := current != nil && current.identity == lateBinding && current.state == bindingActive
	game.bindings.mu.Unlock()
	if !ok {
		t.Fatal("the later-finished Bind on another gate does not hold the session")
	}
}

// 绑定表的新旧判定：同一 Gate 实例内按接受序号，不论完成先后；迟到的旧 Bind 被拒且不退役任何人。不同 Gate
// 实例（含同一 SID 的 Gate 重启后的新代际，序号从头开始）不比较序号，后完成者胜出——重启后的第一个连接序号很小，
// 也不会被当成旧登录。同一连接的重复 Bind 必须带同一个序号。
func TestBindingTableOrdersASessionByAdmissionWithinOneGate(t *testing.T) {
	identity := packetTestBinding()
	bind := func(table *bindingTable, gate ProcessIdentity, admission uint64) *bindingRecord {
		t.Helper()
		request := bindRequest(identity)
		request.Binding.Gate = gate
		request.Binding.ConnectionNonce = newBindingToken()
		request.Admission = admission
		record, owner, err := table.reserve(request, time.Now())
		if err != nil || !owner {
			t.Fatalf("reserve: owner=%v err=%v", owner, err)
		}
		return record
	}
	gate := identity.Gate
	restarted := gate
	restarted.Incarnation = gate.Incarnation + "restarted"

	// 新连接（序号 9）先完成，旧连接（序号 3）迟到完成：旧的被拒，新的不受影响。
	table := newBindingTable(DefaultConfig(), identity.Game)
	older, newer := bind(table, gate, 3), bind(table, gate, 9)
	if previous, result := table.finish(newer, true); previous != nil || result.Outcome != Completed {
		t.Fatalf("newer Bind: previous=%v result=%+v", previous, result)
	}
	previous, result := table.finish(older, true)
	if previous != nil || result.Outcome != NotAdmitted || result.Reason != bindingSupersededReason || older.state != bindingClosed {
		t.Fatalf("late older Bind: previous=%v result=%+v state=%d; want refused as superseded", previous, result, older.state)
	}
	if table.bySession[identity.SessionID] != newer || newer.state != bindingConfirmed {
		t.Fatal("a refused late Bind retired the newer binding")
	}

	// 旧连接先完成，新连接后完成：新的替换旧的。
	table = newBindingTable(DefaultConfig(), identity.Game)
	older, newer = bind(table, gate, 3), bind(table, gate, 9)
	table.finish(older, true)
	if previous, result := table.finish(newer, true); previous != older || result.Outcome != Completed || older.state != bindingClosed {
		t.Fatalf("newer Bind after older: previous=%v result=%+v", previous, result)
	}

	// Gate 重启：新代际的序号从 1 开始，仍按后完成者替换旧代际的绑定。
	table = newBindingTable(DefaultConfig(), identity.Game)
	before := bind(table, gate, 1000)
	table.finish(before, true)
	after := bind(table, restarted, 1)
	if previous, result := table.finish(after, true); previous != before || result.Outcome != Completed {
		t.Fatalf("Bind from the restarted gate: previous=%v result=%+v; want it to replace the previous incarnation", previous, result)
	}

	// 同一连接的重复 Bind 换了序号：不是同一次 Bind，拒绝。
	request := bindRequest(identity)
	request.Admission = 5
	if _, _, err := table.reserve(request, time.Now()); err != nil {
		t.Fatal(err)
	}
	request.Admission = 6
	if _, _, err := table.reserve(request, time.Now()); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("a retried Bind with another admission = %v, want ErrBindingStale", err)
	}
}

// 绑定表跨 Gate 实例的新旧：比客户端连接代号，只裁决在 Game 上重叠的两次 Bind。
//   - 新连接（代号 200）先确认，确认前已到达的旧 Bind（代号 100）迟到完成：被拒，新绑定不受影响。
//   - 旧的先确认、新的后完成：新的替换旧的。
//   - 已确认的绑定代号再大，也挡不住确认之后才到达的 Bind：后者照常替换（代号锁不死会话）。
//   - 任一方没带代号：后完成者胜出。
//   - 同一 Gate 实例只比接受序号，代号不参与。
//   - 同一连接的重复 Bind 换了代号：不是同一次 Bind，拒绝。
func TestBindingTableOrdersASessionAcrossGatesByConnectionGeneration(t *testing.T) {
	identity := packetTestBinding()
	gateA := identity.Gate
	gateB := ProcessIdentity{ServerID: gateA.ServerID + 1, Incarnation: gateA.Incarnation + "b"}
	bind := func(table *bindingTable, gate ProcessIdentity, admission, generation uint64) *bindingRecord {
		t.Helper()
		request := bindRequest(identity)
		request.Binding.Gate = gate
		request.Binding.ConnectionNonce = newBindingToken()
		request.Admission = admission
		request.Generation = generation
		record, owner, err := table.reserve(request, time.Now())
		if err != nil || !owner {
			t.Fatalf("reserve: owner=%v err=%v", owner, err)
		}
		return record
	}
	refused := func(table *bindingTable, late, current *bindingRecord) {
		t.Helper()
		previous, result := table.finish(late, true)
		if previous != nil || result.Outcome != NotAdmitted || result.Reason != bindingSupersededReason || late.state != bindingClosed {
			t.Fatalf("late older Bind: previous=%v result=%+v state=%d; want refused as superseded", previous, result, late.state)
		}
		if table.bySession[identity.SessionID] != current || current.state != bindingConfirmed {
			t.Fatal("a refused late Bind retired the newer binding")
		}
	}
	replaces := func(table *bindingTable, later, earlier *bindingRecord) {
		t.Helper()
		previous, result := table.finish(later, true)
		if previous != earlier || result.Outcome != Completed || earlier.state != bindingClosed || table.bySession[identity.SessionID] != later {
			t.Fatalf("later Bind: previous=%v result=%+v; want it to replace the earlier binding", previous, result)
		}
	}

	// 新连接在 B 上先确认，A 上确认前已到达的旧 Bind 迟到完成。A 的接受序号更大也不影响（不同 Gate 不比序号）。
	table := newBindingTable(DefaultConfig(), identity.Game)
	older, newer := bind(table, gateA, 50, 100), bind(table, gateB, 1, 200)
	table.finish(newer, true)
	refused(table, older, newer)

	// 旧的先确认，新的后完成。
	table = newBindingTable(DefaultConfig(), identity.Game)
	older, newer = bind(table, gateA, 1, 100), bind(table, gateB, 1, 200)
	table.finish(older, true)
	replaces(table, newer, older)

	// 已确认的是超大代号，之后到达的小代号 Bind 照常替换它。
	table = newBindingTable(DefaultConfig(), identity.Game)
	huge := bind(table, gateA, 1, ^uint64(0))
	table.finish(huge, true)
	replaces(table, bind(table, gateB, 1, 100), huge)

	// 任一方没带代号：后完成者胜出。
	for _, generations := range [][2]uint64{{0, 200}, {100, 0}, {0, 0}} {
		table = newBindingTable(DefaultConfig(), identity.Game)
		late, first := bind(table, gateA, 1, generations[0]), bind(table, gateB, 1, generations[1])
		table.finish(first, true)
		replaces(table, late, first)
	}

	// 同一 Gate 实例只比接受序号：序号更大、代号更小的新连接替换旧的；序号更小、代号更大的迟到 Bind 被拒。
	table = newBindingTable(DefaultConfig(), identity.Game)
	older, newer = bind(table, gateA, 3, 200), bind(table, gateA, 9, 100)
	table.finish(older, true)
	replaces(table, newer, older)
	table = newBindingTable(DefaultConfig(), identity.Game)
	older, newer = bind(table, gateA, 3, 200), bind(table, gateA, 9, 100)
	table.finish(newer, true)
	refused(table, older, newer)

	// 同一连接的重复 Bind 换了代号：拒绝。
	request := bindRequest(identity)
	request.Generation = 5
	if _, _, err := table.reserve(request, time.Now()); err != nil {
		t.Fatal(err)
	}
	request.Generation = 6
	if _, _, err := table.reserve(request, time.Now()); !errors.Is(err, ErrBindingStale) {
		t.Fatalf("a retried Bind with another generation = %v, want ErrBindingStale", err)
	}
}
