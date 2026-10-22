// 自身视图接线：真实 gateway TCP（鉴权、出站队列）+ wiring/nest Mod 装出的 Manager + 内存里的玩家实体。
// 客户端按正式 RS v2 帧读：先 ACK，再 Sync 全量，改动后收增量，重连后再收全量。

package selfsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
	return newHarnessWithPayloadLimit(t, 0, afterACK)
}

// newHarnessWithPayloadLimit 是部署写了 player_access.tcp.max_payload_bytes 的装配：同一个键既交给 Nest Mod 的
// Init（与生成工程同一份配置），也是 TCP 服务的载荷上限（生成的 player TCP Mod 从同一个键读）。0 表示不写。
func newHarnessWithPayloadLimit(t testing.TB, payloadLimit uint32, afterACK func(*harness)) *harness {
	t.Helper()
	return newHarnessWithTCP(t, payloadLimit, afterACK, nil)
}

// newHarnessWithTCP 与 newHarnessWithPayloadLimit 相同，configure 非 nil 时在建服务器前再改嵌入 TCP 的配置。
func newHarnessWithTCP(t testing.TB, payloadLimit uint32, afterACK func(*harness), configure func(*gateway.TCPConfig)) *harness {
	t.Helper()
	playerKind(t)
	h := &harness{access: entity.NewManagerAccess(entity.NewEntityManager())}
	self, err := New(Config{Entities: h.access, Kind: testKind, MessageID: testMessageID})
	if err != nil {
		t.Fatal(err)
	}
	h.self = self
	h.mod = wiringnest.NewModWithEntitySync(h.access, self.EntitySyncSetup())
	cfg := viper.New()
	if payloadLimit > 0 {
		cfg.Set("player_access.tcp.max_payload_bytes", payloadLimit)
	}
	if err := h.mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	h.manager = h.mod.EntitySync()
	t.Cleanup(func() { _ = h.manager.Close(context.Background()) })

	config := gateway.DefaultTCPConfig()
	config.Enabled, config.Addr = true, "127.0.0.1:0"
	if payloadLimit > 0 {
		config.MaxPayloadBytes = payloadLimit
	}
	if configure != nil {
		configure(&config)
	}
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
	epoch      uint32 // 最近一帧的 epoch（state 记下）
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
		kind, c.epoch = decoded.Kind, decoded.Epoch
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

// 部署调小 player_access.tcp.max_payload_bytes、不改别的：以前自身视图一帧的上限仍是 gateway 默认的 1MiB，
// 超过接入层上限的帧被拒收、连接被关，客户端重连后的全量同样超限，反复断线重连。现在一帧的上限在 Nest Mod Init
// 时从同一个键取：上限以内照常交付；单个玩家实体超过它时帧在 Manager 内编码失败，不会推出一帧被接入层拒收的帧。
//
// 编码失败丢掉的会话（维护者 2026-10-22：编码失败就关闭连接）：连接没有拒收任何东西，
// 但同步会话已经没了，所以由 SessionLost 断开这名玩家（DisconnectLostPlayer，计 reason=too_large），不计推送拒收；
// 客户端重连后从新 epoch 的全量开始。
func TestSelfSyncFrameLimitFollowsTheTCPPayloadLimit(t *testing.T) {
	const limit = 16 << 10
	h := newHarnessWithPayloadLimit(t, limit, nil)
	if got := h.self.FrameLimit(); got != limit {
		t.Fatalf("frame limit = %d, want player_access.tcp.max_payload_bytes %d", got, limit)
	}
	player := newWallet(t, 7, 0)
	player.pad.Store(8 << 10)
	h.access.Manager().Add(player)
	c := dial(t, h, 7)
	c.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, _ := c.state(t); kind != frame.Full {
		t.Fatalf("first state = %v, want Full", kind)
	}
	firstEpoch := c.epoch
	refusedBefore := counterValue(SyncPushRefusedMetric, "closed")
	disconnectsBefore := labeledCounter(SessionLostDisconnectMetric, "reason", "too_large")
	// 一次改动的帧超过接入层上限（仍在组件上限 64KiB 之内）。
	player.pad.Store(24 << 10)
	player.setGold(1)
	h.flush(t)
	_ = c.connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	packet, err := wire.Read(c.connection, wire.DefaultMaxPayload)
	if err == nil {
		t.Fatalf("a %d-byte frame was pushed over a %d-byte access limit", len(packet.Payload), limit)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the player's sync session was lost to an encode failure (sessions lost %d) and its connection was left up: "+
			"connections=%d, %s{reason=too_large} %d → %d; the client would never receive another frame",
			h.manager.Stats().SessionsLost, h.runtime.ActiveSessions(7), SessionLostDisconnectMetric, disconnectsBefore,
			labeledCounter(SessionLostDisconnectMetric, "reason", "too_large"))
	}
	if got := labeledCounter(SessionLostDisconnectMetric, "reason", "too_large"); got != disconnectsBefore+1 {
		t.Fatalf("%s{reason=too_large} %d → %d, want one disconnect", SessionLostDisconnectMetric, disconnectsBefore, got)
	}
	if got := counterValue(SyncPushRefusedMetric, "closed"); got != refusedBefore {
		t.Fatalf("%s{outcome=closed} %d → %d: a frame over the access limit reached the transport", SyncPushRefusedMetric, refusedBefore, got)
	}
	waitOnline(t, h, 0)
	h.flush(t)
	// 原因消失之后（实体变小）重连：新会话、新 epoch 的全量。
	player.pad.Store(0)
	again := dial(t, h, 7)
	again.ack(t)
	waitOnline(t, h, 1)
	h.flush(t)
	if kind, state := again.state(t); kind != frame.Full || state != "gold=1" {
		t.Fatalf("after reconnect = %v %q, want a Full with gold=1", kind, state)
	}
	if again.epoch == firstEpoch {
		t.Fatalf("the reconnect's Full carries the lost session's epoch %d, want a new one", firstEpoch)
	}
}

