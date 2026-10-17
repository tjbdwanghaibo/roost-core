//go:build integration

// 独立 Gate 下的自身视图：真实 Gate + Game Ingress（NATS）+ 玩家 TCP 客户端。Game → Gate 的出站发送经一道
// 可暂停的闸门（只控制何时发出，队列、预算与错误仍是正式的 AsyncQueueFactory），用来确定地造出
// “这条连接的可靠队列满了”和“整个进程的驻留字节预算耗尽”两种拒收。

package selfsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	wiringnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

// fillerMessageID 是测试直接塞进出站队列的占位推送；客户端读到它就跳过。
const fillerMessageID uint32 = 10902

// sendGate 是 Game → Gate 出站发送前的闸门：paused 时发送等到 resume 或 ctx 结束，进入等待时通知 entered。
type sendGate struct {
	mu      sync.Mutex
	paused  chan struct{} // nil 表示放行
	entered chan struct{}
}

func (g *sendGate) pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused, g.entered = make(chan struct{}), make(chan struct{}, 64)
}

func (g *sendGate) resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused != nil {
		close(g.paused)
		g.paused = nil
	}
}

func (g *sendGate) wait(ctx context.Context) error {
	g.mu.Lock()
	paused, entered := g.paused, g.entered
	g.mu.Unlock()
	if paused == nil {
		return nil
	}
	select {
	case entered <- struct{}{}:
	default:
	}
	select {
	case <-paused:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitEntered 等一次发送停在闸门上（它的帧已从队列取走、算在驻留字节里）。
func (g *sendGate) awaitEntered(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	entered := g.entered
	g.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no outbound send reached the gate")
	}
}

type gateSelfHarness struct {
	access  *entity.ManagerAccess
	self    *SelfSync
	manager *entitysync.Manager
	game    *gateway.GameIngress
	server  *gateway.TCPServer
	sends   *sendGate
	// insideActivation 非 nil 时在 OnActive 登记连接之后、回调返回之前执行（用来把 Flush 挤进激活回调）。
	insideActivation func()
}

// newGateSelfHarness 装配独立 Gate 的正式链路：GameIngress 的 OnActive / OnClosed 接 SelfSync，推送经
// BindPlayers(GameIngress)，Gate 前面是玩家 TCP。configure 调整 gateway.Config（队列条数、驻留预算等）。
// Manager 不 Start，由测试 Flush。
func newGateSelfHarness(t *testing.T, configure func(*gateway.Config)) *gateSelfHarness {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	playerKind(t)
	h := &gateSelfHarness{access: entity.NewManagerAccess(entity.NewEntityManager()), sends: &sendGate{}}
	h.self = MustNew(Config{Entities: h.access, Kind: testKind, MessageID: testMessageID})
	mod := wiringnest.NewModWithEntitySync(h.access, h.self.EntitySyncSetup())
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	h.manager = mod.EntitySync()
	t.Cleanup(func() { _ = h.manager.Close(context.Background()) })

	cfg := gateway.DefaultConfig()
	cfg.Namespace = fmt.Sprintf("roost.selfsync.test.n%d", time.Now().UnixNano())
	// 闸门可能挡住发送几百毫秒：年龄与发送超时放宽，不让帧在测试控制的暂停里过期（过期是另一条关闭路径）。
	cfg.Outbound.MaxAge, cfg.Outbound.SendTimeout = 30*time.Second, 30*time.Second
	if configure != nil {
		configure(&cfg)
	}
	gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	client := func(role string, id gateway.ProcessIdentity) *driver.Client {
		prefix, _ := gateway.InboxPrefix(cfg.Namespace, role, id)
		c, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{InboxPrefix: prefix, ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	gateClient, gameClient := client("gate", gateID), client("game", gameID)
	auth := gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		player, err := strconv.ParseInt(strings.TrimPrefix(ticket, "player:"), 10, 64)
		if err != nil || player <= 0 {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: player, ServerID: gameID.ServerID, SessionID: ticket + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)}, nil
	})
	match := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return (role == "gate" && id == gateID) || (role == "game" && id == gameID), nil
	}
	queue := func(limits gateway.OutboundLimits, send func(context.Context, uint64, []byte) error, onError func(uint64, error)) (gateway.ReliableQueue, error) {
		return gatewiring.AsyncQueueFactory(limits, func(ctx context.Context, receiver uint64, data []byte) error {
			if err := h.sends.wait(ctx); err != nil {
				return err
			}
			return send(ctx, receiver, data)
		}, onError)
	}
	var err error
	h.game, err = gateway.NewGameIngress(cfg, gameID, gateway.GameDependencies{
		Client: gameClient, Authenticator: auth, QueueFactory: queue, Ready: func() bool { return true }, Matches: match,
		Dispatch: func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
			return nil, errors.New("no business messages in this test")
		},
		OnActive: func(binding gateway.Binding, receiver uint64) error {
			err := h.self.OnActive(binding, receiver)
			if h.insideActivation != nil {
				h.insideActivation()
			}
			return err
		},
		OnClosed: h.self.OnClosed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.game.Start(); err != nil {
		t.Fatal(err)
	}
	h.self.BindPlayers(h.game)
	gate, err := gateway.NewGate(cfg, gateID, gateway.GateDependencies{Client: gateClient, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true },
		ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }, Matches: match})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := gateway.DefaultTCPConfig()
	tcp.Enabled, tcp.Addr, tcp.MaxPayloadBytes = true, "127.0.0.1:0", uint32(cfg.MaxPayloadBytes)
	h.server, err = gateway.NewTCPServer(tcp, gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.ConnectHandshake(gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, c := range []*driver.Client{gateClient, gameClient} {
		if err := c.FlushContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.sends.resume()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for name, stop := range map[string]func(context.Context) error{"gate": gate.Stop, "tcp": h.server.Stop} {
			if err := stop(ctx); err != nil {
				t.Errorf("stop %s: %v", name, err)
			}
		}
		if err := h.game.Stop(ctx); err != nil {
			t.Errorf("stop game ingress: %v", err)
		}
	})
	return h
}

