//go:build integration

package gate_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// lockAuthority 按 App 的单实例锁键语义回答 Matches：锁键是 <prefix>:<实际服务名>:<sid>
// （framework/app/singleton.go 的 singletonKey 与 singletonLiveness.Matches），协议角色名不是别名。
type lockAuthority struct {
	mu      sync.Mutex
	held    map[string]string // "<服务名>:<sid>" → 持锁 incarnation
	queries []string
}

func (a *lockAuthority) Matches(_ context.Context, service string, sid int32, token string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := service + ":" + strconv.Itoa(int(sid))
	ok := a.held[key] == token
	a.queries = append(a.queries, fmt.Sprintf("%s=%t", key, ok))
	return ok, nil
}

func (a *lockAuthority) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.queries)
}

// discoveryBook 是 etcd 发现的替身：每个进程一个 IDiscovery（一个登记拥有者），按 ServiceType 登记与查找。
type discoveryBook struct {
	mu       sync.Mutex
	infos    map[string][]*etcd.ServiceInfo
	searched []string
}
type processDiscovery struct {
	etcd.IDiscovery
	book *discoveryBook
	own  *etcd.ServiceInfo
}

func (d *processDiscovery) Register(_ context.Context, info *etcd.ServiceInfo) error {
	d.book.mu.Lock()
	defer d.book.mu.Unlock()
	d.own = info
	d.book.infos[info.ServiceType] = append(d.book.infos[info.ServiceType], info)
	return nil
}
func (d *processDiscovery) Discover(_ context.Context, serviceType string) ([]*etcd.ServiceInfo, error) {
	d.book.mu.Lock()
	defer d.book.mu.Unlock()
	d.book.searched = append(d.book.searched, serviceType)
	return slices.Clone(d.book.infos[serviceType]), nil
}
func (d *processDiscovery) Deregister(context.Context) error {
	d.book.mu.Lock()
	defer d.book.mu.Unlock()
	if d.own != nil {
		d.book.infos[d.own.ServiceType] = slices.DeleteFunc(d.book.infos[d.own.ServiceType], func(info *etcd.ServiceInfo) bool { return info == d.own })
	}
	return nil
}

