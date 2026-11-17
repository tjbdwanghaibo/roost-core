package relay

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// WebSocket 接入（§4.7.1）：TCP 房主与 WebSocket 成员在同一个房间里双向转发；relay 写给浏览器的每条二进制消息恰好是一帧；
// Origin 不在白名单的升级被拒；etcd 广播带上 ws_addr。
func TestWebSocketMembersShareRoomsWithTCP(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	cfg := testConfig()
	cfg.Set("relay.ws_listen", "127.0.0.1:0")
	cfg.Set("relay.ws_advertise_addr", "wss://relay.test/relay")
	cfg.Set("relay.ws_allowed_origins", []string{"https://play.example.com"})
	server, etcd, ctx := startRelay(t, cfg)
	etcd.waitValue(t, "/t/relays/relay-3", Advertisement{Instance: "relay-3", TCPAddr: "relay.test:7100", WSAddr: "wss://relay.test/relay"})
	url := "ws://" + server.WSAddr().String() + "/relay"

	// Origin 不在白名单：403，连接不进 relay。
	if _, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example.com"}}}); err == nil ||
		resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %v %v", resp, err)
	}

	host := dialRelay(t, server.Addr().String(), "room-ws", "host", token.RoleHost)
	socket, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://play.example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	signed, err := token.Sign([]byte(testKey), token.Claims{Relay: "relay-3", Room: "room-ws", Account: "browser", Role: token.RoleMember, MaxMembers: 4, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	sendMessage := func(p *wire.Packet) {
		t.Helper()
		data, err := wire.EncodeRelay([]*wire.Packet{p}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(ctx, websocket.MessageBinary, data); err != nil {
			t.Fatal(err)
		}
	}
	// readMessage 读一条消息，要求它恰好是一帧。
	readMessage := func() *wire.Packet {
		t.Helper()
		readCtx, done := context.WithTimeout(ctx, 3*time.Second)
		defer done()
		kind, data, err := socket.Read(readCtx)
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("websocket read: %v %v", kind, err)
		}
		packets, err := wire.DecodeRelay(data, 0)
		if err != nil || len(packets) != 1 {
			t.Fatalf("a message held %d frames: %v", len(packets), err)
		}
		return packets[0]
	}
	sendMessage(&wire.Packet{Payload: []byte(signed)})
	if welcome := readMessage(); welcome.MsgID != wire.RelayOpWelcome || welcome.Seq != 2 {
		t.Fatalf("websocket welcome op %d slot %d", welcome.MsgID, welcome.Seq)
	}
	host.expectControl(t, wire.RelayOpMemberJoined, wire.EncodeRelayMember(wire.RelayMember{Account: "browser"}))

	sendMessage(&wire.Packet{Flags: wire.FlagRelayGame, MsgID: 11, Payload: []byte("from browser")})
	host.expectData(t, wire.RelayGame, 11, 2, "from browser")
	if err := wire.WriteRelay(host.conn, []*wire.Packet{{Flags: wire.FlagRelayGame, MsgID: 12, Seq: 2, Payload: []byte("to browser")},
		{Flags: wire.FlagRelayRoom, MsgID: 13, Seq: wire.RelayAll, Payload: []byte("chat")}}, 0); err != nil {
		t.Fatal(err)
	}
	if p := readMessage(); p.RelayChannel() != wire.RelayGame || p.MsgID != 12 || p.Seq != 1 || string(p.Payload) != "to browser" {
		t.Fatalf("browser got %+v", p)
	}
	if p := readMessage(); p.RelayChannel() != wire.RelayRoom || p.MsgID != 13 || p.Seq != 1 || string(p.Payload) != "chat" {
		t.Fatalf("browser got %+v", p)
	}
}

func TestWebSocketConfigIsChecked(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	for name, set := range map[string][2]string{
		"no advertise url":  {"relay.ws_advertise_addr", ""},
		"http advertise":    {"relay.ws_advertise_addr", "http://relay.test/relay"},
		"path without root": {"relay.ws_path", "relay"},
	} {
		cfg := testConfig()
		cfg.Set("relay.ws_listen", ":0")
		cfg.Set("relay.ws_advertise_addr", "ws://relay.test/relay")
		cfg.Set(set[0], set[1])
		if err := NewMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "relay.ws_") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// startRelay 走正式的 Mod → Server 链路起一个 relay（假 NATS、假 etcd），用例结束时停机。
func startRelay(t *testing.T, cfg *viper.Viper) (*Server, *fakeEtcd, context.Context) {
	t.Helper()
	registry := app.NewRegistry(cfg)
	etcd := newFakeEtcd()
	if err := registry.Register(mods.ModNatsRaw, fnats.RawClient(newFakeNats())); err != nil {
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
	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(stopCtx)
	})
	return server, etcd, ctx
}

