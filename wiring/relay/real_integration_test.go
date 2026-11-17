//go:build integration

package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	domain "github.com/tjbdwanghaibo/roost-core/service/relay"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	kitetcd "github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
)

// 真实依赖：私有环境的 NATS（ROOST_DATAENGINE_IT_NATS_URL，scripts/mirror-local.sh）与用例自己起的单节点 etcd（PATH 上的 etcd）。
// 正式的 NATS Mod、etcd Mod、relay Mod 与 Server：报告经真实 NATS 到 <subject>.events，lobby 的 leave 命令经真实 NATS 送达，
// etcd 广播键随房间数更新、停机后消失。
//
//	source ~/.roost-it/roost-dataengine-it/env.sh && go test -tags integration -run TestRealRelay ./wiring/relay/
func TestRealRelayReportsCommandsAndAdvertisement(t *testing.T) {
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	etcdEndpoint := startPrivateEtcd(t)
	t.Setenv(testKeyEnv, testKey)
	suffix := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	subject, prefix := "relayit"+suffix+".lobby", "/relay-it-"+suffix+"/relays/"

	cfg := viper.New()
	cfg.Set("sid", 9)
	cfg.Set("server_type", "relay")
	cfg.Set("nats.url", natsURL)
	cfg.Set("nats.prefix", "relayit"+suffix)
	cfg.Set("etcd.endpoints", []string{etcdEndpoint})
	cfg.Set("relay.listen", "127.0.0.1:0")
	cfg.Set("relay.advertise_addr", "relay.it:7100")
	cfg.Set("relay.token_key_env", testKeyEnv)
	cfg.Set("relay.subject", subject)
	cfg.Set("relay.registry_prefix", prefix)
	cfg.Set("relay.registry_ttl", "3s")
	cfg.Set("relay.heartbeat_interval", "200ms")

	registry := app.NewRegistry(cfg)
	natsMod, etcdMod, relayMod := kitnats.NewNatsMod(nil), kitetcd.NewEtcdMod(), NewMod()
	for _, mod := range []app.Mod{natsMod, etcdMod, relayMod} {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s init: %v", mod.Name(), err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatalf("%s provide: %v", mod.Name(), err)
		}
		if err := mod.Start(); err != nil {
			t.Fatalf("%s start: %v", mod.Name(), err)
		}
	}
	t.Cleanup(func() {
		_ = etcdMod.StopWithContext(context.Background())
		_ = natsMod.StopWithContext(context.Background())
	})

	// lobby 一侧：订阅报告主题。
	raw, _ := app.Lookup[fnats.RawClient](registry, mods.ModNatsRaw)
	var mu sync.Mutex
	var reports []domain.Report
	sub, err := raw.SubscribeBounded(EventSubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		var report domain.Report
		if json.Unmarshal(msg.Data, &report) == nil {
			mu.Lock()
			reports = append(reports, report)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.DrainContext(context.Background())
	waitReport := func(match func(domain.Report) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			found := slices.ContainsFunc(reports, match)
			mu.Unlock()
			if found {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no matching report among %+v", reports)
	}
	client, _ := app.Lookup[fetcd.IEtcd](registry, mods.ModEtcd)
	waitAdvert := func(want Advertisement) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			kv, err := client.Get(context.Background(), prefix+"relay-9")
			if err == nil && kv != nil {
				last = kv.Value
				var got Advertisement
				if json.Unmarshal([]byte(kv.Value), &got) == nil && got == want {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("etcd advertisement %q, want %+v", last, want)
	}

	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	waitAdvert(Advertisement{Instance: "relay-9", TCPAddr: "relay.it:7100", Rooms: 0})

	signer := func(account string, role token.Role) string {
		signed, err := token.Sign([]byte(testKey), token.Claims{Relay: "relay-9", Room: "room-it", Account: account, Role: role, MaxMembers: 4, ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	host := dialWithToken(t, server.Addr(), signer("host", token.RoleHost))
	member := dialWithToken(t, server.Addr(), signer("acc-a", token.RoleMember))
	host.expectControl(t, wire.RelayOpMemberJoined, nil)
	waitAdvert(Advertisement{Instance: "relay-9", TCPAddr: "relay.it:7100", Rooms: 1})
	waitReport(func(r domain.Report) bool {
		return r.Type == domain.EventMemberJoined && r.Relay == "relay-9" && r.Account == "acc-a" && slices.Equal(r.Online, []string{"host", "acc-a"})
	})
	waitReport(func(r domain.Report) bool { return r.Type == domain.EventHeartbeat && len(r.Online) == 2 })

	// 转发经真实 TCP。
	if err := wire.WriteRelay(member.conn, []*wire.Packet{{Flags: wire.FlagRelayGame, MsgID: 5, Payload: []byte("hello host")}}, 0); err != nil {
		t.Fatal(err)
	}
	host.expectData(t, wire.RelayGame, 5, 2, "hello host")

	// lobby 的 leave 命令经真实 NATS。
	command, _ := json.Marshal(map[string]string{"type": "leave", "room": "room-it", "account": "acc-a"})
	if err := raw.PublishOnce(CommandSubject(subject, "relay-9"), command); err != nil {
		t.Fatal(err)
	}
	member.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonLeft)})
	waitReport(func(r domain.Report) bool {
		return r.Type == domain.EventMemberLeft && r.Account == "acc-a" && r.Reason == "left"
	})

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(stopCtx); err != nil {
		t.Fatal(err)
	}
	host.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonShutdown)})
	waitReport(func(r domain.Report) bool { return r.Type == domain.EventClosed && r.Reason == "shutdown" })
	if kv, err := client.Get(context.Background(), prefix+"relay-9"); !errors.Is(err, fetcd.ErrKeyNotFound) {
		t.Fatalf("advertisement after shutdown: %+v %v", kv, err)
	}
}

func dialWithToken(t *testing.T, addr net.Addr, signed string) *relayClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
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

// startPrivateEtcd 在空闲端口上起一个单节点 etcd（与 wiring/etcd 的 singleton 用例同一写法），不碰共享环境。
func startPrivateEtcd(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary not on PATH")
	}
	port := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}
	clientURL, peerURL := fmt.Sprintf("http://127.0.0.1:%d", port()), fmt.Sprintf("http://127.0.0.1:%d", port())
	cmd := exec.Command(binary, "--name", "roost-relay-it", "--data-dir", t.TempDir(),
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "roost-relay-it="+peerURL, "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	endpoint := strings.TrimPrefix(clientURL, "http://")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return endpoint
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("etcd did not start")
	return ""
}
