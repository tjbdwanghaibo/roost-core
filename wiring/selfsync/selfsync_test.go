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
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
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
		return entity.TakeFrozenSyncPayload(1, []byte("gold="+strconv.FormatInt(w.gold.Load(), 10))), nil
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
