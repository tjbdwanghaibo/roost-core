//go:build integration

package relay

import (
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/service/lobby"
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

	// lobby 一侧的验权：这条用例只看报告、命令与广播，一律放行（真正的 lobby 验权见 TestRealRelayHostChangeKickAndVerification）。
	verify, err := raw.SubscribeBounded(VerifySubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		_ = raw.PublishOnce(msg.Reply, []byte(`{"ok":true}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer verify.DrainContext(context.Background())

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
	host := dialWithToken(t, server.Addr().String(), signer("host", token.RoleHost))
	host.expectControl(t, wire.RelayOpWelcome, nil)
	member := dialWithToken(t, server.Addr().String(), signer("acc-a", token.RoleMember))
	member.expectControl(t, wire.RelayOpWelcome, nil)
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

// 真实 NATS（+ 私有 etcd，有 ROOST_DATAENGINE_IT_REDIS_ADDR 时房间目录放真实 Redis）上的 relay 与 lobby（service/lobby
// 的验权与报告处理，经 NATS 订阅）：每条连接经 <subject>.verify 验权；主动转交换 host 后 lobby 收到 room.host_changed、
// 改写房主；新房主踢掉旧房主后 lobby 记进被踢名单，join 被拒，旧令牌也连不上；lobby 不应答时新连接被拒。
//
//	source ~/.roost-it/roost-dataengine-it/env.sh && go test -tags integration -run TestRealRelay ./wiring/relay/
func TestRealRelayHostChangeKickAndVerification(t *testing.T) {
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	etcdEndpoint := startPrivateEtcd(t)
	t.Setenv(testKeyEnv, testKey)
	suffix := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	subject := "relayhost" + suffix + ".lobby"

	cfg := viper.New()
	cfg.Set("sid", 9)
	cfg.Set("server_type", "relay")
	cfg.Set("nats.url", natsURL)
	cfg.Set("nats.prefix", "relayhost"+suffix)
	cfg.Set("etcd.endpoints", []string{etcdEndpoint})
	cfg.Set("relay.listen", "127.0.0.1:0")
	cfg.Set("relay.advertise_addr", "relay.it:7100")
	cfg.Set("relay.token_key_env", testKeyEnv)
	cfg.Set("relay.subject", subject)
	cfg.Set("relay.registry_prefix", "/relay-host-it-"+suffix+"/relays/")
	cfg.Set("relay.admit_timeout", "500ms")

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
	raw, _ := app.Lookup[fnats.RawClient](registry, mods.ModNatsRaw)

	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	t.Cleanup(func() { // 先于 NATS / etcd Mod 的清理执行（后进先出）
		cancel()
		<-served
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(stopCtx)
	})

	// lobby：房间目录（真实 Redis 或内存）、报告与验权都经真实 NATS。
	var rooms lobby.RoomStore
	var err error
	if addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"); addr != "" {
		prefix := redistest.Prefix(t, addr, "roost:relayit:{lobby}")
		client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if rooms, err = lobby.NewRedisRooms(client, prefix, time.Hour); err != nil {
			t.Fatal(err)
		}
	} else if rooms, err = lobby.NewMemoryRooms(time.Hour, time.Now); err != nil {
		t.Fatal(err)
	}
	service, err := lobby.New(lobby.Config{
		Games: map[string]lobby.GameRule{"party": {MaxMembers: 4}}, Rooms: rooms,
		Tokens:   bridgeTokens{"gt-host": "host", "gt-alice": "acc-alice"},
		Relays:   bridgeRelays{{Instance: "relay-9", TCPAddr: server.Addr().String()}},
		Commands: rawPublisher{raw}, Subject: subject, RelayTokenKey: []byte(testKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := raw.SubscribeBounded(EventSubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		if err := service.HandleRelayEventData(context.Background(), msg.Data); err != nil {
			t.Errorf("lobby event: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer events.DrainContext(context.Background())
	verify, err := raw.SubscribeBounded(lobby.VerifySubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		_ = raw.PublishOnce(msg.Reply, service.HandleRelayVerifyData(context.Background(), msg.Data))
	})
	if err != nil {
		t.Fatal(err)
	}
	flushCtx, flushed := context.WithTimeout(ctx, 5*time.Second)
	defer flushed()
	if err := raw.FlushContext(flushCtx); err != nil {
		t.Fatal(err)
	}
	waitRoom := func(id string, match func(lobby.Room) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			room, _, err := rooms.Get(context.Background(), id)
			if err == nil && match(room) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("lobby room %+v (%v)", room, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	created, err := service.CreateRoom(ctx, "gt-host", lobby.CreateRequest{Game: "party", Mode: "duel"})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Room.ID
	host := dialWithToken(t, created.RelayAddr, created.Token)
	host.expectControl(t, wire.RelayOpWelcome, nil)
	joined, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatal(err)
	}
	alice := dialWithToken(t, joined.RelayAddr, joined.Token)
	alice.expectControl(t, wire.RelayOpWelcome, nil)
	host.expectControl(t, wire.RelayOpMemberJoined, nil)

	// 主动转交：alice 接手，lobby 收到 room.host_changed，房主改成 alice、epoch 1。
	if err := wire.WriteRelay(host.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpTransfer, Seq: 2, Payload: []byte("state")}}, 0); err != nil {
		t.Fatal(err)
	}
	alice.expectControl(t, wire.RelayOpYouAreHost, nil)
	if err := wire.WriteRelay(alice.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostReady, Payload: wire.EncodeRelayEpoch(1)}}, 0); err != nil {
		t.Fatal(err)
	}
	host.expectControl(t, wire.RelayOpHostChanged, wire.EncodeRelayHostChanged(wire.RelayHostChanged{Epoch: 1, Account: "acc-alice"}))
	waitRoom(id, func(r lobby.Room) bool { return r.Host == "acc-alice" && r.HostEpoch == 1 })
	if _, err := service.SetRoomState(ctx, "gt-host", id, lobby.StateRequest{Game: "party", State: lobby.StateStarted}); !errors.Is(err, lobby.ErrNotHost) {
		t.Fatalf("old host changing state: %v", err)
	}

	// 新房主踢掉旧房主：lobby 记进被踢名单；join 被拒；旧的 host 令牌（还没过期）也连不上。
	if err := wire.WriteRelay(alice.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpKick, Seq: 1}}, 0); err != nil {
		t.Fatal(err)
	}
	host.expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonKicked)})
	waitRoom(id, func(r lobby.Room) bool { return slices.Contains(r.Kicked, "host") })
	if _, err := service.JoinRoom(ctx, "gt-host", id, lobby.JoinRequest{Game: "party"}); !errors.Is(err, lobby.ErrKicked) {
		t.Fatalf("kicked old host joining: %v", err)
	}
	dialWithToken(t, created.RelayAddr, created.Token).expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonKicked)}) // relay 本地先挡

	// lobby 不应答（验权订阅撤掉）：alice 用新令牌重连也进不来，失败即关闭；已连上的 alice 不受影响。
	if err := verify.DrainContext(flushCtx); err != nil {
		t.Fatal(err)
	}
	rejoined, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatal(err)
	}
	dialWithToken(t, rejoined.RelayAddr, rejoined.Token).expectControl(t, wire.RelayOpClosed, []byte{byte(wire.RelayReasonLobbyUnavailable)})
	if err := wire.WriteRelay(alice.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPing, Payload: []byte("ok")}}, 0); err != nil {
		t.Fatal(err)
	}
	alice.expectControl(t, wire.RelayOpPong, []byte("ok"))
}

// 开局后掉线的席位（方案 §6 第 12 条）在真实 NATS（+ 私有 etcd，有 ROOST_DATAENGINE_IT_REDIS_ADDR 时目录放真实 Redis）上：
// 房间已开始，alice 断开连接，relay 报 member_left；lobby 的时钟拨过预留期后她仍占着席位——新人 join 被拒（room_started），
// 她再 join 回到原座、拿新令牌，relay 向 lobby 验权通过，她重新连上，房主收到 member_joined。
//
//	source ~/.roost-it/roost-dataengine-it/env.sh && go test -tags integration -run TestRealRelay ./wiring/relay/
func TestRealRelayStartedRoomKeepsADroppedMembersSeat(t *testing.T) {
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	etcdEndpoint := startPrivateEtcd(t)
	t.Setenv(testKeyEnv, testKey)
	suffix := fmt.Sprintf("%d%d", os.Getpid(), time.Now().UnixNano())
	subject := "relayseat" + suffix + ".lobby"

	cfg := viper.New()
	cfg.Set("sid", 9)
	cfg.Set("server_type", "relay")
	cfg.Set("nats.url", natsURL)
	cfg.Set("nats.prefix", "relayseat"+suffix)
	cfg.Set("etcd.endpoints", []string{etcdEndpoint})
	cfg.Set("relay.listen", "127.0.0.1:0")
	cfg.Set("relay.advertise_addr", "relay.it:7100")
	cfg.Set("relay.token_key_env", testKeyEnv)
	cfg.Set("relay.subject", subject)
	cfg.Set("relay.registry_prefix", "/relay-seat-it-"+suffix+"/relays/")
	cfg.Set("relay.host_idle_timeout", "30s") // 房主不发心跳，别让它在用例里被当成断线

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
	raw, _ := app.Lookup[fnats.RawClient](registry, mods.ModNatsRaw)

	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(stopCtx)
	})

	var rooms lobby.RoomStore
	var err error
	if addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"); addr != "" {
		prefix := redistest.Prefix(t, addr, "roost:relayseat:{lobby}")
		client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if rooms, err = lobby.NewRedisRooms(client, prefix, time.Hour); err != nil {
			t.Fatal(err)
		}
	} else if rooms, err = lobby.NewMemoryRooms(time.Hour, time.Now); err != nil {
		t.Fatal(err)
	}
	// lobby 的时钟可以往前拨（越过席位预留期）；往前拨只让它签的令牌更晚过期，relay 按真实时间验签不受影响。
	var skew atomic.Int64
	service, err := lobby.New(lobby.Config{
		Games: map[string]lobby.GameRule{"party": {MaxMembers: 2}}, Rooms: rooms,
		Tokens:   bridgeTokens{"gt-host": "host", "gt-alice": "acc-alice", "gt-bob": "acc-bob"},
		Relays:   bridgeRelays{{Instance: "relay-9", TCPAddr: server.Addr().String()}},
		Commands: rawPublisher{raw}, Subject: subject, RelayTokenKey: []byte(testKey),
		Now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) },
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := raw.SubscribeBounded(EventSubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		if err := service.HandleRelayEventData(context.Background(), msg.Data); err != nil {
			t.Errorf("lobby event: %v", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer events.DrainContext(context.Background())
	verify, err := raw.SubscribeBounded(lobby.VerifySubject(subject), fnats.PendingLimits{Messages: 1024, Bytes: 1 << 20}, func(msg *fnats.Msg) {
		_ = raw.PublishOnce(msg.Reply, service.HandleRelayVerifyData(context.Background(), msg.Data))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer verify.DrainContext(context.Background())
	flushCtx, flushed := context.WithTimeout(ctx, 5*time.Second)
	defer flushed()
	if err := raw.FlushContext(flushCtx); err != nil {
		t.Fatal(err)
	}
	waitRoom := func(id string, match func(lobby.Room) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			room, _, err := rooms.Get(context.Background(), id)
			if err == nil && match(room) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("lobby room %+v (%v)", room, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	member := func(r lobby.Room, account string) (lobby.Member, bool) {
		for _, m := range r.Members {
			if m.Account == account {
				return m, true
			}
		}
		return lobby.Member{}, false
	}

	created, err := service.CreateRoom(ctx, "gt-host", lobby.CreateRequest{Game: "party", Mode: "duel"})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Room.ID
	host := dialWithToken(t, created.RelayAddr, created.Token)
	host.expectControl(t, wire.RelayOpWelcome, nil)
	joined, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatal(err)
	}
	alice := dialWithToken(t, joined.RelayAddr, joined.Token)
	alice.expectControl(t, wire.RelayOpWelcome, nil)
	host.expectControl(t, wire.RelayOpMemberJoined, nil)
	waitRoom(id, func(r lobby.Room) bool { m, ok := member(r, "acc-alice"); return ok && m.Online })
	if _, err := service.SetRoomState(ctx, "gt-host", id, lobby.StateRequest{Game: "party", State: lobby.StateStarted}); err != nil {
		t.Fatal(err)
	}

	// alice 掉线：relay 报 member_left，lobby 记她离线但席位留着。
	_ = alice.conn.Close()
	host.expectControl(t, wire.RelayOpMemberLeft, nil)
	waitRoom(id, func(r lobby.Room) bool { m, ok := member(r, "acc-alice"); return ok && !m.Online })

	// 拨过预留期：新人被拒，alice 回座（满员 2/2 也不算新人）、拿新令牌，relay 验权通过，她重新连上。
	skew.Store(int64(10 * time.Minute))
	if _, err := service.JoinRoom(ctx, "gt-bob", id, lobby.JoinRequest{Game: "party"}); !errors.Is(err, lobby.ErrRoomStarted) {
		t.Fatalf("newcomer into the started room: %v, want ErrRoomStarted", err)
	}
	back, err := service.JoinRoom(ctx, "gt-alice", id, lobby.JoinRequest{Game: "party"})
	if err != nil {
		t.Fatalf("dropped member returning to the started room: %v", err)
	}
	if back.Token == joined.Token || back.Room.Members != 2 {
		t.Fatalf("return ticket %+v, want a fresh token and the same two seats", back.Room)
	}
	returned := dialWithToken(t, back.RelayAddr, back.Token)
	returned.expectControl(t, wire.RelayOpWelcome, nil)
	host.expectControl(t, wire.RelayOpMemberJoined, nil)
	waitRoom(id, func(r lobby.Room) bool { m, ok := member(r, "acc-alice"); return ok && m.Online && len(r.Members) == 2 })
}

// rawPublisher 把 lobby 的命令经 NATS 发出（wiring/lobby 用 IClient.Publish，这里只有 RawClient）。
type rawPublisher struct{ client fnats.RawClient }

func (p rawPublisher) Publish(subject string, data []byte) error {
	return p.client.PublishOnce(subject, data)
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
