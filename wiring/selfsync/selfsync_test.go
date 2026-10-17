// 自身视图接线：真实 gateway TCP（鉴权、出站队列）+ wiring/nest Mod 装出的 Manager + 内存里的玩家实体。
// 客户端按正式 RS v2 帧读：先 ACK，再 Sync 全量，改动后收增量，重连后再收全量。

package selfsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	wiringnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

// testKind 只在本包登记；同一进程里不与其他包的 kind 冲突（各包是独立测试二进制）。
const (
	testKind      entity.EntityKind = 61
	testMessageID uint32            = 10901
)

var registerKind sync.Once

func playerKind(t testing.TB) {
	t.Helper()
	registerKind.Do(func() { entity.MustRegisterEntityKindCategory(testKind, entity.EntityCategoryPlayer) })
}

// wallet 是测试里的玩家实体：一个金币字段，packer 把它写成 "gold=N"，认识 self 视图。
type wallet struct {
	*entity.EntityBase
	gold atomic.Int64
	pad  atomic.Int64 // 非零时在载荷后追加这么多字节，用来让停读的连接很快积满出站队列
}

func (w *wallet) Base() *entity.EntityBase { return w.EntityBase }

func newWallet(t testing.TB, playerID int64, gold int64) *wallet {
	t.Helper()
	id, err := entity.BuildEntityID(playerID, testKind)
	if err != nil {
		t.Fatal(err)
	}
	w := &wallet{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryPlayer, false, testKind)}
	w.gold.Store(gold)
	pack := func(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		if profile.Normalize().Key != policy.SelfProfile.Key {
			return entity.FrozenSyncPayload{}, fmt.Errorf("%w: %+v", entity.ErrSyncViewUnknown, profile)
		}
		payload := []byte("gold=" + strconv.FormatInt(w.gold.Load(), 10))
		if pad := w.pad.Load(); pad > 0 {
			payload = append(payload, make([]byte, pad)...)
		}
		return entity.TakeFrozenSyncPayload(1, payload), nil
	}
	w.SetSyncState(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "player", SubjectKind: uint32(testKind),
		Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }},
	}))
	return w
}

// setGold 是测试里的“handler 改金币”：改内容再标脏，与生成 DAO setter + MarkSyncDirty 同一顺序。
func (w *wallet) setGold(gold int64) {
	w.gold.Store(gold)
	w.Sync().MarkDirty(1)
}

type harness struct {
	access  *entity.ManagerAccess
	self    *SelfSync
	mod     *wiringnest.Mod
	manager *entitysync.Manager
	server  *gateway.TCPServer
	runtime *gateway.TCPRuntime
}