// WebSocket 连接的限额与 TCP 相同：空闲超时收到 Closed{idle_timeout}，超大帧收到 Closed{frame_too_big}。
func TestWebSocketLimitsMatchTCP(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	cfg := testConfig()
	cfg.Set("relay.ws_listen", "127.0.0.1:0")
	cfg.Set("relay.ws_advertise_addr", "ws://relay.test/relay")
	cfg.Set("relay.idle_timeout", "300ms")
	cfg.Set("relay.max_payload_bytes", 1024)
	cfg.Set("relay.room_bytes_per_second", 1024)
	server, _, ctx := startRelay(t, cfg)
	url := "ws://" + server.WSAddr().String() + "/relay"
	host := dialRelay(t, server.Addr().String(), "room-wsl", "host", token.RoleHost)
	stopPinging := make(chan struct{})
	defer close(stopPinging)
	go func() { // 房主靠心跳保活
		for {
			select {
			case <-stopPinging:
				return
			case <-time.After(100 * time.Millisecond):
				_ = wire.WriteRelay(host.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPing}}, 0)
			}
		}
	}()
	join := func(account string) *websocket.Conn {
		socket, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { socket.CloseNow() })
		signed, err := token.Sign([]byte(testKey), token.Claims{Relay: "relay-3", Room: "room-wsl", Account: account, Role: token.RoleMember, MaxMembers: 4, ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := wire.EncodeRelay([]*wire.Packet{{Payload: []byte(signed)}}, 0)
		if err := socket.Write(ctx, websocket.MessageBinary, data); err != nil {
			t.Fatal(err)
		}
		return socket
	}
	// expectClosed 读到 Closed{reason}（跳过 Welcome）。
	expectClosed := func(socket *websocket.Conn, reason wire.RelayReason) {
		t.Helper()
		readCtx, done := context.WithTimeout(ctx, 3*time.Second)
		defer done()
		for {
			_, data, err := socket.Read(readCtx)
			if err != nil {
				t.Fatalf("waiting for Closed{%v}: %v", reason, err)
			}
			packets, err := wire.DecodeRelay(data, 0)
			if err != nil || len(packets) != 1 {
				t.Fatalf("message held %d frames: %v", len(packets), err)
			}
			if packets[0].MsgID == wire.RelayOpClosed {
				if wire.RelayReason(packets[0].Payload[0]) != reason {
					t.Fatalf("closed with %v, want %v", wire.RelayReason(packets[0].Payload[0]), reason)
				}
				return
			}
		}
	}
	idle := join("acc-idle")
	expectClosed(idle, wire.RelayReasonIdleTimeout)

	big := join("acc-big")
	data, _ := wire.EncodeRelay([]*wire.Packet{{Flags: wire.FlagRelayGame, MsgID: 1, Payload: make([]byte, 1025)}}, 0)
	if err := big.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
	expectClosed(big, wire.RelayReasonFrameTooBig)
}

// wsClient 是浏览器一侧：每条二进制消息一帧。
type wsClient struct {
	t      *testing.T
	ctx    context.Context
	socket *websocket.Conn
}

func dialWS(t *testing.T, ctx context.Context, url, room, account string, role token.Role) *wsClient {
	t.Helper()
	socket, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://play.example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	socket.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = socket.CloseNow() })
	c := &wsClient{t: t, ctx: ctx, socket: socket}
	c.send(&wire.Packet{Payload: []byte(signTestToken(t, room, account, role))})
	c.expectControl(wire.RelayOpWelcome)
	return c
}

func (c *wsClient) send(p *wire.Packet) {
	c.t.Helper()
	data, err := wire.EncodeRelay([]*wire.Packet{p}, len(p.Payload)+1)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.socket.Write(c.ctx, websocket.MessageBinary, data); err != nil {
		c.t.Fatal(err)
	}
}

// expectControl 跳过别的帧，直到读到操作码 op 的控制帧，返回载荷。每条消息必须恰好一帧。
func (c *wsClient) expectControl(op uint32) []byte {
	c.t.Helper()
	readCtx, done := context.WithTimeout(c.ctx, 3*time.Second)
	defer done()
	for {
		_, data, err := c.socket.Read(readCtx)
		if err != nil {
			c.t.Fatalf("websocket waiting for op %d: %v", op, err)
		}
		packets, err := wire.DecodeRelay(data, 1<<20)
		if err != nil || len(packets) != 1 {
			c.t.Fatalf("a message held %d frames: %v", len(packets), err)
		}
		if p := packets[0]; p.RelayChannel() == wire.RelayControl && p.MsgID == op {
			return p.Payload
		}
	}
}