// gateClientConn 是经 Gate 连上的玩家：先读 ACK，之后只认 Sync 推送（跳过占位推送）。
type gateClientConn struct {
	connection net.Conn
	receiver   *frame.Receiver
}

func (h *gateSelfHarness) dial(t *testing.T, playerID int64) *gateClientConn {
	t.Helper()
	connection, err := net.DialTimeout("tcp", h.server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := wire.Write(connection, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("player:" + strconv.FormatInt(playerID, 10))}}, wire.DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	if ack, err := wire.Read(connection, wire.DefaultMaxPayload); err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("authentication ACK = %+v, %v", ack, err)
	}
	return &gateClientConn{connection: connection, receiver: frame.NewReceiver(frame.Limits{})}
}

// next 在 wait 内读下一个自身视图推送。closed 表示连接已被服务端关闭；两者都为零值表示 wait 内没有推送。
func (c *gateClientConn) next(t *testing.T, wait time.Duration) (kind frame.Kind, state string, got, closed bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		_ = c.connection.SetReadDeadline(deadline)
		packet, err := wire.Read(c.connection, wire.DefaultMaxPayload)
		var timeout net.Error
		switch {
		case errors.As(err, &timeout) && timeout.Timeout():
			return 0, "", false, false
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed), err != nil && strings.Contains(err.Error(), "connection reset"):
			return 0, "", false, true
		case err != nil:
			t.Fatalf("read: %v", err)
		}
		if packet.MsgID == fillerMessageID {
			continue
		}
		if packet.Flags != wire.FlagPush|wire.FlagSync || packet.MsgID != testMessageID {
			t.Fatalf("packet %+v is not a self-view push", packet)
		}
		applied, err := c.receiver.Receive(packet.Payload, func(decoded frame.Frame) error {
			kind = decoded.Kind
			for _, object := range decoded.Objects {
				for _, component := range object.Components {
					update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
					if err != nil {
						return err
					}
					state = string(update.Payload.BytesCopy())
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("self-view frame refused by the client's receiver: %v", err)
		}
		if applied {
			return kind, state, true, false
		}
	}
}

// fillPlayerQueue 让玩家这条连接的可靠队列满：闸门暂停，第一条占位推送被发送协程取走并停在闸门上，
// 第二条留在队列里（QueueEntries=1），之后的推送都被 ErrReliableBackpressure 拒收。
func (h *gateSelfHarness) fillPlayerQueue(t *testing.T, playerID int64) {
	t.Helper()
	h.sends.pause()
	if err := h.game.PushSyncPlayer(context.Background(), playerID, fillerMessageID, []byte("filler in flight")); err != nil {
		t.Fatalf("first filler: %v", err)
	}
	h.sends.awaitEntered(t)
	if err := h.game.PushSyncPlayer(context.Background(), playerID, fillerMessageID, []byte("filler queued")); err != nil {
		t.Fatalf("second filler: %v", err)
	}
}

func waitSelfOnline(t *testing.T, self *SelfSync, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for self.Online() != want {
		if time.Now().After(deadline) {
			t.Fatalf("online players = %d, want %d", self.Online(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// 一次偶发的拒收（这条连接的可靠队列满了，连接还活着）之后，玩家总能拿回自己的状态：连接被关闭，
// 客户端重连后收到全量。连着发生多次也一样——每次都是一条新连接、一次新激活，不会在某一次之后永久失去视图。
func TestSelfSyncOverGateRecoversAfterEveryRefusedPush(t *testing.T) {
	h := newGateSelfHarness(t, func(cfg *gateway.Config) { cfg.Outbound.QueueEntries = 1 })
	player := newWallet(t, 7, 0)
	h.access.Manager().Add(player)
	c := h.dial(t, 7)
	waitSelfOnline(t, h.self, 1)
	if err := h.manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kind, state, got, _ := c.next(t, 3*time.Second); !got || kind != frame.Full || state != "gold=0" {
		t.Fatalf("first state = %v %q (got=%v), want a Full with gold=0", kind, state, got)
	}
	reconnects := 0
	for round := 1; round <= 12; round++ {
		h.fillPlayerQueue(t, 7)
		player.setGold(int64(round))
		_ = h.manager.Flush(context.Background()) // 这一帧被拒收
		h.sends.resume()
		want := "gold=" + strconv.Itoa(round)
		recovered := false
		for deadline := time.Now().Add(3 * time.Second); !recovered && time.Now().Before(deadline); {
			kind, state, got, closed := c.next(t, 100*time.Millisecond)
			switch {
			case closed:
				// 连接被关闭：客户端照常重连，新连接从全量开始。
				waitSelfOnline(t, h.self, 0)
				c = h.dial(t, 7)
				reconnects++
				waitSelfOnline(t, h.self, 1)
			case got && state == want:
				if kind != frame.Full {
					t.Fatalf("round %d: the state came back as %v, want the Full of a new session", round, kind)
				}
				recovered = true
			}
			if !recovered {
				_ = h.manager.Flush(context.Background())
			}
		}
		if !recovered {
			t.Fatalf("round %d: one refused push and the player never got its state back: no frame on the live connection "+
				"and the connection was not closed (sessions=%d lost=%d online=%d)",
				round, h.manager.Stats().Sessions, h.manager.Stats().SessionsLost, h.self.Online())
		}
	}
	// 每次拒收都断开了那条连接，恢复都经重连：同一条连接上不再有“会话死了、连接还活着”的中间状态。
	if reconnects != 12 {
		t.Fatalf("recovered through %d reconnects, want 12 (one per refused push)", reconnects)
	}
}

// 整个进程的驻留字节预算耗尽不是哪个玩家的错：整轮重试、保留脏状态，连接与同步会话都不动；
// 预算恢复后照常收到增量（不是重开后的全量）。
func TestSelfSyncOverGateRetriesTheTickWhenTheResidentBudgetIsExhausted(t *testing.T) {
	h := newGateSelfHarness(t, func(cfg *gateway.Config) {
		cfg.MaxPayloadBytes = 32 << 10
		cfg.Outbound.MaxPacketBytes = 40 << 10
		cfg.Outbound.PerSessionBytes = 1 << 20
		cfg.Outbound.QueueEntries = 1024
		cfg.Outbound.ResidentBytes = 48 << 10
	})
	player := newWallet(t, 7, 1)
	h.access.Manager().Add(player)
	c := h.dial(t, 7)
	h.dial(t, 8) // 只用来占预算的另一个玩家：没有实体，自身视图不向它推送
	waitSelfOnline(t, h.self, 2)
	if err := h.manager.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kind, state, got, _ := c.next(t, 3*time.Second); !got || kind != frame.Full || state != "gold=1" {
		t.Fatalf("first state = %v %q (got=%v), want a Full with gold=1", kind, state, got)
	}

	// 占满驻留预算：一条大占位帧停在闸门上，再用越来越小的占位帧把剩余字节填到连 1 字节的推送也放不下。
	h.sends.pause()
	if err := h.game.PushSyncPlayer(context.Background(), 8, fillerMessageID, make([]byte, 30<<10)); err != nil {
		t.Fatal(err)
	}
	h.sends.awaitEntered(t)
	for size := 8 << 10; size >= 1; size /= 2 {
		for h.game.PushSyncPlayer(context.Background(), 8, fillerMessageID, make([]byte, size)) == nil {
		}
	}

	player.setGold(2)
	err := h.manager.Flush(context.Background())
	if !errors.Is(err, entitysync.ErrRetryLater) {
		t.Fatalf("flush with the resident budget exhausted = %v, want ErrRetryLater (the tick is retried, nobody is disconnected)", err)
	}
	if lost := h.manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("sessions lost = %d, want 0: an exhausted process budget is not the player's failure", lost)
	}
	if online := h.self.Online(); online != 2 {
		t.Fatalf("online = %d, want 2: no connection is closed for an exhausted process budget", online)
	}
	h.sends.resume()
	var kind frame.Kind
	var state string
	for deadline := time.Now().Add(3 * time.Second); state != "gold=2" && time.Now().Before(deadline); {
		if err := h.manager.Flush(context.Background()); err != nil && !errors.Is(err, entitysync.ErrRetryLater) {
			t.Fatal(err)
		}
		var got, closed bool
		kind, state, got, closed = c.next(t, 100*time.Millisecond)
		if closed {
			t.Fatal("the player's connection was closed for an exhausted process budget")
		}
		_ = got
	}
	if state != "gold=2" || kind != frame.Delta {
		t.Fatalf("after the budget recovered = %v %q, want a Delta with gold=2 on the same session", kind, state)
	}
	if lost := h.manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("sessions lost = %d after the budget recovered, want 0", lost)
	}
}

// 激活回调里登记连接会立刻唤醒同步；同步若在回调返回之前就推送（on_change 下很常见），Game Ingress 必须已经
// 把这个绑定当作可推送：否则推送得到“玩家没有连接”，同步会话被丢而连接活着，没有东西再打开它。
// 第一帧全量在 ACK 之后照常送达，没有丢会话。
func TestSelfSyncOverGateDeliversTheFirstFullWhenSyncRunsInsideActivation(t *testing.T) {
	h := newGateSelfHarness(t, nil)
	h.insideActivation = func() {
		if err := h.manager.Flush(context.Background()); err != nil {
			t.Errorf("flush inside activation: %v", err)
		}
	}
	h.access.Manager().Add(newWallet(t, 7, 5))
	c := h.dial(t, 7)
	if kind, state, got, closed := c.next(t, 3*time.Second); !got || kind != frame.Full || state != "gold=5" {
		t.Fatalf("first state = %v %q (got=%v closed=%v), want a Full with gold=5", kind, state, got, closed)
	}
	if lost := h.manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("sessions lost = %d, want 0: the push inside activation found the player without a connection", lost)
	}
}