// newHarness 走正式装配：wiring/nest.NewModWithEntitySync(EntitySyncSetup()) 的 Init 建 Manager 并装上自身视图；
// 嵌入 TCP 用 ConnectRuntime + ConnectHandshake(Handshake())，推送经 BindPlayers 接上。Manager 不 Start，
// 由测试调用 Flush 决定每个 tick 何时发生。afterACK 非 nil 时在激活之后、握手返回之前再执行一次（用来把 Flush
// 挤进握手里）。
func newHarness(t testing.TB, afterACK func(*harness)) *harness {
	t.Helper()
	playerKind(t)
	h := &harness{access: entity.NewManagerAccess(entity.NewEntityManager())}
	self, err := New(Config{Entities: h.access, Kind: testKind, MessageID: testMessageID})
	if err != nil {
		t.Fatal(err)
	}
	h.self = self
	h.mod = wiringnest.NewModWithEntitySync(h.access, self.EntitySyncSetup())
	if err := h.mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	h.manager = h.mod.EntitySync()
	t.Cleanup(func() { _ = h.manager.Close(context.Background()) })

	config := gateway.DefaultTCPConfig()
	config.Enabled, config.Addr = true, "127.0.0.1:0"
	server, err := gateway.NewTCPServer(config, func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, errors.New("no business messages in this test")
	}, gateway.AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (gateway.Principal, error) {
		player, err := strconv.ParseInt(strings.TrimPrefix(token, "player:"), 10, 64)
		if err != nil || player <= 0 {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: player, SessionID: token + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	h.server, h.runtime = server, gateway.NewTCPRuntime(nil, time.Second)
	if err := server.ConnectRuntime(h.runtime); err != nil {
		t.Fatal(err)
	}
	hooks := self.Handshake()
	if afterACK != nil {
		hooks = squeezeHandshake{TCPHandshake: hooks, after: func() { afterACK(h) }}
	}
	if err := server.ConnectHandshake(hooks); err != nil {
		t.Fatal(err)
	}
	self.BindPlayers(h.runtime)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	return h
}

type squeezeHandshake struct {
	gateway.TCPHandshake
	after func()
}

func (s squeezeHandshake) AfterACK(ctx context.Context, session gateway.ConnectionSession) error {
	err := s.TCPHandshake.AfterACK(ctx, session)
	s.after()
	return err
}

func (h *harness) flush(t testing.TB) {
	t.Helper()
	if err := h.manager.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// client 是一个按正式协议读的玩家连接：frame.Receiver 维护基线，DecodeSubjectUpdate 取出自己的状态。
type client struct {
	connection net.Conn
	receiver   *frame.Receiver
}

func dial(t testing.TB, h *harness, playerID int64) *client {
	t.Helper()
	connection, err := net.Dial("tcp", h.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := wire.Write(connection, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("player:" + strconv.FormatInt(playerID, 10))}}, wire.DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
	return &client{connection: connection, receiver: frame.NewReceiver(frame.Limits{})}
}

// read 读下一个包。
func (c *client) read(t testing.TB) *wire.Packet {
	t.Helper()
	_ = c.connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	packet, err := wire.Read(c.connection, wire.DefaultMaxPayload)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return packet
}

func (c *client) ack(t testing.TB) {
	t.Helper()
	if packet := c.read(t); packet.MsgID != 0 || packet.Seq != 1 || len(packet.Payload) != 0 || packet.Flags != 0 {
		t.Fatalf("first packet %+v is not the authentication ACK", packet)
	}
}

// state 读下一个 Sync 推送，返回帧类型与其中自己的状态。
func (c *client) state(t testing.TB) (frame.Kind, string) {
	t.Helper()
	packet := c.read(t)
	if packet.Flags != wire.FlagPush|wire.FlagSync || packet.MsgID != testMessageID {
		t.Fatalf("packet %+v is not a Sync push on message %d", packet, testMessageID)
	}
	var kind frame.Kind
	var payload string
	applied, err := c.receiver.Receive(packet.Payload, func(decoded frame.Frame) error {
		kind = decoded.Kind
		for _, object := range decoded.Objects {
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					return err
				}
				payload = string(update.Payload.BytesCopy())
			}
		}
		return nil
	})
	if err != nil || !applied {
		t.Fatalf("frame not applied: applied=%v err=%v", applied, err)
	}
	return kind, payload
}

func waitOnline(t testing.TB, h *harness, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.self.Online() != want {
		if time.Now().After(deadline) {
			t.Fatalf("online players = %d, want %d", h.self.Online(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSelfSyncOverRealTCP(t *testing.T) {
	h := newHarness(t, nil)
	player := newWallet(t, 7, 10)
	h.access.Manager().Add(player)

	c := dial(t, h, 7)
	c.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, state := c.state(t); kind != frame.Full || state != "gold=10" {
		t.Fatalf("first state = %v %q, want a Full with gold=10", kind, state)
	}
	player.setGold(15)
	h.flush(t)
	if kind, state := c.state(t); kind != frame.Delta || state != "gold=15" {
		t.Fatalf("after the change = %v %q, want a Delta with gold=15", kind, state)
	}

	// 断线重连：新连接从全量开始。
	_ = c.connection.Close()
	waitOnline(t, h, 0)
	h.flush(t)
	if stats := h.manager.Stats(); stats.Sessions != 0 || stats.Subjects != 0 {
		t.Fatalf("after disconnect: sessions=%d subjects=%d, want 0/0", stats.Sessions, stats.Subjects)
	}
	player.setGold(20) // 离线期间的改动：没有登记，不推送
	again := dial(t, h, 7)
	again.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, state := again.state(t); kind != frame.Full || state != "gold=20" {
		t.Fatalf("after reconnect = %v %q, want a Full with gold=20", kind, state)
	}
}

// 首个全量排在登录回包之后：即使 Flush 挤在激活回调里（握手还没返回）立即推送，客户端也先读到 ACK——
// ACK 在激活之前已写出，推送进同一条先入先出的出站队列。
func TestSelfSyncFirstSnapshotFollowsTheLoginACK(t *testing.T) {
	var squeezed atomic.Int32
	h := newHarness(t, func(h *harness) {
		squeezed.Add(1)
		_ = h.manager.Flush(context.Background())
	})
	h.access.Manager().Add(newWallet(t, 8, 3))
	for round := range 20 {
		c := dial(t, h, 8)
		c.ack(t)
		if kind, state := c.state(t); kind != frame.Full || state != "gold=3" {
			t.Fatalf("round %d: first push = %v %q, want the Full right after the ACK", round, kind, state)
		}
		_ = c.connection.Close()
		waitOnline(t, h, 0)
		h.flush(t)
	}
	if squeezed.Load() != 20 {
		t.Fatalf("the flush ran inside %d handshakes, want 20", squeezed.Load())
	}
}

// 顶号：同一玩家两条连接，新连接到来时两条都收到新 epoch 的全量；旧连接关闭不带走新连接的视图。
func TestSelfSyncSecondConnectionGetsAFullAndSurvivesTheFirstClosing(t *testing.T) {
	h := newHarness(t, nil)
	player := newWallet(t, 9, 1)
	h.access.Manager().Add(player)
	first := dial(t, h, 9)
	first.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, _ := first.state(t); kind != frame.Full {
		t.Fatalf("first connection got %v, want Full", kind)
	}
	second := dial(t, h, 9)
	second.ack(t)
	// 两条连接都已激活后再 tick。
	deadline := time.Now().Add(3 * time.Second)
	for h.runtime.ActiveSessions(9) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("second connection not registered")
		}
		time.Sleep(time.Millisecond)
	}
	h.flush(t)
	if kind, state := second.state(t); kind != frame.Full || state != "gold=1" {
		t.Fatalf("second connection got %v %q, want Full gold=1", kind, state)
	}
	if kind, _ := first.state(t); kind != frame.Full {
		t.Fatalf("first connection got %v after the reset, want Full in a new epoch", kind)
	}
	_ = first.connection.Close()
	for h.runtime.ActiveSessions(9) != 1 {
		time.Sleep(time.Millisecond)
	}
	h.flush(t)
	player.setGold(2)
	h.flush(t)
	if kind, state := second.state(t); kind != frame.Delta || state != "gold=2" {
		t.Fatalf("second connection after the first closed = %v %q, want Delta gold=2", kind, state)
	}
	if h.self.Online() != 1 {
		t.Fatalf("online = %d, want 1", h.self.Online())
	}
}

// 独立 Gate：Game Ingress 的 OnActive / OnClosed 同样登记 / 撤销连接（receiver ID 区分连接）。
func TestSelfSyncIngressCallbacks(t *testing.T) {
	playerKind(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	self := MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID})
	mod := wiringnest.NewModWithEntitySync(access, self.EntitySyncSetup())
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.EntitySync().Close(context.Background()) })
	binding := gateway.Binding{PlayerID: 5}
	if err := self.OnActive(binding, 1); err != nil {
		t.Fatal(err)
	}
	if err := self.OnActive(binding, 2); err != nil {
		t.Fatal(err)
	}
	self.OnClosed(binding, 1)
	if self.Online() != 1 {
		t.Fatalf("online = %d, want 1: receiver 2 is still active", self.Online())
	}
	self.OnClosed(binding, 2)
	if self.Online() != 0 {
		t.Fatalf("online = %d, want 0", self.Online())
	}
}

// 嵌入 TCP：客户端停读、出站队列积满时接入层关闭这条连接；自身视图不在原连接上重开，等连接关闭收尾，
// 客户端重连后从全量开始、拿到停读期间的最新状态。
func TestSelfSyncOverTCPClosesAStalledReaderAndTheReconnectGetsAFull(t *testing.T) {
	h := newHarness(t, nil)
	player := newWallet(t, 7, 0)
	h.access.Manager().Add(player)
	c := dial(t, h, 7)
	c.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, _ := c.state(t); kind != frame.Full {
		t.Fatalf("first state = %v, want Full", kind)
	}
	// 客户端不再读；每次改动的增量带 48KiB 填充（组件上限 64KiB 之内），几十帧后出站队列（字节上限 2MiB）连同 socket 缓冲一起满。
	player.pad.Store(48 << 10)
	gold := int64(0)
	for h.runtime.ActiveSessions(7) > 0 {
		if gold++; gold > 4096 {
			t.Fatalf("a client that stopped reading was never disconnected: %+v last=%v", h.manager.Stats(), h.manager.LastError())
		}
		player.setGold(gold)
		h.flush(t)
	}
	waitOnline(t, h, 0)
	h.flush(t)
	if stats := h.manager.Stats(); stats.Sessions != 0 || stats.Subjects != 0 {
		t.Fatalf("after the stalled connection closed: sessions=%d subjects=%d, want 0/0", stats.Sessions, stats.Subjects)
	}
	player.pad.Store(0)
	again := dial(t, h, 7)
	again.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	want := "gold=" + strconv.FormatInt(gold, 10)
	if kind, state := again.state(t); kind != frame.Full || state != want {
		t.Fatalf("after reconnect = %v %q, want a Full with %s", kind, state, want)
	}
}

func TestSelfSyncAttachesToOneManagerOnly(t *testing.T) {
	playerKind(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	self := MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID})
	first := wiringnest.NewModWithEntitySync(access, self.EntitySyncSetup())
	if err := first.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.EntitySync().Close(context.Background()) })
	second := wiringnest.NewModWithEntitySync(access, self.EntitySyncSetup())
	if err := second.Init(viper.New()); err == nil {
		t.Fatal("one SelfSync attached to two managers")
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	for name, config := range map[string]Config{
		"entities":   {Kind: testKind, MessageID: testMessageID},
		"kind":       {Entities: access, MessageID: testMessageID},
		"message id": {Entities: access, Kind: testKind},
		"frame size": {Entities: access, Kind: testKind, MessageID: testMessageID, MaxFrameBytes: -1},
	} {
		if _, err := New(config); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}

// 推送能力还没接上时整轮重试，不关会话。
func TestTransportRetriesTheTickUntilPlayersAreBound(t *testing.T) {
	playerKind(t)
	self := MustNew(Config{Entities: entity.NewManagerAccess(entity.NewEntityManager()), Kind: testKind, MessageID: testMessageID})
	err := self.EntitySyncSetup().Config.Transport.Push(context.Background(), 1, []byte{1})
	if !errors.Is(err, entitysync.ErrRetryLater) {
		t.Fatalf("push before BindPlayers = %v, want ErrRetryLater", err)
	}
	self.BindPlayers(gateway.NewTCPRuntime(nil, time.Second)) // 未接 server：ErrTransportUnavailable
	err = self.EntitySyncSetup().Config.Transport.Push(context.Background(), 1, []byte{1})
	if !errors.Is(err, entitysync.ErrRetryLater) || !errors.Is(err, gateway.ErrTransportUnavailable) {
		t.Fatalf("push on an unstarted runtime = %v, want ErrRetryLater wrapping ErrTransportUnavailable", err)
	}
}

// fakePlayers 是只实现推送、连接计数与关连接的接入层替身：PushSyncPlayer 返回 refuse 给出的错误，
// CloseSessions 记录被关的玩家与原因。其余方法不会被 Transport 调用（嵌入的 nil 接口调用会 panic）。
type fakePlayers struct {
	gateway.PlayerRuntime
	refuse      error
	connections int
	closed      []int64
	cause       error
}

func (f *fakePlayers) PushSyncPlayer(context.Context, int64, uint32, []byte) error { return f.refuse }
func (f *fakePlayers) ActiveSessions(int64) int                                    { return f.connections }
func (f *fakePlayers) CloseSessions(player int64, cause error) int {
	f.closed, f.cause = append(f.closed, player), cause
	closed := f.connections
	f.connections = 0
	return closed
}

// 推送结果的分类就是慢消费者规则：接入层不可用、整个进程的驻留预算耗尽 → 整轮重试，连接不动；
// 玩家没有连接 → 只交还错误；活着的连接拒收 → 关闭该玩家的全部连接，再交还错误让 Manager 关会话。
// 关连接的原因带着原错误，便于从断线日志找回是哪一类拒收。
func TestTransportClassifiesRefusedPushes(t *testing.T) {
	playerKind(t)
	budget := nettransport.ErrResidentBudgetExhausted
	backpressure := nettransport.ErrReliableBackpressure
	for name, c := range map[string]struct {
		refuse      error
		connections int
		retry       bool
		close       bool
	}{
		"admitted":                        {refuse: nil, connections: 1},
		"transport unavailable":           {refuse: gateway.ErrTransportUnavailable, connections: 1, retry: true},
		"async transport closed":          {refuse: nettransport.ErrTransportClosed, connections: 1, retry: true},
		"resident budget, one connection": {refuse: errors.Join(budget), connections: 1, retry: true},
		"resident budget on every connection": {
			refuse: errors.Join(budget, fmt.Errorf("receiver 2: %w", budget)), connections: 2, retry: true},
		// 另一条连接收下了这一帧：整轮重试会让它把同一 tick 的重发当旧包丢掉，所以按拒收处理。
		"resident budget on one of two connections": {refuse: errors.Join(budget), connections: 2, close: true},
		"no connection":                      {refuse: fmt.Errorf("%w: player 7", gateway.ErrSessionNotFound), connections: 0},
		"queue full on a live connection":    {refuse: errors.Join(backpressure), connections: 1, close: true},
		"stale binding on a live connection": {refuse: errors.Join(gateway.ErrBindingStale), connections: 1, close: true},
		"one connection gone, one refusing": {
			refuse: errors.Join(gateway.ErrSessionNotFound, backpressure), connections: 1, close: true},
	} {
		t.Run(name, func(t *testing.T) {
			players := &fakePlayers{refuse: c.refuse, connections: c.connections}
			self := MustNew(Config{Entities: entity.NewManagerAccess(entity.NewEntityManager()), Kind: testKind, MessageID: testMessageID})
			self.BindPlayers(players)
			err := self.EntitySyncSetup().Config.Transport.Push(context.Background(), 7, []byte{1})
			if c.refuse == nil {
				if err != nil || len(players.closed) != 0 {
					t.Fatalf("admitted push: err=%v closed=%v", err, players.closed)
				}
				return
			}
			if got := errors.Is(err, entitysync.ErrRetryLater); got != c.retry {
				t.Fatalf("push = %v: retry the tick = %v, want %v", err, got, c.retry)
			}
			if !errors.Is(err, c.refuse) && !errors.Is(err, errors.Unwrap(c.refuse)) {
				t.Fatalf("push = %v lost the access layer's error %v", err, c.refuse)
			}
			if c.close {
				if len(players.closed) != 1 || players.closed[0] != 7 {
					t.Fatalf("connections closed for %v, want player 7 closed once: a live connection that refused a frame "+
						"must not stay up without its sync session", players.closed)
				}
				if !errors.Is(players.cause, c.refuse) && !errors.Is(players.cause, errors.Unwrap(c.refuse)) {
					t.Fatalf("close cause %v does not carry the refusal %v", players.cause, c.refuse)
				}
			} else if len(players.closed) != 0 {
				t.Fatalf("connections closed for %v on %v, want none", players.closed, c.refuse)
			}
		})
	}
	// ctx 已结束：Manager 自己整轮重试，传输不能因此断开玩家（停机时不应把在线玩家全部踢掉）。
	players := &fakePlayers{refuse: context.Canceled, connections: 1}
	self := MustNew(Config{Entities: entity.NewManagerAccess(entity.NewEntityManager()), Kind: testKind, MessageID: testMessageID})
	self.BindPlayers(players)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := self.EntitySyncSetup().Config.Transport.Push(ctx, 7, []byte{1}); !errors.Is(err, context.Canceled) || len(players.closed) != 0 {
		t.Fatalf("push with an ended ctx = %v, closed=%v; want the ctx error and no close", err, players.closed)
	}
}

// gaugeValue 读默认注册表里无标签 Gauge 的当前值；没有这个序列时为 0。
func gaugeValue(name string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && len(metric.Labels) == 0 {
			return metric.Value
		}
	}
	return 0
}