// 换 host 在 WebSocket 上与 TCP 相同（§4.7.4 刷新页面 = 普通换 host）：浏览器房主推一份比普通帧上限大的快照
// （单条消息上限按 MaxFrameBytes），刷新页面（断开）后在宽限期内回来仍是房主、TCP 成员收到 host_resumed；
// 再断开不回来，宽限期满 TCP 成员拿到那份快照当新房主。之后 TCP 新房主主动转交回浏览器，交接数据经 WebSocket 送达。
func TestWebSocketHostChange(t *testing.T) {
	t.Setenv(testKeyEnv, testKey)
	cfg := testConfig()
	cfg.Set("relay.ws_listen", "127.0.0.1:0")
	cfg.Set("relay.ws_advertise_addr", "wss://relay.test/relay")
	cfg.Set("relay.ws_allowed_origins", []string{"https://play.example.com"})
	cfg.Set("relay.host_grace", "400ms")
	server, _, ctx := startRelay(t, cfg)
	url := "ws://" + server.WSAddr().String() + "/relay"

	browser := dialWS(t, ctx, url, "room-ws-host", "browser", token.RoleHost)
	member := dialRelay(t, server.Addr().String(), "room-ws-host", "acc-tcp", token.RoleMember)
	browser.expectControl(wire.RelayOpMemberJoined)
	snapshot := make([]byte, 100<<10) // 大于 max_payload_bytes（64 KiB）
	snapshot[0], snapshot[len(snapshot)-1] = 0xAB, 0xCD
	browser.send(&wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpSnapshot, Payload: wire.EncodeRelaySnapshot(wire.RelaySnapshot{Seq: 1, Data: snapshot})})
	browser.send(&wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpPing, Payload: []byte("p")})
	browser.expectControl(wire.RelayOpPong) // 快照已收下，连接没被断开

	_ = browser.socket.CloseNow() // 刷新页面
	browser = dialWS(t, ctx, url, "room-ws-host", "browser", token.RoleHost)
	member.expectControl(t, wire.RelayOpHostResumed, wire.EncodeRelayEpoch(0))

	_ = browser.socket.CloseNow() // 这次没回来
	_ = member.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var offer wire.RelayYouAreHost
	for {
		p, err := wire.ReadRelay(member.reader, 1<<20)
		if err != nil {
			t.Fatalf("waiting for you_are_host: %v", err)
		}
		if p.RelayChannel() == wire.RelayControl && p.MsgID == wire.RelayOpYouAreHost {
			if offer, err = wire.DecodeRelayYouAreHost(p.Payload); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if offer.Epoch != 1 || offer.Source != wire.RelayStateSnapshot || offer.SnapshotSeq != 1 || !bytes.Equal(offer.State, snapshot) {
		t.Fatalf("offer epoch %d source %d seq %d state %d bytes", offer.Epoch, offer.Source, offer.SnapshotSeq, len(offer.State))
	}
	if err := wire.WriteRelay(member.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostReady, Payload: wire.EncodeRelayEpoch(1)}}, 0); err != nil {
		t.Fatal(err)
	}
	member.expectControl(t, wire.RelayOpHostChanged, wire.EncodeRelayHostChanged(wire.RelayHostChanged{Epoch: 1, Account: "acc-tcp"}))

	// 浏览器再回来只是成员；TCP 新房主把房主转交给它，交接数据经 WebSocket 送达。
	browser = dialWS(t, ctx, url, "room-ws-host", "browser", token.RoleHost)
	member.expectControl(t, wire.RelayOpMemberJoined, nil)
	if err := wire.WriteRelay(member.conn, []*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpTransfer, Seq: 1, Payload: []byte("handoff")}}, 0); err != nil {
		t.Fatal(err)
	}
	member.expectControl(t, wire.RelayOpYouAreMember, wire.EncodeRelayEpoch(2))
	back, err := wire.DecodeRelayYouAreHost(browser.expectControl(wire.RelayOpYouAreHost))
	if err != nil || back.Epoch != 2 || back.Source != wire.RelayStateHandoff || string(back.State) != "handoff" {
		t.Fatalf("browser offer %+v %v", back, err)
	}
	browser.send(&wire.Packet{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpHostReady, Payload: wire.EncodeRelayEpoch(2)})
	member.expectControl(t, wire.RelayOpHostChanged, wire.EncodeRelayHostChanged(wire.RelayHostChanged{Epoch: 2, Account: "browser"}))
	browser.send(&wire.Packet{Flags: wire.FlagRelayGame, MsgID: 21, Seq: wire.RelayAll, Payload: []byte("browser hosts again")})
	member.expectData(t, wire.RelayGame, 21, 1, "browser hosts again")
}