// RV-20261116-02：自定义 Gate 服务名（roost add gate edge）或自定义玩家服务名时，Gate 握手按协议角色 gate / game
// 查单实例锁，查不到实际持锁实例（ErrBindingStale），客户端 EOF。这里用正式的 GateMod + GameIngressMod（真实
// NATS、真实 TCP 握手与一次转发），身份替身按实际服务名持锁，三组名字（加上两端都自定义）都要完整握手。
func TestRealGateHandshakeUsesActualServiceNames(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	for _, names := range []struct{ gate, game string }{{"gate", "game"}, {"edge", "game"}, {"gate", "world"}, {"edge", "world"}} {
		t.Run(names.gate+"_"+names.game, func(t *testing.T) {
			gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
			gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
			authority := &lockAuthority{held: map[string]string{
				names.gate + ":11": gateID.Incarnation,
				names.game + ":22": gameID.Incarnation,
			}}
			book := &discoveryBook{infos: map[string][]*etcd.ServiceInfo{}}
			namespace := fmt.Sprintf("roost.gate.names.n%d", time.Now().UnixNano())
			auth := gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
				if ticket != "ticket" {
					return gateway.Principal{}, gateway.ErrUnauthenticated
				}
				return gateway.Principal{PlayerID: 123, ServerID: 22, SessionID: "session"}, nil
			})
			registry := func(service string, id gateway.ProcessIdentity) *app.Registry {
				raw, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{ReconnectBufferBytes: -1})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(raw.Close)
				r := app.NewRegistry(viper.New())
				if err = r.RegisterBatch(
					app.Capability{Name: app.ModSingletonIncarnation, Value: app.SingletonIncarnation{Key: "it:singleton:" + service + ":" + strconv.Itoa(int(id.ServerID)), Sid: id.ServerID, Token: id.Incarnation}},
					app.Capability{Name: app.ModSingletonIdentityChecker, Value: app.SingletonIdentityChecker(authority)},
					app.Capability{Name: mods.ModNatsRaw, Value: fnats.RawClient(raw)},
					app.Capability{Name: mods.ModEtcdDiscov, Value: etcd.IDiscovery(&processDiscovery{book: book})},
				); err != nil {
					t.Fatal(err)
				}
				return r
			}
			options := gatewiring.DefaultOptions()
			options.Config.Namespace = namespace
			options.RegisterDiscovery = true
			options.Authenticator = auth
			options.Ready = func() bool { return true }
			options.GateService, options.GameService = names.gate, names.game

			game := gatewiring.NewGameIngressMod(func(_ context.Context, _ gateway.Session, id, seq uint32, _ wire.PayloadKind, payload []byte) (any, error) {
				return &gateway.TCPResponse{MessageID: id, Sequence: seq, Payload: payload}, nil
			}, nil, options)
			startMod(t, game, registry(names.game, gameID))

			gateOptions := options
			gateOptions.TCP.Enabled = true
			gateOptions.TCP.Addr = "127.0.0.1:0"
			gateOptions.AdvertiseAddr = "127.0.0.1:7000"
			gate := gatewiring.NewGateMod(gateOptions)
			startMod(t, gate, registry(names.gate, gateID))

			book.mu.Lock()
			registered := map[string]int{}
			for service, infos := range book.infos {
				registered[service] = len(infos)
			}
			book.mu.Unlock()
			if registered[names.gate] != 1 || registered[names.game] != 1 || len(registered) != 2 {
				// 不在这里停：继续走握手，同一轮里也看到握手本身的结果。
				t.Errorf("discovery candidates must be registered under the actual service names %s/%s: %v", names.gate, names.game, registered)
			}

			conn, err := net.DialTimeout("tcp", gate.Addr(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if err = wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
				t.Fatal(err)
			}
			ack, err := wire.Read(conn, 8192)
			t.Logf("actual services=%s/%s singleton checks=%v discover=%v", names.gate, names.game, authority.snapshot(), book.searched)
			if err != nil || ack.MsgID != 0 || ack.Seq != 1 {
				t.Fatalf("handshake under service names %s/%s: ACK=%+v err=%v", names.gate, names.game, ack, err)
			}
			if err = wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte("echo")}}, 8192); err != nil {
				t.Fatal(err)
			}
			reply, err := wire.Read(conn, 8192)
			if err != nil || reply.MsgID != 101 || string(reply.Payload) != "echo" {
				t.Fatalf("forward: %+v %v", reply, err)
			}
			for _, query := range authority.snapshot() {
				if query != names.gate+":11=true" && query != names.game+":22=true" {
					t.Fatalf("singleton identity checked under a name that is not an actual service: %v", authority.snapshot())
				}
			}
			book.mu.Lock()
			searched := slices.Clone(book.searched)
			book.mu.Unlock()
			if len(searched) == 0 || slices.ContainsFunc(searched, func(s string) bool { return s != names.game }) {
				t.Fatalf("the Gate must resolve the player service by its actual name %q: %v", names.game, searched)
			}
		})
	}
}

// 自定义名字的 Gate 若没把实际服务名交给接线（Options 还是缺省 gate），Provide 当场拒绝：
// 本端的单实例锁键不是 <prefix>:gate:<sid>，启动后的每次握手都会被拒。
func TestGateRefusesAServiceNameThatIsNotItsSingletonLock(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	raw, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{ReconnectBufferBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	r := app.NewRegistry(viper.New())
	if err = r.RegisterBatch(
		app.Capability{Name: app.ModSingletonIncarnation, Value: app.SingletonIncarnation{Key: "it:singleton:edge:11", Sid: 11, Token: "gate000000000001"}},
		app.Capability{Name: app.ModSingletonIdentityChecker, Value: app.SingletonIdentityChecker(&lockAuthority{})},
		app.Capability{Name: mods.ModNatsRaw, Value: fnats.RawClient(raw)},
	); err != nil {
		t.Fatal(err)
	}
	options := gatewiring.DefaultOptions()
	options.TCP.Enabled = true
	options.TCP.Addr = "127.0.0.1:0"
	options.Authenticator = gateway.AuthenticatorFunc(func(context.Context, string, net.Addr) (gateway.Principal, error) {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	})
	options.ResolveGame = func(context.Context, int32) (gateway.ProcessIdentity, error) { return gateway.ProcessIdentity{}, nil }
	options.Ready = func() bool { return true }
	gate := gatewiring.NewGateMod(options)
	if err := gate.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := gate.Provide(r); err == nil || !strings.Contains(err.Error(), `holds singleton lock "it:singleton:edge:11" but is wired as gate service "gate"`) {
		t.Fatalf("a Gate whose singleton lock is edge:11 started as service gate: %v", err)
	}
}

type stoppableMod interface {
	Init(*viper.Viper) error
	Provide(*app.Registry) error
	Start() error
	StopWithContext(context.Context) error
}

func startMod(t *testing.T, mod stoppableMod, registry *app.Registry) {
	t.Helper()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mod.StopWithContext(ctx); err != nil {
			t.Error(err)
		}
	})
}