// 在线却没有打开的同步会话的玩家数：传输拒收后若连接没有被关闭（违反约定的传输；这里的替身记下关闭但不送
// 关闭通知），超过几个同步周期后指标上升；连接关闭通知到达后归零。
func TestSelfSyncReportsPlayersOnlineWithoutASession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		playerKind(t)
		access := entity.NewManagerAccess(entity.NewEntityManager())
		self := MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID})
		mod := wiringnest.NewModWithEntitySync(access, self.EntitySyncSetup())
		if err := mod.Init(viper.New()); err != nil {
			t.Fatal(err)
		}
		manager := mod.EntitySync()
		defer func() { _ = manager.Close(context.Background()) }()
		players := &fakePlayers{refuse: errors.Join(nettransport.ErrReliableBackpressure), connections: 1}
		self.BindPlayers(players)
		access.Manager().Add(newWallet(t, 7, 1))
		binding := gateway.Binding{PlayerID: 7}
		if err := self.OnActive(binding, 1); err != nil {
			t.Fatal(err)
		}
		flush := func() {
			t.Helper()
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		flush() // 第一帧被拒：关闭连接（替身不送关闭通知）、会话被丢
		flush() // 处理丢会话
		if len(players.closed) != 1 {
			t.Fatalf("connections closed %v, want player 7 once", players.closed)
		}
		if got := gaugeValue(StrandedPlayersMetric); got != 0 {
			t.Fatalf("%s right after the loss = %d, want 0", StrandedPlayersMetric, got)
		}
		time.Sleep(4 * manager.Interval())
		flush()
		if got := gaugeValue(StrandedPlayersMetric); got != 1 || self.Stranded() != 1 {
			t.Fatalf("%s with a player online and no session for 4 periods = %d (Stranded %d), want 1", StrandedPlayersMetric, got, self.Stranded())
		}
		self.OnClosed(binding, 1)
		flush()
		if got := gaugeValue(StrandedPlayersMetric); got != 0 {
			t.Fatalf("%s after the connection closed = %d, want 0", StrandedPlayersMetric, got)
		}
	})
}
