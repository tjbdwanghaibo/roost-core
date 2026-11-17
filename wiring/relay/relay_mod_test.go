package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	domain "github.com/tjbdwanghaibo/roost-core/service/relay"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

const testKeyEnv = "ROOST_RELAY_WIRING_TEST_KEY"
const testKey = "wiring-test-key-0123456789"

func testConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("sid", 3)
	cfg.Set("server_type", "relay")
	cfg.Set("relay.listen", "127.0.0.1:0")
	cfg.Set("relay.advertise_addr", "relay.test:7100")
	cfg.Set("relay.token_key_env", testKeyEnv)
	cfg.Set("relay.subject", "t.lobby")
	cfg.Set("relay.registry_prefix", "/t/relays/")
	cfg.Set("relay.heartbeat_interval", "50ms")
	return cfg
}

func TestModInitReadsConfigAndKey(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	mod := NewMod()
	if err := mod.Init(testConfig()); err != nil {
		t.Fatal(err)
	}
	if mod.domain.Instance != "relay-3" || string(mod.domain.TokenKey) != testKey || mod.domain.MaxPayloadBytes != domain.DefaultMaxPayloadBytes ||
		mod.domain.HeartbeatInterval != 50*time.Millisecond || mod.domain.EmptyRoomTimeout != domain.DefaultEmptyRoomTimeout {
		t.Fatalf("domain config %+v", mod.domain)
	}

	for name, mutate := range map[string]func(*viper.Viper){
		"key env unset":       func(c *viper.Viper) { c.Set("relay.token_key_env", "ROOST_RELAY_WIRING_TEST_UNSET") },
		"wildcard subject":    func(c *viper.Viper) { c.Set("relay.subject", "t.*") },
		"bad instance":        func(c *viper.Viper) { c.Set("relay.instance", "relay.1") },
		"no listen":           func(c *viper.Viper) { c.Set("relay.listen", "") },
		"room bytes too low":  func(c *viper.Viper) { c.Set("relay.room_bytes_per_second", 1024) },
		"negative idle":       func(c *viper.Viper) { c.Set("relay.idle_timeout", "-1s") },
		"no registry prefix":  func(c *viper.Viper) { c.Set("relay.registry_prefix", "") },
		"queue below 2 frame": func(c *viper.Viper) { c.Set("relay.send_queue_bytes", 1000) },
	} {
		cfg := testConfig()
		mutate(cfg)
		if err := NewMod().Init(cfg); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// 声明与读取一致（同 wiring/config_schema_test.go 的规则）：每个声明的键写成读不出来的值，Init 都点名它；
// 声明与 App 的声明合并不冲突；App 启动前的检查接受一份完整配置。
func TestModLoadsWhatItDeclares(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	schema := NewMod().ConfigSchema()
	wrong := map[configschema.Kind]any{configschema.KindInt: "8k", configschema.KindDuration: 15, configschema.KindString: []any{"a"}}
	checked := 0
	for _, key := range schema.Keys {
		value, ok := wrong[key.Kind]
		if !ok {
			continue
		}
		cfg := testConfig()
		cfg.Set(key.Name, value)
		if err := NewMod().Init(cfg); err == nil || !strings.Contains(err.Error(), key.Name) {
			t.Errorf("%s = %v: Init = %v; want a refusal naming the key", key.Name, value, err)
		}
		checked++
	}
	if checked < 20 {
		t.Fatalf("only %d keys checked", checked)
	}
	if _, err := configschema.Merge(app.AppConfigSchema(), schema); err != nil {
		t.Fatal(err)
	}
	if err := app.CheckConfig(testConfig(), NewMod()); err != nil {
		t.Fatal(err)
	}
}

// 正式链路：Mod.Init → Provide → Server.Init → Serve。客户端经 TCP 进房，报告发到 <subject>.events，
// lobby 的 leave 命令从 <subject>.relay.<instance> 收，etcd 广播随房间数更新、租约丢了重新登记，停机撤销。
func TestServerReportsAdvertisesAndTakesLobbyCommands(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	nats := newFakeNats()
	etcd := newFakeEtcd()
	cfg := testConfig()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModNatsRaw, fnats.RawClient(nats)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModEtcd, fetcd.IEtcd(etcd)); err != nil {
		t.Fatal(err)
	}
	mod := NewMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if relay, ok := app.Lookup[*domain.Server](registry, CapabilityName); !ok || relay == nil {
		t.Fatal("relay capability missing")
	}
	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()

	const key = "/t/relays/relay-3"
	etcd.waitValue(t, key, Advertisement{Instance: "relay-3", TCPAddr: "relay.test:7100", Rooms: 0})

	host := dialRelay(t, server.Addr().String(), "room-1", "host", token.RoleHost)
	nats.waitReport(t, "t.lobby.events", func(r domain.Report) bool {
		return r.Type == domain.EventHeartbeat && r.Seq == 1 && r.Relay == "relay-3" && r.Room == "room-1" && slices.Equal(r.Online, []string{"host"})
	})
	etcd.waitValue(t, key, Advertisement{Instance: "relay-3", TCPAddr: "relay.test:7100", Rooms: 1})
	member := dialRelay(t, server.Addr().String(), "room-1", "acc-a", token.RoleMember)
	nats.waitReport(t, "t.lobby.events", func(r domain.Report) bool {
		return r.Type == domain.EventMemberJoined && r.Account == "acc-a" && slices.Equal(r.Online, []string{"host", "acc-a"})
	})
	nats.waitReport(t, "t.lobby.events", func(r domain.Report) bool { // 定时心跳
		return r.Type == domain.EventHeartbeat && r.Room == "room-1" && len(r.Online) == 2
	})

	// lobby 的 leave 命令。
	nats.deliver(t, "t.lobby.relay.relay-3", `{"type":"leave","room":"room-1","account":"acc-a"}`)
	member.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonLeft)})
	nats.waitReport(t, "t.lobby.events", func(r domain.Report) bool {
		return r.Type == domain.EventMemberLeft && r.Account == "acc-a" && r.Reason == "left"
	})
	nats.deliver(t, "t.lobby.relay.relay-3", `not json`) // 坏命令丢弃，不影响别的

	// 租约丢了：重新申请并重写键。
	etcd.loseLease()
	etcd.waitValue(t, key, Advertisement{Instance: "relay-3", TCPAddr: "relay.test:7100", Rooms: 1})
	if grants := etcd.grantCount(); grants < 2 {
		t.Fatalf("lease was not re-granted: %d grants", grants)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	host.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonShutdown)})
	if _, ok := etcd.value(key); ok {
		t.Fatal("etcd advertisement survived shutdown")
	}
	if !nats.drained() {
		t.Fatal("lobby command subscription was not drained")
	}
}

type relayClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialRelay(t *testing.T, addr, room, account string, role token.Role) *relayClient {
	t.Helper()
	signed, err := token.Sign([]byte(testKey), token.Claims{Relay: "relay-3", Room: room, Account: account, Role: role, MaxMembers: 4, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &relayClient{conn: conn, reader: bufio.NewReader(conn)}
	if err := wire.WriteRelay(conn, []*wire.Packet{{Payload: []byte(signed)}}, 0); err != nil {
		t.Fatal(err)
	}
	c.expectControl(t, wire.RelayOpWelcome, nil)
	return c
}

// expectControl 跳过别的帧，直到读到操作码 op 的控制帧；payload 非 nil 时核对载荷。
func (c *relayClient) expectControl(t *testing.T, op uint32, payload []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		p, err := wire.ReadRelay(c.reader, 0)
		if err != nil {
			t.Fatalf("waiting for op %d: %v", op, err)
		}
		if p.RelayChannel() == wire.RelayControl && p.MsgID == op {
			if payload != nil && string(p.Payload) != string(payload) {
				t.Fatalf("op %d payload %v, want %v", op, p.Payload, payload)
			}
			return
		}
	}
}

type fakeNats struct {
	mu        sync.Mutex
	published []published
	handlers  map[string]fnats.MsgHandler
	drainedOK bool
}

type published struct {
	subject string
	data    []byte
}

func newFakeNats() *fakeNats { return &fakeNats{handlers: map[string]fnats.MsgHandler{}} }

func (f *fakeNats) RequestContext(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
func (f *fakeNats) PublishOnce(subject string, data []byte) error {
	f.mu.Lock()
	f.published = append(f.published, published{subject, append([]byte(nil), data...)})
	f.mu.Unlock()
	return nil
}
func (f *fakeNats) MaxPayload() (int64, error)         { return 1 << 20, nil }
func (f *fakeNats) FlushContext(context.Context) error { return nil }
func (f *fakeNats) SubscribeBounded(subject string, _ fnats.PendingLimits, handler fnats.MsgHandler) (fnats.DrainSubscription, error) {
	f.mu.Lock()
	f.handlers[subject] = handler
	f.mu.Unlock()
	return fakeSub{f}, nil
}

func (f *fakeNats) deliver(t *testing.T, subject, data string) {
	t.Helper()
	f.mu.Lock()
	handler := f.handlers[subject]
	f.mu.Unlock()
	if handler == nil {
		t.Fatalf("nothing subscribed to %s", subject)
	}
	handler(&fnats.Msg{Subject: subject, Data: []byte(data)})
}

func (f *fakeNats) waitReport(t *testing.T, subject string, match func(domain.Report) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, p := range f.published {
			var report domain.Report
			if p.subject == subject && json.Unmarshal(p.data, &report) == nil && match(report) {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var seen []string
	for _, p := range f.published {
		seen = append(seen, p.subject+" "+string(p.data))
	}
	t.Fatalf("no matching report on %s among:\n%s", subject, strings.Join(seen, "\n"))
}

func (f *fakeNats) drained() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainedOK
}

type fakeSub struct{ f *fakeNats }

func (s fakeSub) Unsubscribe() error { return nil }
func (s fakeSub) IsValid() bool      { return true }
func (s fakeSub) DrainContext(context.Context) error {
	s.f.mu.Lock()
	s.f.drainedOK = true
	s.f.mu.Unlock()
	return nil
}

// fakeEtcd 只实现广播用到的租约与写键；其余方法不该被调用（嵌入的 nil 接口会 panic）。
type fakeEtcd struct {
	fetcd.IEtcd
	mu     sync.Mutex
	next   int64
	grants int
	lost   map[int64]chan struct{}
	keys   map[string]leasedValue
}

type leasedValue struct {
	value string
	lease int64
}

func newFakeEtcd() *fakeEtcd {
	return &fakeEtcd{lost: map[int64]chan struct{}{}, keys: map[string]leasedValue{}}
}

func (f *fakeEtcd) Grant(context.Context, int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.grants++
	f.lost[f.next] = make(chan struct{})
	return f.next, nil
}

func (f *fakeEtcd) KeepAlive(_ context.Context, lease int64) (<-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lost[lease], nil
}

func (f *fakeEtcd) PutWithLease(_ context.Context, key, value string, lease int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, live := f.lost[lease]; !live {
		return errors.New("lease not found")
	}
	f.keys[key] = leasedValue{value, lease}
	return nil
}

func (f *fakeEtcd) Revoke(_ context.Context, lease int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLeaseLocked(lease)
	return nil
}

func (f *fakeEtcd) dropLeaseLocked(lease int64) {
	if ch, ok := f.lost[lease]; ok {
		close(ch)
		delete(f.lost, lease)
	}
	for key, v := range f.keys {
		if v.lease == lease {
			delete(f.keys, key)
		}
	}
}

// loseLease 模拟租约过期：键消失、续约信号结束。
func (f *fakeEtcd) loseLease() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for lease := range f.lost {
		f.dropLeaseLocked(lease)
	}
}

func (f *fakeEtcd) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants
}

func (f *fakeEtcd) value(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.keys[key]
	return v.value, ok
}

func (f *fakeEtcd) waitValue(t *testing.T, key string, want Advertisement) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if value, ok := f.value(key); ok {
			var got Advertisement
			if json.Unmarshal([]byte(value), &got) == nil && got == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	value, _ := f.value(key)
	t.Fatalf("etcd %s = %q, want %+v", key, value, want)
}