// 独立 Gate 部署下帧上限取 Game Ingress 的载荷上限（生成的 IngressMod.WithSelfSync 调 LimitFrameBytes），与
// 接入层配置、Config.MaxFrameBytes 一起取最小；Manager 建好之后不能再改。
func TestSelfSyncFrameLimitTakesTheSmallestAccessLimit(t *testing.T) {
	playerKind(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	self := MustNew(Config{Entities: access, Kind: testKind, MessageID: testMessageID, MaxFrameBytes: 300 << 10})
	if got := self.FrameLimit(); got != 300<<10 {
		t.Fatalf("frame limit with only Config.MaxFrameBytes = %d, want %d", got, 300<<10)
	}
	if err := self.LimitFrameBytes(256 << 10); err != nil {
		t.Fatal(err)
	}
	if err := self.LimitFrameBytes(0); err == nil {
		t.Fatal("a zero ingress limit was accepted")
	}
	mod := wiringnest.NewModWithEntitySync(access, self.EntitySyncSetup())
	cfg := viper.New()
	cfg.Set("player_access.tcp.max_payload_bytes", 512<<10)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.EntitySync().Close(context.Background()) })
	if got := self.FrameLimit(); got != 256<<10 {
		t.Fatalf("frame limit = %d, want the ingress limit %d", got, 256<<10)
	}
	if err := self.LimitFrameBytes(128 << 10); err == nil {
		t.Fatal("LimitFrameBytes after the manager was built was accepted; the manager's frame limit is already fixed")
	}
	declared := false
	for _, key := range mod.ConfigSchema().Keys {
		declared = declared || key.Name == "player_access.tcp.max_payload_bytes"
	}
	if !declared {
		t.Fatal("the Nest Mod's declaration does not carry player_access.tcp.max_payload_bytes, so the App would not check it before Init")
	}
}

