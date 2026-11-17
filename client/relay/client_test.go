package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/relay"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	relaysvc "github.com/tjbdwanghaibo/roost-core/service/relay"
)

// 这些用例连真实的 relay 与 lobby（peer_test.go 在进程内起 service/relay 与 service/lobby）。房间目录用
// ROOST_REDIS_TEST_ADDR / ROOST_DATAENGINE_IT_REDIS_ADDR 的 Redis（source ~/.roost-it/roost-dataengine-it/env.sh），
// 没有时用内存目录，日志里写明。

const game = peerGame

func startPeer(t *testing.T, relayCfg relaysvc.Config) *testPeer {
	t.Helper()
	peer, err := startTestPeer(peerConfig{RedisAddr: redisAddrFromEnv(), Relay: relayCfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("peer: lobby %s, relay %s, room directory %s", peer.LobbyURL, peer.RelayTCP, peer.Backend)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := peer.Close(ctx); err != nil {
			t.Errorf("peer close: %v", err)
		}
	})
	return peer
}

// player 是一个玩家：lobby 客户端 + relay 客户端 + 收到的事件（每种一个通道）。
type player struct {
	t       *testing.T
	account string
	lobby   *relay.Lobby
	client  *relay.Client
	joins   *atomic.Int32 // 经 lobby 的 join 次数（重连要重新要令牌）

	game, room   chan relay.Frame
	joined, left chan relay.Member
	muted        chan bool
	disconnected chan relay.Disconnect
	reconnected  chan relay.Welcome
	custom       chan relay.Frame
	conns        chan net.Conn // 拨出的 TCP 连接，用来模拟断网
	ticket       relay.Ticket  // 最近一次 join 拿到的票据（测旧令牌）

	// 换 host 的通知。
	youAreHost   chan relay.YouAreHost
	youAreMember chan uint64
	hostChanging chan uint64
	hostChanged  chan relay.HostEvent
	hostResumed  chan relay.HostEvent
	rejected     chan relay.Rejected
	presence     chan relay.MemberPresence // 当房主时收到的成员前台上报
}

func newPlayer(t *testing.T, peer *testPeer, account string, opts relay.Options) *player {
	t.Helper()
	p := &player{t: t, account: account, joins: &atomic.Int32{},
		game: make(chan relay.Frame, 64), room: make(chan relay.Frame, 64), joined: make(chan relay.Member, 64),
		left: make(chan relay.Member, 64), muted: make(chan bool, 64), disconnected: make(chan relay.Disconnect, 64),
		reconnected: make(chan relay.Welcome, 64), custom: make(chan relay.Frame, 64), conns: make(chan net.Conn, 64),
		youAreHost: make(chan relay.YouAreHost, 64), youAreMember: make(chan uint64, 64), hostChanging: make(chan uint64, 64),
		hostChanged: make(chan relay.HostEvent, 64), hostResumed: make(chan relay.HostEvent, 64), rejected: make(chan relay.Rejected, 64),
		presence: make(chan relay.MemberPresence, 64)}
	httpClient := &http.Client{Transport: countJoins{p.joins, http.DefaultTransport}}
	p.lobby = relay.NewLobby(peer.LobbyURL, relay.StaticGameToken(peer.IssueGameToken(account, game)), httpClient)
	opts.Game = game
	opts.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			p.conns <- conn
		}
		return conn, err
	}
	p.client = relay.NewClient(p.lobby, opts)
	p.client.OnGame(func(f relay.Frame) { p.game <- f })
	p.client.OnRoom(func(f relay.Frame) { p.room <- f })
	p.client.OnMemberJoined(func(m relay.Member) { p.joined <- m })
	p.client.OnMemberLeft(func(m relay.Member) { p.left <- m })
	p.client.OnMuted(func(on bool) { p.muted <- on })
	p.client.OnDisconnected(func(d relay.Disconnect) { p.disconnected <- d })
	p.client.OnReconnected(func(w relay.Welcome) { p.reconnected <- w })
	p.client.OnYouAreHost(func(e relay.YouAreHost) { p.youAreHost <- e })
	p.client.OnYouAreMember(func(epoch uint64) { p.youAreMember <- epoch })
	p.client.OnHostChanging(func(epoch uint64) { p.hostChanging <- epoch })
	p.client.OnHostChanged(func(e relay.HostEvent) { p.hostChanged <- e })
	p.client.OnHostResumed(func(e relay.HostEvent) { p.hostResumed <- e })
	p.client.OnRejected(func(r relay.Rejected) { p.rejected <- r })
	p.client.OnMemberPresence(func(m relay.MemberPresence) { p.presence <- m })
	t.Cleanup(func() {
		_ = p.client.Close()
		select {
		case <-p.client.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("%s: client did not finish after Close", account)
		}
	})
	return p
}