// 嵌入 TCP 的全进程出站驻留预算（player_access.tcp.outbound.resident_bytes）耗尽不是哪个玩家慢：与独立 Gate 下
// Game Ingress 的预算同一规则，Sync 整轮重试（ErrRetryLater）、保留脏状态，不关任何连接、不丢会话；占预算的停读连接
// 关闭后预算归还，同一会话照常收到增量。预算由另一个停读的连接占满：推给它的一整帧卡在写协程里，它不读就永远写不完。
func TestSelfSyncOverTCPRetriesTheTickWhenTheResidentBudgetIsExhausted(t *testing.T) {
	const fillerBytes = 16 << 20 // 嵌入 TCP 的 max_payload_bytes 硬上限
	h := newHarnessWithTCP(t, fillerBytes, nil, func(config *gateway.TCPConfig) {
		// 预算恰好一帧（resident_bytes 允许的下限）：占预算的那一帧进队后剩余为 0。每连接字节上限是两帧，
		// 占预算的连接不会先因自己的上限被关。
		config.OutboundResidentBytes = fillerBytes + 16
	})
	player := newWallet(t, 7, 1)
	h.access.Manager().Add(player)
	c := dial(t, h, 7)
	c.ack(t)
	filler := dial(t, h, 8) // 只用来占预算的另一个玩家：没有实体，自身视图不向它推送；它从不读
	_ = filler.connection.(*net.TCPConn).SetReadBuffer(4 << 10)
	waitOnline(t, h, 2)
	h.flush(t)
	if kind, state := c.state(t); kind != frame.Full || state != "gold=1" {
		t.Fatalf("first state = %v %q, want a Full with gold=1", kind, state)
	}

	// 占满预算：给停读连接推一帧 16 MiB。这一帧的字节要整帧写进内核才扣减，而它的接收缓冲 4 KiB、
	// 单个 socket 缓冲有系统上限（macOS kern.ipc.maxsockbuf 8 MiB、自动调优上限 4 MiB；Linux tcp_wmem 缺省上限 4 MiB），
	// 它不读就永远写不完：从这一帧进队到 filler 关闭，剩余预算一直是 0，带 4 KiB 填充的自身视图帧必然被拒，
	// 不靠“停一会儿再探”判断缓冲已满。首帧全量要等写协程写完才扣减，客户端读到它时可能还没扣：这时 filler 被拒，
	// 有界重试到首帧归还。
	player.pad.Store(4 << 10)
	filling := make([]byte, fillerBytes)
	for deadline := time.Now().Add(5 * time.Second); ; {
		err := h.runtime.PushSyncPlayer(context.Background(), 8, testMessageID+1, filling)
		if err == nil {
			break
		}
		if !errors.Is(err, gateway.ErrResidentBudgetExhausted) || time.Now().After(deadline) {
			t.Fatalf("filler push = %v: the first state frame was never returned to the process budget", err)
		}
		time.Sleep(time.Millisecond)
	}
	if h.runtime.ActiveSessions(8) != 1 {
		t.Fatalf("filler connections = %d; want the budget exhausted with the filler still connected", h.runtime.ActiveSessions(8))
	}

	before := counterValue(SyncPushRefusedMetric, "retried")
	player.setGold(2)
	err := h.manager.Flush(context.Background())
	if !errors.Is(err, entitysync.ErrRetryLater) {
		t.Fatalf("flush with the resident budget exhausted = %v, want ErrRetryLater (the tick is retried, nobody is disconnected)", err)
	}
	if got := counterValue(SyncPushRefusedMetric, "retried") - before; got < 1 {
		t.Fatalf("%s{outcome=retried} grew by %d, want at least 1", SyncPushRefusedMetric, got)
	}
	if lost := h.manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("sessions lost = %d, want 0: an exhausted process budget is not the player's failure", lost)
	}
	if online := h.runtime.ActiveSessions(7); online != 1 {
		t.Fatalf("player 7 connections = %d, want 1: no connection is closed for an exhausted process budget", online)
	}

	// 占预算的连接关闭：它排队与卡在写协程里的字节随写协程退出归还，下一轮把 gold=2 作为增量送到同一会话。
	_ = filler.connection.Close()
	waitOnline(t, h, 1)
	player.pad.Store(0) // 之后的状态不带填充，便于比较
	var kind frame.Kind
	var state string
	for deadline := time.Now().Add(3 * time.Second); ; {
		if err := h.manager.Flush(context.Background()); err == nil {
			kind, state = c.state(t)
			break
		} else if !errors.Is(err, entitysync.ErrRetryLater) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the process budget was never returned after the filler connection closed")
		}
		time.Sleep(time.Millisecond)
	}
	// 帧类型不作断言：被拒那一轮若已有帧准入（requireSnapshotsAfterRetry），下一轮按规则改发全量；承诺的是
	// 同一会话、同一连接收到最新状态，期间没有丢会话。
	if state != "gold=2" {
		t.Fatalf("after the budget was returned = %v %q, want gold=2 on the same session", kind, state)
	}
	if lost := h.manager.Stats().SessionsLost; lost != 0 {
		t.Fatalf("sessions lost = %d after the budget recovered, want 0", lost)
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

// fakePlayers 是只实现推送、连接计数与关连接的接入层替身：PushSyncPlayer 返回 refuse 给出的错误（为 nil 时记下帧），
// CloseSessions 记录被关的玩家与原因。其余方法不会被 Transport 调用（嵌入的 nil 接口调用会 panic）。
type fakePlayers struct {
	gateway.PlayerRuntime
	refuse      error
	connections int
	closed      []int64
	cause       error
	frames      [][]byte // 准入的帧（refuse 为 nil 时）
}

func (f *fakePlayers) PushSyncPlayer(_ context.Context, _ int64, _ uint32, payload []byte) error {
	if f.refuse == nil {
		f.frames = append(f.frames, append([]byte(nil), payload...))
	}
	return f.refuse
}
func (f *fakePlayers) ActiveSessions(int64) int { return f.connections }
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
		// 嵌入 TCP 的进程驻留预算（gateway.ErrResidentBudgetExhausted）与 Game Ingress 的同一规则。
		"embedded tcp resident budget, one connection": {refuse: errors.Join(fmt.Errorf("tcpSession a: %w", gateway.ErrResidentBudgetExhausted)), connections: 1, retry: true},
		"embedded tcp resident budget on every connection": {
			refuse: errors.Join(fmt.Errorf("tcpSession a: %w", gateway.ErrResidentBudgetExhausted), fmt.Errorf("tcpSession b: %w", gateway.ErrResidentBudgetExhausted)), connections: 2, retry: true},
		"embedded tcp resident budget on one of two connections": {
			refuse: errors.Join(fmt.Errorf("tcpSession a: %w", gateway.ErrResidentBudgetExhausted)), connections: 2, close: true},
		"no connection": {refuse: fmt.Errorf("%w: player 7", gateway.ErrSessionNotFound), connections: 0},
		// 报“没有连接”时却数得到一条：不能留下它没有同步会话地活着。
		"no connection reported, one counted": {refuse: fmt.Errorf("%w: player 7", gateway.ErrSessionNotFound), connections: 1, close: true},
		"queue full on a live connection":     {refuse: errors.Join(backpressure), connections: 1, close: true},
		"stale binding on a live connection":  {refuse: errors.Join(gateway.ErrBindingStale), connections: 1, close: true},
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
// counterValue 读带 outcome 标签的计数（进程级累计，用前后差值）。
func counterValue(name, outcome string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && metric.Labels["outcome"] == outcome {
			return metric.Value
		}
	}
	return 0
}

// labeledCounter 读带一个标签的计数（进程级累计，用前后差值）。
func labeledCounter(name, label, value string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name && metric.Labels[label] == value {
			return metric.Value
		}
	}
	return 0
}

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

// 会话 ID 就是玩家 ID 的 Manager（自身视图、game-demo 场景）丢会话之后不留下“连接活着、同步会话已死”的玩家：
// 会话持有的对象超过 Limits.MaxObjects（frame.ErrObjectLimit）时帧编不出来、没有交给传输，连接没拒收任何东西；
// DisconnectLostPlayer 断开该玩家（计 reason=object_limit）。客户端重连、重开会话，收到新 epoch 的全量。
// 推送拒收的旧路径不变：PushPlayerFrame 关连接（计 push_refused closed），SessionLost 不再关第二次。
func TestALostSessionDisconnectsThePlayerUnlessThePushWasRefused(t *testing.T) {
	playerKind(t)
	const playerID = 7
	players := &fakePlayers{connections: 1}
	var lost []error
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{
		Name:     "lane",
		Interval: time.Hour,
		Limits:   frame.Limits{MaxObjects: 2},
		Transport: entitysync.TransportFunc(func(ctx context.Context, session entitysync.SessionID, payload []byte) error {
			return PushPlayerFrame(ctx, players, int64(session), testMessageID, payload)
		}),
		SessionLost: func(session entitysync.SessionID, cause error) {
			lost = append(lost, cause)
			DisconnectLostPlayer(players, int64(session), cause)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	flush := func() {
		t.Helper()
		if err := manager.Flush(context.Background()); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	wallets := []*wallet{newWallet(t, 101, 1), newWallet(t, 102, 2), newWallet(t, 103, 3)}
	for _, w := range wallets {
		if err := manager.Register(w.Sync()); err != nil {
			t.Fatal(err)
		}
	}
	subscribe := func(ws ...*wallet) {
		t.Helper()
		for _, w := range ws {
			if err := manager.Subscribe(playerID, w.ID(), policy.SelfProfile); err != nil {
				t.Fatalf("subscribe %d: %v", w.ID(), err)
			}
		}
	}
	firstEpoch := func() uint32 {
		t.Helper()
		if len(players.frames) == 0 {
			t.Fatal("no frame was admitted")
		}
		decoded, err := entitysync.DecodeFrame(players.frames[0], frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Kind != frame.Full {
			t.Fatalf("first frame of the session is %v, want Full", decoded.Kind)
		}
		return decoded.Epoch
	}

	// 1. 两个对象：会话照常收到全量。
	if err := manager.OpenSession(playerID); err != nil {
		t.Fatal(err)
	}
	subscribe(wallets[0], wallets[1])
	flush()
	epoch := firstEpoch()
	players.frames = nil

	// 2. 第三个对象超过 MaxObjects：编码失败，会话被丢，连接被断开。
	disconnectsBefore := labeledCounter(SessionLostDisconnectMetric, "reason", "object_limit")
	refusedBefore := counterValue(SyncPushRefusedMetric, "closed")
	subscribe(wallets[2])
	flush()
	if len(lost) != 1 || entitysync.LossReason(lost[0]) != entitysync.SessionLossObjectLimit || !errors.Is(lost[0], frame.ErrObjectLimit) {
		t.Fatalf("SessionLost causes %v, want one object_limit loss wrapping frame.ErrObjectLimit", lost)
	}
	if len(players.closed) != 1 || players.closed[0] != playerID {
		t.Fatalf("connections closed for %v after the session was lost to %v, want player %d once: "+
			"its connection stays up and receives nothing until the player logs in again", players.closed, lost[0], playerID)
	}
	if !errors.Is(players.cause, ErrSyncSessionLost) || !errors.Is(players.cause, frame.ErrObjectLimit) {
		t.Fatalf("close cause %v, want ErrSyncSessionLost carrying frame.ErrObjectLimit", players.cause)
	}
	if got := labeledCounter(SessionLostDisconnectMetric, "reason", "object_limit"); got != disconnectsBefore+1 {
		t.Fatalf("%s{reason=object_limit} %d → %d, want +1", SessionLostDisconnectMetric, disconnectsBefore, got)
	}
	if got := counterValue(SyncPushRefusedMetric, "closed"); got != refusedBefore {
		t.Fatalf("%s{outcome=closed} %d → %d: an encode failure is not a refused push", SyncPushRefusedMetric, refusedBefore, got)
	}

	// 3. 客户端重连、重开会话（视野回到两个对象之内）：新 epoch 的全量。
	players.connections = 1
	if err := manager.OpenSession(playerID); err != nil {
		t.Fatalf("reopen after the disconnect: %v", err)
	}
	subscribe(wallets[0], wallets[1])
	flush()
	if again := firstEpoch(); again == epoch {
		t.Fatalf("the reopened session's Full carries the lost session's epoch %d, want a new one", epoch)
	}
	players.frames = nil

	// 4. 推送拒收：PushPlayerFrame 关连接一次，SessionLost 不再关第二次、不计会话丢失断开。
	pushDisconnectsBefore := labeledCounter(SessionLostDisconnectMetric, "reason", string(entitysync.SessionLossPushRefused))
	refusedBefore = counterValue(SyncPushRefusedMetric, "closed")
	players.refuse = errors.Join(nettransport.ErrReliableBackpressure)
	wallets[0].setGold(10)
	flush()
	if len(lost) != 2 || entitysync.LossReason(lost[1]) != entitysync.SessionLossPushRefused || !errors.Is(lost[1], nettransport.ErrReliableBackpressure) {
		t.Fatalf("SessionLost causes %v, want a push_refused loss carrying the refusal", lost)
	}
	if len(players.closed) != 2 || !errors.Is(players.cause, ErrSyncPushRefused) {
		t.Fatalf("connections closed %v (last cause %v), want one more close by the push rule", players.closed, players.cause)
	}
	if got := counterValue(SyncPushRefusedMetric, "closed"); got != refusedBefore+1 {
		t.Fatalf("%s{outcome=closed} %d → %d, want +1", SyncPushRefusedMetric, refusedBefore, got)
	}
	if got := labeledCounter(SessionLostDisconnectMetric, "reason", string(entitysync.SessionLossPushRefused)); got != pushDisconnectsBefore {
		t.Fatalf("a refused push was counted as a session-lost disconnect: %d → %d", pushDisconnectsBefore, got)
	}
}