type countJoins struct {
	joins *atomic.Int32
	next  http.RoundTripper
}

func (c countJoins) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/join") {
		c.joins.Add(1)
	}
	return c.next.RoundTrip(r)
}

func (p *player) connect(ticket relay.Ticket) relay.Welcome {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	welcome, err := p.client.Connect(ctx, ticket)
	if err != nil {
		p.t.Fatalf("%s connect: %v", p.account, err)
	}
	return welcome
}

func (p *player) join(roomID, password string) relay.Welcome {
	p.t.Helper()
	ticket, err := p.lobby.JoinRoom(context.Background(), roomID, relay.JoinRoomRequest{Game: game, Password: password, Transport: relay.TransportTCP})
	if err != nil {
		p.t.Fatalf("%s join: %v", p.account, err)
	}
	p.ticket = ticket
	return p.connect(ticket)
}

// lastConn 返回最近一次拨出的 TCP 连接（重连会拨新的）。
func (p *player) lastConn() net.Conn {
	p.t.Helper()
	conn := recv(p.t, p.conns, p.account+" tcp connection")
	for {
		select {
		case newer := <-p.conns:
			conn = newer
		default:
			return conn
		}
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func none[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected %s: %+v", what, v)
	default:
	}
}

func mustFrame(t *testing.T, f relay.Frame, channel wire.RelayChannel, slot, msgID uint32, payload string) {
	t.Helper()
	if f.Channel != channel || f.Slot != slot || f.MsgID != msgID || string(f.Payload) != payload {
		t.Fatalf("frame %+v (payload %q), want channel %d slot %d msg %d payload %q", f, f.Payload, channel, slot, msgID, payload)
	}
}

// 建房、列房、加入；游戏通道往返（成员只到房主，房主按目标或全体）；房间通道直达某个槽位与全体；禁言；踢人；
// 成员与房主的权限；房主改状态。
func TestRoomLifecycleAgainstRealRelayAndLobby(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	host := newPlayer(t, peer, "host-1", relay.Options{})
	ticket, err := host.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "duel", Name: "sdk", Password: "pw", MaxMembers: 4, Attrs: json.RawMessage(`{"map":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Room.Host != "host-1" || !ticket.Room.HasPassword || ticket.Token == "" || ticket.RelayAddr != peer.RelayTCP || string(ticket.Room.Attrs) != `{"map":"a"}` {
		t.Fatalf("create ticket %+v", ticket)
	}
	if w := host.connect(ticket); !w.Host || w.Slot != 1 || w.HostSlot != 1 || !host.client.IsHost() {
		t.Fatalf("host welcome %+v", w)
	}
	// 房主连上后 relay 报告房间，lobby 才把它列出来。
	waitFor(t, "room listed", func() bool {
		rooms, err := host.lobby.ListRooms(context.Background(), game, "duel", 0)
		return err == nil && len(rooms) == 1 && rooms[0].ID == ticket.Room.ID
	})

	m1 := newPlayer(t, peer, "member-1", relay.Options{Password: "pw"})
	m2 := newPlayer(t, peer, "member-2", relay.Options{Password: "pw"})
	if _, err := m1.lobby.JoinRoom(context.Background(), ticket.Room.ID, relay.JoinRoomRequest{Game: game, Password: "bad"}); !errors.Is(err, relay.ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if w := m1.join(ticket.Room.ID, "pw"); w.Host || w.Slot != 2 || w.HostSlot != 1 {
		t.Fatalf("m1 welcome %+v", w)
	}
	if w := m2.join(ticket.Room.ID, "pw"); w.Slot != 3 {
		t.Fatalf("m2 welcome %+v", w)
	}
	if m := recv(t, host.joined, "m1 joined"); m.Slot != 2 || m.Account != "member-1" || m.Reason != wire.RelayReasonNone {
		t.Fatalf("joined %+v", m)
	}
	if m := recv(t, host.joined, "m2 joined"); m.Slot != 3 || m.Account != "member-2" {
		t.Fatalf("joined %+v", m)
	}

	// 游戏通道：成员的帧只到房主（目标不看），房主看到成员槽位；房主按目标回给一个成员，游戏位原样带过。
	if err := m1.client.SendGame(3, 101, []byte("move")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "member game frame"), wire.RelayGame, 2, 101, "move")
	if err := host.client.Send(relay.Frame{Channel: wire.RelayGame, Slot: 2, MsgID: 202, Flags: wire.FlagPush | wire.FlagSync, Payload: []byte("state")}); err != nil {
		t.Fatal(err)
	}
	f := recv(t, m1.game, "host game frame")
	mustFrame(t, f, wire.RelayGame, 1, 202, "state")
	if f.Flags != wire.FlagPush|wire.FlagSync {
		t.Fatalf("game flags %08b", f.Flags)
	}
	if err := host.client.SendGame(relay.All, 203, []byte("tick")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, m1.game, "broadcast to m1"), wire.RelayGame, 1, 203, "tick")
	mustFrame(t, recv(t, m2.game, "broadcast to m2"), wire.RelayGame, 1, 203, "tick")
	none(t, m2.game, "m2 game frame meant for m1 or the host") // 202 只给了 m1；m1 的 101 只给了房主

	// 房间通道：直达某个槽位（不经房主），全体时房主也收到。
	if err := m1.client.SendRoom(3, 7, []byte("psst")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, m2.room, "direct room frame"), wire.RelayRoom, 2, 7, "psst")
	if err := m2.client.SendRoom(relay.All, 8, []byte("hi all")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.room, "room broadcast at host"), wire.RelayRoom, 3, 8, "hi all")
	mustFrame(t, recv(t, m1.room, "room broadcast at m1"), wire.RelayRoom, 3, 8, "hi all")
	none(t, host.room, "host room frame (the direct one went to slot 3 only)")

	// 只有房主能踢人、禁言、改状态（改状态在最后）。
	if err := m1.client.Kick(3); !errors.Is(err, relay.ErrNotRoomHost) {
		t.Fatalf("member kick: %v", err)
	}

	// 禁言：m2 收到通知，它的房间帧被丢弃，游戏帧照常（同一连接按序，游戏帧先到说明房间帧没被转出）。
	if err := host.client.Mute(3, true); err != nil {
		t.Fatal(err)
	}
	if on := recv(t, m2.muted, "muted"); !on {
		t.Fatal("muted notice says unmuted")
	}
	if err := m2.client.SendRoom(relay.All, 9, []byte("muted")); err != nil {
		t.Fatal(err)
	}
	if err := m2.client.SendGame(relay.All, 104, []byte("after-mute")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "game frame after mute"), wire.RelayGame, 3, 104, "after-mute")
	none(t, host.room, "room frame from a muted member")

	// 踢人：被踢的连接收到 Closed{kicked}，不自动重连；房主收到 MemberLeft{kicked}。
	if err := host.client.Kick(3); err != nil {
		t.Fatal(err)
	}
	if d := recv(t, m2.disconnected, "kicked"); d.Reason != wire.RelayReasonKicked || d.Reconnecting {
		t.Fatalf("kicked disconnect %+v", d)
	} else if closed := (*relay.ClosedError)(nil); !errors.As(d.Err, &closed) || closed.Reason != wire.RelayReasonKicked {
		t.Fatalf("kicked error %v", d.Err)
	}
	if m := recv(t, host.left, "kicked member left"); m.Slot != 3 || m.Account != "member-2" || m.Reason != wire.RelayReasonKicked {
		t.Fatalf("left %+v", m)
	}
	// 被踢记在 lobby（relay 报告 member_left{kicked}）：被踢的账号重新加入，lobby 的 join 答 403 kicked。
	waitFor(t, "lobby records the kick", func() bool {
		_, err := m2.lobby.JoinRoom(context.Background(), ticket.Room.ID, relay.JoinRoomRequest{Game: game, Password: "pw"})
		return errors.Is(err, relay.ErrKicked)
	})
	var lobbyErr *relay.LobbyError
	if _, err := m2.client.Rejoin(context.Background()); !errors.Is(err, relay.ErrKicked) || !errors.As(err, &lobbyErr) || lobbyErr.Status != 403 {
		t.Fatalf("kicked member rejoin: %v", err)
	}
	// 拿还没过期的旧令牌直接连 relay：relay 本地记着被踢，Closed{kicked}，不再问 lobby。
	if _, err := relay.NewClient(nil, relay.Options{Game: game}).Connect(context.Background(), m2.ticket); !isClosed(err, wire.RelayReasonKicked) {
		t.Fatalf("kicked member with its old token: %v", err)
	}

	// 离开：先在 relay 上发 Leave（Closed{left}），再经 lobby 让出席位；房主收到 MemberLeft{left}。
	if err := m1.client.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, host.left, "member left"); m.Slot != 2 || m.Reason != wire.RelayReasonLeft {
		t.Fatalf("left %+v", m)
	}
	// 席位让出后拿还没过期的旧令牌连 relay：令牌验签通过，lobby 验权不让进（不占席位），Closed{lobby_rejected}。
	if _, err := relay.NewClient(nil, relay.Options{Game: game}).Connect(context.Background(), m1.ticket); !isClosed(err, wire.RelayReasonLobbyRejected) {
		t.Fatalf("left member with its old token: %v", err)
	}

	if _, err := m2.lobby.SetRoomState(context.Background(), ticket.Room.ID, relay.SetRoomStateRequest{Game: game, State: relay.StateStarted}); !errors.Is(err, relay.ErrNotHost) {
		t.Fatalf("member state change: %v", err)
	}
	room, err := host.lobby.SetRoomState(context.Background(), ticket.Room.ID, relay.SetRoomStateRequest{Game: game, State: relay.StateStarted})
	if err != nil || room.State != relay.StateStarted {
		t.Fatalf("host state change: %+v %v", room, err)
	}
	if _, err := m1.lobby.JoinRoom(context.Background(), ticket.Room.ID, relay.JoinRoomRequest{Game: game, Password: "pw"}); !errors.Is(err, relay.ErrRoomStarted) {
		t.Fatalf("join a started room after leaving: %v", err)
	}
}

// 房间里最后一个人 Leave：relay 收到 Leave 帧随即关房并报 room.closed，lobby 的 leave 可能答 room_not_found。
// 房间已经不在，“我要离开”的目的已达到，Leave 不报错。第二个房间把这个先后定下来：最后一人先只在 relay 上发
// Leave 帧，等 lobby 确认房间已关，再调 Leave。
func TestLeaveByTheLastMemberSucceedsAfterTheRoomCloses(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	openRoom := func(suffix string) (*player, *player, string) {
		host := newPlayer(t, peer, "host-"+suffix, relay.Options{})
		ticket, err := host.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "duel-" + suffix, MaxMembers: 2})
		if err != nil {
			t.Fatal(err)
		}
		host.connect(ticket)
		member := newPlayer(t, peer, "member-"+suffix, relay.Options{})
		member.join(ticket.Room.ID, "")
		recv(t, host.joined, suffix+" member joined")
		if err := member.client.Leave(context.Background()); err != nil {
			t.Fatalf("%s: first leave: %v", suffix, err)
		}
		if m := recv(t, host.left, suffix+" member left"); m.Reason != wire.RelayReasonLeft {
			t.Fatalf("%s: left %+v", suffix, m)
		}
		return host, member, ticket.Room.ID
	}
	roomGone := func(host *player, roomID string) bool {
		_, err := host.lobby.SetRoomState(context.Background(), roomID, relay.SetRoomStateRequest{Game: game, State: relay.StateOpen})
		return errors.Is(err, relay.ErrRoomNotFound)
	}

	// 自然先后：最后一人直接 Leave。
	host, _, roomID := openRoom("a")
	if err := host.client.Leave(context.Background()); err != nil {
		t.Fatalf("last member leave: %v", err)
	}
	waitFor(t, "room a closed in the lobby", func() bool { return roomGone(host, roomID) })

	// 定下先后：relay 已关房、lobby 已记下关闭，再 Leave。
	host, _, roomID = openRoom("b")
	if err := host.client.SendControl(wire.RelayOpLeave, 0, nil); err != nil {
		t.Fatal(err)
	}
	if d := recv(t, host.disconnected, "host left on the relay"); d.Reconnecting {
		t.Fatalf("host disconnect %+v", d)
	}
	waitFor(t, "room b closed in the lobby", func() bool { return roomGone(host, roomID) })
	if err := host.client.Leave(context.Background()); err != nil {
		t.Fatalf("leave a room that already closed: %v", err)
	}
	// 只放过房间不在这一种：别的 lobby 错误照常返回（这里拿一个从没有过的房间号经 lobby 直接离开）。
	if _, err := host.lobby.LeaveRoom(context.Background(), "0000000000000000", game); !errors.Is(err, relay.ErrRoomNotFound) {
		t.Fatalf("raw lobby leave of an unknown room: %v", err)
	}
}

// 断线重连：连接自己断掉（模拟断网，relay 没发 Closed）时，Client 重新调 lobby 的 join 要令牌、再连 relay，
// 之后游戏通道照常往返。房主断开后宽限期满，唯一的成员被选为候选；它不回 HostReady，超时后收到 YouAreMember，
// 没有别的候选就关房：成员收到 Closed{room_closed}，即使开着自动重连也不重连。
func TestReconnectAsksTheLobbyForAFreshToken(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{HostGrace: 200 * time.Millisecond, HostReadyTimeout: 300 * time.Millisecond})
	host := newPlayer(t, peer, "host-r", relay.Options{})
	ticket, err := host.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "coop"})
	if err != nil {
		t.Fatal(err)
	}
	host.connect(ticket)
	member := newPlayer(t, peer, "member-r", relay.Options{Reconnect: relay.ReconnectPolicy{MaxAttempts: 5, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}})
	first := member.join(ticket.Room.ID, "")
	recv(t, host.joined, "member joined")
	joinsBefore := member.joins.Load()

	conn := recv(t, member.conns, "member tcp connection")
	_ = conn.Close() // 断网：本地连接断了，relay 那边也随之读到 EOF
	if d := recv(t, member.disconnected, "disconnect"); d.Reason != wire.RelayReasonDisconnected || !d.Reconnecting {
		t.Fatalf("disconnect %+v", d)
	}
	again := recv(t, member.reconnected, "reconnected")
	if again.Host || again.HostSlot != 1 || again.Slot != first.Slot {
		t.Fatalf("reconnected welcome %+v (first %+v)", again, first)
	}
	if got := member.joins.Load() - joinsBefore; got != 1 {
		t.Fatalf("reconnect made %d lobby joins, want 1 (a fresh relay token per attempt)", got)
	}
	// 房主看到这个成员走了又回来（或被顶号），之后游戏通道照常。
	waitFor(t, "host sees the member again", func() bool {
		select {
		case m := <-host.joined:
			return m.Account == "member-r"
		default:
			return false
		}
	})
	if err := member.client.SendGame(relay.All, 1, []byte("back")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "frame after reconnect"), wire.RelayGame, again.Slot, 1, "back")
	if err := host.client.SendGame(again.Slot, 2, []byte("welcome back")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, member.game, "host frame after reconnect"), wire.RelayGame, 1, 2, "welcome back")

	// 房主断开：宽限期满选中成员当候选；成员不回 HostReady，超时后 YouAreMember，然后关房，不重连。
	if err := host.client.Close(); err != nil {
		t.Fatal(err)
	}
	if offer := recv(t, member.youAreHost, "you are host"); offer.Epoch != 1 || offer.Source != relay.StateNone || !member.client.Role().Candidate {
		t.Fatalf("offer %+v role %+v", offer, member.client.Role())
	}
	if epoch := recv(t, member.youAreMember, "candidate timed out"); epoch != 1 || member.client.Role().Candidate {
		t.Fatalf("you are member %d role %+v", epoch, member.client.Role())
	}
	if d := recv(t, member.disconnected, "room closed"); d.Reason != wire.RelayReasonRoomClosed || d.Reconnecting {
		t.Fatalf("room closed disconnect %+v", d)
	}
	select {
	case <-member.client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("member client still running after the room closed")
	}
	if d := recv(t, host.disconnected, "host local close"); d.Reason != wire.RelayReasonNone || !errors.Is(d.Err, relay.ErrClosed) {
		t.Fatalf("host close %+v", d)
	}
}

// 心跳：relay 的空闲超时很短时，按 PingInterval 发心跳的连接一直活着并测到 RTT；不发心跳的连接被 relay 以
// idle_timeout 断开（并按策略重连回来）。
func TestHeartbeatKeepsTheConnectionAlive(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{IdleTimeout: 400 * time.Millisecond})
	host := newPlayer(t, peer, "host-h", relay.Options{PingInterval: 100 * time.Millisecond})
	ticket, err := host.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "hb"})
	if err != nil {
		t.Fatal(err)
	}
	// OnControl 是控制帧的扩展口：内置处理过的操作码（Pong 先算 RTT）也交给注册的回调。
	host.client.OnControl(wire.RelayOpPong, func(f relay.Frame) { host.custom <- f })
	host.connect(ticket)
	quiet := newPlayer(t, peer, "member-quiet", relay.Options{PingInterval: time.Hour, ReadTimeout: 2 * time.Hour,
		Reconnect: relay.ReconnectPolicy{MaxAttempts: 2, MinBackoff: 10 * time.Millisecond}})
	quiet.join(ticket.Room.ID, "")

	if d := recv(t, quiet.disconnected, "idle member"); d.Reason != wire.RelayReasonIdleTimeout || !d.Reconnecting {
		t.Fatalf("quiet member disconnect %+v", d)
	}
	recv(t, quiet.reconnected, "idle member reconnected")
	time.Sleep(time.Second) // 2.5 个空闲超时
	none(t, host.disconnected, "host disconnect while sending pings")
	if host.client.RTT() <= 0 {
		t.Fatalf("rtt %v after pings", host.client.RTT())
	}
	if pong := recv(t, host.custom, "pong via OnControl"); pong.Channel != wire.RelayControl || len(pong.Payload) != 8 {
		t.Fatalf("pong %+v", pong)
	}
}

// lobby 的错误映射成 *LobbyError，可以 errors.Is 判断。
func TestLobbyErrorsMapToSentinels(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	bad := relay.NewLobby(peer.LobbyURL, relay.StaticGameToken("gt2.not-issued"), nil)
	_, err := bad.ListRooms(context.Background(), game, "duel", 0)
	var lobbyErr *relay.LobbyError
	if !errors.Is(err, relay.ErrUnauthorized) || !errors.As(err, &lobbyErr) || lobbyErr.Status != 401 || lobbyErr.Code != "unauthorized" {
		t.Fatalf("bad token: %v", err)
	}
	p := newPlayer(t, peer, "errs", relay.Options{})
	if _, err := p.lobby.ListRooms(context.Background(), "other", "duel", 0); !errors.Is(err, relay.ErrGameNotServed) {
		t.Fatalf("other game: %v", err)
	}
	if _, err := p.lobby.JoinRoom(context.Background(), "0000000000000000", relay.JoinRoomRequest{Game: game}); !errors.Is(err, relay.ErrRoomNotFound) {
		t.Fatalf("missing room: %v", err)
	}
	if _, err := p.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "bad mode!"}); !errors.Is(err, relay.ErrBadRequest) {
		t.Fatalf("bad mode: %v", err)
	}
	ticket, err := p.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "full", MaxMembers: 1})
	if err != nil {
		t.Fatal(err)
	}
	other := newPlayer(t, peer, "errs-2", relay.Options{})
	if _, err := other.lobby.JoinRoom(context.Background(), ticket.Room.ID, relay.JoinRoomRequest{Game: game}); !errors.Is(err, relay.ErrRoomFull) || errors.Is(err, relay.ErrRoomStarted) {
		t.Fatalf("full room: %v", err)
	}
	if _, err := p.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "x", Transport: relay.TransportWS}); !errors.Is(err, relay.ErrNoRelay) {
		t.Fatalf("ws without a ws relay: %v", err)
	}
	// Go SDK 只连 TCP：拿 ws 地址来连，本地就拒绝。
	if _, err := p.client.Connect(context.Background(), relay.Ticket{RelayAddr: "ws://127.0.0.1:1/relay", Token: "x"}); err == nil || !strings.Contains(err.Error(), "not a TCP address") {
		t.Fatalf("ws address: %v", err)
	}
	// 令牌被拒：relay 发 Closed{token_rejected}，Connect 返回 *ClosedError。
	if _, err := p.client.Connect(context.Background(), relay.Ticket{RelayAddr: peer.RelayTCP, Token: "rt1.forged"}); !isClosed(err, wire.RelayReasonTokenRejected) {
		t.Fatalf("forged token: %v", err)
	}
}

func isClosed(err error, reason wire.RelayReason) bool {
	var closed *relay.ClosedError
	return errors.As(err, &closed) && closed.Reason == reason
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
