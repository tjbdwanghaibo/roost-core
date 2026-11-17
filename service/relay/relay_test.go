package relay_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

var testKey = []byte("relay-test-key-0123456789")

const testInstance = "relay-1"

type harness struct {
	t       *testing.T
	server  *relay.Server
	addr    string
	dialer  func() net.Conn
	reports *recorder
}

// newHarness 起一个 relay，默认监听 TCP 回环；pipe 为 true 时换成内存 net.Pipe 的 Listener（任意 net.Listener 都能接）。
func newHarness(t *testing.T, pipe bool, mutate func(*relay.Config)) *harness {
	t.Helper()
	config := relay.Config{Instance: testInstance, TokenKey: testKey}
	if mutate != nil {
		mutate(&config)
	}
	reports := &recorder{}
	server, err := relay.NewServer(config, reports)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, server: server, reports: reports}
	var listener net.Listener
	if pipe {
		pl := newPipeListener()
		listener, h.dialer = pl, pl.dial
	} else {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listener, h.addr = tcp, tcp.Addr().String()
		h.dialer = func() net.Conn {
			c, err := net.Dial("tcp", h.addr)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h
}

func (h *harness) token(room, account string, role token.Role, maxMembers int) string {
	h.t.Helper()
	signed, err := token.Sign(testKey, token.Claims{Relay: testInstance, Room: room, Account: account, Role: role, MaxMembers: maxMembers, ExpiresAt: time.Now().Add(30 * time.Second)})
	if err != nil {
		h.t.Fatal(err)
	}
	return signed
}

// connect 发令牌首帧，读回 Welcome。
func (h *harness) connect(room, account string, role token.Role, maxMembers int) (*client, wire.RelayWelcome) {
	h.t.Helper()
	c := h.raw()
	c.sendToken(h.token(room, account, role, maxMembers))
	p := c.read()
	if p.RelayChannel() != wire.RelayControl || p.MsgID != wire.RelayOpWelcome {
		h.t.Fatalf("%s: expected welcome, got channel %d op %d payload %v", account, p.RelayChannel(), p.MsgID, p.Payload)
	}
	welcome, err := wire.DecodeRelayWelcome(p.Payload)
	if err != nil || welcome.HostSlot != 1 || p.Seq == 0 {
		h.t.Fatalf("%s: welcome %+v slot %d: %v", account, welcome, p.Seq, err)
	}
	c.slot = p.Seq
	return c, welcome
}

// rejected 连接并期望在 Welcome 之前收到 Closed{reason}。
func (h *harness) rejected(tok string, reason wire.RelayReason) {
	h.t.Helper()
	c := h.raw()
	c.sendToken(tok)
	c.expectClosed(reason)
}

func (h *harness) raw() *client {
	conn := h.dialer()
	h.t.Cleanup(func() { _ = conn.Close() })
	return &client{t: h.t, conn: conn, reader: bufio.NewReader(conn)}
}

type client struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	slot   uint32
}

func (c *client) sendToken(tok string) {
	c.t.Helper()
	if err := wire.WriteRelay(c.conn, []*wire.Packet{{Payload: []byte(tok)}}, 0); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) send(flags byte, msgID, slot uint32, payload []byte) {
	c.t.Helper()
	if err := wire.WriteRelay(c.conn, []*wire.Packet{{Flags: flags, MsgID: msgID, Seq: slot, Payload: payload}}, 1<<20); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) read() *wire.Packet {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	p, err := wire.ReadRelay(c.reader, 1<<20)
	if err != nil {
		c.t.Fatalf("slot %d read: %v", c.slot, err)
	}
	return p
}

// expect 读一帧并核对通道、message_id、槽位与载荷。
func (c *client) expect(channel wire.RelayChannel, msgID, slot uint32, payload string) {
	c.t.Helper()
	p := c.read()
	if p.RelayChannel() != channel || p.MsgID != msgID || p.Seq != slot || string(p.Payload) != payload {
		c.t.Fatalf("slot %d got channel %d msg %d seq %d payload %q; want channel %d msg %d seq %d payload %q",
			c.slot, p.RelayChannel(), p.MsgID, p.Seq, p.Payload, channel, msgID, slot, payload)
	}
}

func (c *client) expectMember(op, slot uint32, reason wire.RelayReason, account string) {
	c.t.Helper()
	c.expect(wire.RelayControl, op, slot, string(wire.EncodeRelayMember(wire.RelayMember{Reason: reason, Account: account})))
}

// expectSilence 在 d 内收不到任何帧。
func (c *client) expectSilence(d time.Duration) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	p, err := wire.ReadRelay(c.reader, 1<<20)
	var timeout net.Error
	if err == nil {
		c.t.Fatalf("slot %d expected silence, got channel %d msg %d seq %d payload %q", c.slot, p.RelayChannel(), p.MsgID, p.Seq, p.Payload)
	}
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		c.t.Fatalf("slot %d expected silence, connection ended: %v", c.slot, err)
	}
}

// expectClosed 读到 Closed{reason}，之后连接断开。
func (c *client) expectClosed(reason wire.RelayReason) {
	c.t.Helper()
	c.expect(wire.RelayControl, wire.RelayOpClosed, 0, string([]byte{byte(reason)}))
	c.expectEOF()
}

func (c *client) expectEOF() {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if p, err := wire.ReadRelay(c.reader, 1<<20); err == nil {
		c.t.Fatalf("slot %d expected the connection to end, got msg %d", c.slot, p.MsgID)
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) && !isReset(err) {
		c.t.Fatalf("slot %d expected the connection to end, got %v", c.slot, err)
	}
}

func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && !op.Timeout()
}

type recorder struct {
	mu      sync.Mutex
	reports []relay.Report
}

func (r *recorder) Report(report relay.Report) {
	r.mu.Lock()
	r.reports = append(r.reports, report)
	r.mu.Unlock()
}

// wait 等到出现一条满足 match 的报告。
func (r *recorder) wait(t *testing.T, match func(relay.Report) bool) relay.Report {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, report := range r.reports {
			if match(report) {
				r.mu.Unlock()
				return report
			}
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("no matching report among %+v", r.reports)
	return relay.Report{}
}

func (r *recorder) count(match func(relay.Report) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, report := range r.reports {
		if match(report) {
			n++
		}
	}
	return n
}

// room3 建一个房主 + 两个成员的房间。
func (h *harness) room3(room string) (host, a, b *client) {
	host, welcome := h.connect(room, "host", token.RoleHost, 8)
	if !welcome.Host || host.slot != 1 {
		h.t.Fatalf("host welcome %+v slot %d", welcome, host.slot)
	}
	a, welcome = h.connect(room, "acc-a", token.RoleMember, 8)
	if welcome.Host || a.slot != 2 {
		h.t.Fatalf("member a welcome %+v slot %d", welcome, a.slot)
	}
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-a")
	b, welcome = h.connect(room, "acc-b", token.RoleMember, 8)
	if welcome.Host || b.slot != 3 {
		h.t.Fatalf("member b welcome %+v slot %d", welcome, b.slot)
	}
	host.expectMember(wire.RelayOpMemberJoined, 3, wire.RelayReasonNone, "acc-b")
	return host, a, b
}

const quiet = 150 * time.Millisecond

// 游戏通道：成员的帧只到房主（带成员槽位，成员写的目标不看）；房主的帧按目标到单个成员或全体成员，不回到房主。
func TestGameChannelDirection(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-game")

	a.send(wire.FlagRelayGame, 101, 3, []byte("from-a")) // 目标写成 b 也只到房主
	host.expect(wire.RelayGame, 101, 2, "from-a")
	b.expectSilence(quiet)
	a.expectSilence(quiet)

	host.send(wire.FlagRelayGame|wire.FlagPush, 202, 2, []byte("to-a"))
	a.expect(wire.RelayGame, 202, 1, "to-a")
	b.expectSilence(quiet)

	host.send(wire.FlagRelayGame, 303, wire.RelayAll, []byte("to-all"))
	a.expect(wire.RelayGame, 303, 1, "to-all")
	b.expect(wire.RelayGame, 303, 1, "to-all")
	host.expectSilence(quiet)

	host.send(wire.FlagRelayGame, 404, 9, []byte("nobody")) // 不在线的目标丢弃
	a.expectSilence(quiet)
	b.expectSilence(quiet)
}

// 房间通道：按目标直达，房主也是收件人，发送者自己收不到。
func TestRoomChannelDirect(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-chat")

	a.send(wire.FlagRelayRoom, 7, wire.RelayAll, []byte("hi all"))
	host.expect(wire.RelayRoom, 7, 2, "hi all")
	b.expect(wire.RelayRoom, 7, 2, "hi all")
	a.expectSilence(quiet)

	a.send(wire.FlagRelayRoom, 8, 3, []byte("psst b"))
	b.expect(wire.RelayRoom, 8, 2, "psst b")
	host.expectSilence(quiet)

	b.send(wire.FlagRelayRoom, 9, 1, []byte("to host"))
	host.expect(wire.RelayRoom, 9, 3, "to host")
	a.expectSilence(quiet)

	host.send(wire.FlagRelayRoom, 10, wire.RelayAll, []byte("host says"))
	a.expect(wire.RelayRoom, 10, 1, "host says")
	b.expect(wire.RelayRoom, 10, 1, "host says")
}

// 房间通道单独限额：超出的房间帧丢弃、连接保留，游戏通道不受影响。
func TestRoomChannelHasItsOwnLimit(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) {
		c.RoomFramesPerSecond = 3
		c.MaxPayloadBytes = 1024
		c.RoomBytesPerSecond = 1024
	})
	host, a, _ := h.room3("room-limit")

	for i := 0; i < 10; i++ {
		a.send(wire.FlagRelayRoom, 1, 1, []byte{byte(i)})
	}
	a.send(wire.FlagRelayGame, 2, 0, []byte("game"))
	for i := 0; i < 3; i++ {
		host.expect(wire.RelayRoom, 1, 2, string([]byte{byte(i)}))
	}
	host.expect(wire.RelayGame, 2, 2, "game") // 第 4～10 帧房间帧被丢，游戏帧照常
	a.send(wire.FlagRelayControl, wire.RelayOpPing, 0, []byte("p"))
	a.expect(wire.RelayControl, wire.RelayOpPong, 0, "p")

	// 字节额度：每秒 1024 字节，600 字节的帧一秒内只过一帧。
	time.Sleep(1100 * time.Millisecond)
	big := make([]byte, 600)
	a.send(wire.FlagRelayRoom, 3, 1, big)
	a.send(wire.FlagRelayRoom, 4, 1, big)
	a.send(wire.FlagRelayGame, 5, 0, nil)
	host.expect(wire.RelayRoom, 3, 2, string(big))
	host.expect(wire.RelayGame, 5, 2, "")
}

// 禁言：被禁言的槽位发不出房间通道的帧，游戏通道照常；重连后仍禁言；解除后恢复。
func TestMuteBlocksOnlyTheRoomChannel(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-mute")

	host.send(wire.FlagRelayControl, wire.RelayOpMute, 3, []byte{1})
	b.expect(wire.RelayControl, wire.RelayOpMuted, 3, "\x01")
	b.send(wire.FlagRelayRoom, 1, wire.RelayAll, []byte("spam"))
	b.send(wire.FlagRelayGame, 2, 0, []byte("move"))
	host.expect(wire.RelayGame, 2, 3, "move") // 房间帧没到
	a.expectSilence(quiet)

	// 成员发禁言控制帧不生效。
	a.send(wire.FlagRelayControl, wire.RelayOpMute, 1, []byte{1})
	host.send(wire.FlagRelayRoom, 3, 2, []byte("still talking"))
	a.expect(wire.RelayRoom, 3, 1, "still talking")

	// 重连顶号后仍禁言。
	b2, _ := h.connect("room-mute", "acc-b", token.RoleMember, 8)
	b2.expect(wire.RelayControl, wire.RelayOpMuted, 3, "\x01")
	b.expectClosed(wire.RelayReasonReplaced)
	host.expectMember(wire.RelayOpMemberJoined, 3, wire.RelayReasonReplaced, "acc-b")
	b2.send(wire.FlagRelayRoom, 4, wire.RelayAll, []byte("spam again"))
	a.expectSilence(quiet)

	host.send(wire.FlagRelayControl, wire.RelayOpMute, 3, []byte{0})
	b2.expect(wire.RelayControl, wire.RelayOpMuted, 3, "\x00")
	b2.send(wire.FlagRelayRoom, 5, wire.RelayAll, []byte("back"))
	a.expect(wire.RelayRoom, 5, 3, "back")
	host.expect(wire.RelayRoom, 5, 3, "back")
}

// 槽位、踢人、同账号顶号。
func TestSlotsKickAndReplace(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-slots")

	// 成员走了，槽位空出来，新成员取最小的空槽位。
	_ = a.conn.Close()
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonDisconnected, "acc-a")
	c, _ := h.connect("room-slots", "acc-c", token.RoleMember, 8)
	if c.slot != 2 {
		t.Fatalf("new member slot %d, want the freed 2", c.slot)
	}
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-c")

	// 踢人：被踢的收到 Closed{kicked} 后断开，房主收到 MemberLeft{kicked}；同一账号再来被拒。
	host.send(wire.FlagRelayControl, wire.RelayOpKick, 3, nil)
	b.expectClosed(wire.RelayReasonKicked)
	host.expectMember(wire.RelayOpMemberLeft, 3, wire.RelayReasonKicked, "acc-b")
	h.rejected(h.token("room-slots", "acc-b", token.RoleMember, 8), wire.RelayReasonKicked)
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "acc-b" && r.Reason == "kicked"
	})

	// 成员发踢人控制帧不生效。
	c.send(wire.FlagRelayControl, wire.RelayOpKick, 1, nil)
	host.send(wire.FlagRelayGame, 1, 2, []byte("alive"))
	c.expect(wire.RelayGame, 1, 1, "alive")

	// 同账号顶号：旧连接收到 Closed{replaced}，新连接沿用槽位，房主收到 MemberJoined{replaced}。
	c2, _ := h.connect("room-slots", "acc-c", token.RoleMember, 8)
	if c2.slot != 2 {
		t.Fatalf("replacing connection slot %d, want 2", c2.slot)
	}
	c.expectClosed(wire.RelayReasonReplaced)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonReplaced, "acc-c")
	c2.send(wire.FlagRelayGame, 2, 0, []byte("from new"))
	host.expect(wire.RelayGame, 2, 2, "from new")
	host.send(wire.FlagRelayGame, 3, 2, []byte("to new"))
	c2.expect(wire.RelayGame, 3, 1, "to new")
	if n := h.reports.count(func(r relay.Report) bool { return r.Type == relay.EventMemberLeft && r.Account == "acc-c" }); n != 0 {
		t.Fatalf("replacement reported %d left events", n)
	}

	// 房主顶号：房间不关，新连接仍是房主。
	host2, welcome := h.connect("room-slots", "host", token.RoleHost, 8)
	if !welcome.Host || host2.slot != 1 {
		t.Fatalf("host replacement welcome %+v slot %d", welcome, host2.slot)
	}
	host.expectClosed(wire.RelayReasonReplaced)
	c2.send(wire.FlagRelayGame, 4, 0, []byte("to new host"))
	host2.expect(wire.RelayGame, 4, 2, "to new host")
}

// 角色以房间状态为准（§4.6.5）：房间建好以后不看令牌里的角色字段。
func TestRoleFollowsTheRoomNotTheToken(t *testing.T) {
	h := newHarness(t, false, nil)
	host, _ := h.connect("room-role", "host", token.RoleHost, 8)

	impostor, welcome := h.connect("room-role", "acc-x", token.RoleHost, 8)
	if welcome.Host || impostor.slot != 2 {
		t.Fatalf("a host token for a non-host account became %+v slot %d", welcome, impostor.slot)
	}
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-x")
	impostor.send(wire.FlagRelayGame, 1, 0, []byte("x"))
	host.expect(wire.RelayGame, 1, 2, "x")
	impostor.send(wire.FlagRelayControl, wire.RelayOpKick, 1, nil) // 不是房主，踢人无效
	impostor.send(wire.FlagRelayGame, 2, 0, []byte("still here"))
	host.expect(wire.RelayGame, 2, 2, "still here")

	again, welcome := h.connect("room-role", "host", token.RoleMember, 8)
	if !welcome.Host || again.slot != 1 {
		t.Fatalf("the host account with a member token became %+v slot %d", welcome, again.slot)
	}
	host.expectClosed(wire.RelayReasonReplaced)
}

// 房间生命周期：房主到了才建房；成员先到被拒；满员；房主断开关房、断开全部成员；关过的房间号不能用旧令牌重建；
// 令牌不对被拒。报告依次是 opened、joined、left、closed。
func TestRoomLifecycleAndReports(t *testing.T) {
	h := newHarness(t, false, nil)
	h.rejected(h.token("room-life", "acc-a", token.RoleMember, 2), wire.RelayReasonRoomNotFound)

	hostToken := h.token("room-life", "host", token.RoleHost, 2)
	host := h.raw()
	host.sendToken(hostToken)
	host.expect(wire.RelayControl, wire.RelayOpWelcome, 1, string(wire.EncodeRelayWelcome(wire.RelayWelcome{Host: true, HostSlot: 1})))
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventHeartbeat && r.Seq == 1 && r.Room == "room-life" && r.Relay == testInstance && r.Host == "host" && r.Epoch == 0 &&
			slices.Equal(r.Online, []string{"host"})
	})
	a, _ := h.connect("room-life", "acc-a", token.RoleMember, 2)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-a")
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberJoined && r.Seq == 2 && r.Slot == 2 && r.Account == "acc-a" &&
			slices.Equal(r.Online, []string{"host", "acc-a"})
	})
	h.rejected(h.token("room-life", "acc-b", token.RoleMember, 2), wire.RelayReasonRoomFull)

	_ = host.conn.Close()
	a.expectClosed(wire.RelayReasonRoomClosed)
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventClosed && r.Seq == 3 && r.Room == "room-life" && r.Reason == "room_closed" && len(r.Online) == 0
	})
	if stats := h.server.Stats(); stats.Rooms != 0 {
		t.Fatalf("stats %+v after close", stats)
	}
	h.rejected(hostToken, wire.RelayReasonRoomNotFound) // 还没过期的 host 令牌不能重建关过的房间

	other, err := token.Sign(testKey, token.Claims{Relay: "relay-2", Room: "room-x", Account: "host", Role: token.RoleHost, MaxMembers: 2, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	h.rejected(other, wire.RelayReasonTokenRejected)
	h.rejected("rt1.garbage", wire.RelayReasonTokenRejected)

	bad := h.raw()
	bad.send(wire.FlagRelayGame, 1, 0, []byte("not a token"))
	bad.expectClosed(wire.RelayReasonProtocol)
}

// 限额：超大帧、超速断开；空闲超时断开，心跳保活。
func TestConnectionLimits(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) {
		c.MaxPayloadBytes = 1024
		c.RoomBytesPerSecond = 1024
		c.FramesPerSecond = 20
		c.IdleTimeout = 300 * time.Millisecond
	})
	host, _ := h.connect("room-limits", "host", token.RoleHost, 8)

	big, _ := h.connect("room-limits", "acc-big", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-big")
	big.send(wire.FlagRelayGame, 1, 0, make([]byte, 1025))
	big.expectClosed(wire.RelayReasonFrameTooBig)
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonFrameTooBig, "acc-big")

	fast, _ := h.connect("room-limits", "acc-fast", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-fast")
	for i := 0; i < 25; i++ {
		fast.send(wire.FlagRelayGame, 1, 0, nil)
	}
	for i := 0; i < 20; i++ { // 前 20 帧（一秒的额度）照常转给房主
		host.expect(wire.RelayGame, 1, 2, "")
	}
	fast.expectClosed(wire.RelayReasonRateLimited)
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonRateLimited, "acc-fast")

	// 心跳保活：每 100ms 一次 Ping，过了两倍空闲超时仍在线。房主同样靠心跳保活。
	alive, _ := h.connect("room-limits", "acc-alive", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-alive")
	idle, _ := h.connect("room-limits", "acc-idle", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 3, wire.RelayReasonNone, "acc-idle")
	for i := 0; i < 6; i++ {
		alive.send(wire.FlagRelayControl, wire.RelayOpPing, 0, []byte{byte(i)})
		alive.expect(wire.RelayControl, wire.RelayOpPong, 0, string([]byte{byte(i)}))
		host.send(wire.FlagRelayControl, wire.RelayOpPing, 0, nil)
		time.Sleep(100 * time.Millisecond)
	}
	idle.expectClosed(wire.RelayReasonIdleTimeout)
	for {
		p := host.read()
		if p.MsgID == wire.RelayOpPong {
			continue
		}
		if p.MsgID != wire.RelayOpMemberLeft || p.Seq != 3 {
			t.Fatalf("host got op %d seq %d", p.MsgID, p.Seq)
		}
		if member, _ := wire.DecodeRelayMember(p.Payload); member.Reason != wire.RelayReasonIdleTimeout {
			t.Fatalf("idle member left with %v", member.Reason)
		}
		break
	}
	host.send(wire.FlagRelayGame, 9, 2, []byte("ok"))
	alive.expect(wire.RelayGame, 9, 1, "ok")

	// 超长的 Ping 是协议错误。
	alive.send(wire.FlagRelayControl, wire.RelayOpPing, 0, make([]byte, wire.RelayMaxPingBytes+1))
	alive.expectClosed(wire.RelayReasonProtocol)
}

// 发送队列满：不读的慢连接被断开，房主与别的成员照常收发、不被阻塞。走内存 net.Pipe：写对端不读就立刻堵住。
func TestSlowConsumerIsDisconnectedWithoutBlockingOthers(t *testing.T) {
	h := newHarness(t, true, func(c *relay.Config) {
		c.SendQueueFrames = 32
		c.WriteTimeout = time.Minute // 断开只能来自队列上限，不是写超时
	})
	host, a, slow := h.room3("room-slow")
	_ = slow // 从这里起不再读

	// 房主分批广播，每批 16 帧，读的成员收完一批再发下一批：它的队列不会超过 16，不读的成员在第 33 帧时超限。
	start := time.Now()
	for batch := 0; batch < 12; batch++ {
		for i := 0; i < 16; i++ {
			host.send(wire.FlagRelayGame, 1, wire.RelayAll, []byte{byte(batch), byte(i)})
		}
		for i := 0; i < 16; i++ {
			a.expect(wire.RelayGame, 1, 1, string([]byte{byte(batch), byte(i)}))
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("broadcast took %v; a slow member blocked the room", elapsed)
	}
	host.expectMember(wire.RelayOpMemberLeft, 3, wire.RelayReasonSlowConsumer, "acc-b")
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "acc-b" && r.Reason == "slow_consumer"
	})
}

// lobby 的 leave 命令：成员被移出（Closed{left}，房主收到 MemberLeft{left}）；房主被移出时关房。
func TestLobbyLeaveCommand(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-leave")
	h.server.Leave("room-leave", "acc-a")
	a.expectClosed(wire.RelayReasonLeft)
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonLeft, "acc-a")
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "acc-a" && r.Reason == "left" && slices.Equal(r.Online, []string{"host", "acc-b"})
	})
	h.server.Leave("room-leave", "nobody") // 迟到或无关的命令不影响房间
	h.server.Leave("no-such-room", "acc-b")
	host.send(wire.FlagRelayGame, 1, 3, []byte("b still here"))
	b.expect(wire.RelayGame, 1, 1, "b still here")

	h.server.Leave("room-leave", "host")
	host.expectClosed(wire.RelayReasonLeft)
	b.expectClosed(wire.RelayReasonRoomClosed)
	h.reports.wait(t, func(r relay.Report) bool { return r.Type == relay.EventClosed && r.Room == "room-leave" })
}

// 房主断开后关房：全部成员收到 Closed{room_closed} 后断开。
func TestHostDisconnectClosesTheRoom(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-close")
	_ = host.conn.Close()
	a.expectClosed(wire.RelayReasonRoomClosed)
	b.expectClosed(wire.RelayReasonRoomClosed)
	report := h.reports.wait(t, func(r relay.Report) bool { return r.Type == relay.EventClosed && r.Room == "room-close" })
	if len(report.Online) != 0 || report.Reason != "room_closed" {
		t.Fatalf("closed report %+v", report)
	}
}

// 心跳与空房超时：每个房间按间隔发心跳；只剩房主太久关房。
func TestHeartbeatAndEmptyRoomTimeout(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) {
		c.HeartbeatInterval = 20 * time.Millisecond
		c.EmptyRoomTimeout = 300 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.server.Run(ctx) }()

	host, _ := h.connect("room-hb", "host", token.RoleHost, 4)
	a, _ := h.connect("room-hb", "acc-a", token.RoleMember, 4)
	host.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-a")
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventHeartbeat && r.Room == "room-hb" && len(r.Online) == 2
	})
	time.Sleep(400 * time.Millisecond) // 有成员时不算空房
	host.send(wire.FlagRelayGame, 1, 2, []byte("still open"))
	a.expect(wire.RelayGame, 1, 1, "still open")

	_ = a.conn.Close()
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonDisconnected, "acc-a")
	host.send(wire.FlagRelayControl, wire.RelayOpPing, 0, nil)
	host.expect(wire.RelayControl, wire.RelayOpPong, 0, "")
	host.expectClosed(wire.RelayReasonIdleTimeout)
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventClosed && r.Room == "room-hb" && r.Reason == "idle_timeout"
	})
}

// 停机：每条连接收到 Closed{shutdown}。
func TestShutdownClosesEveryConnection(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, _ := h.room3("room-stop")
	pending := h.raw() // 还没发令牌的连接；等它被接受，否则它还在 accept 队列里，停机只会直接关掉
	deadline := time.Now().Add(3 * time.Second)
	for h.server.Stats().Connections < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = h.server.Shutdown(ctx) }()
	host.expectClosed(wire.RelayReasonShutdown)
	a.expectClosed(wire.RelayReasonShutdown)
	pending.expectClosed(wire.RelayReasonShutdown)
}

func TestConfigValidation(t *testing.T) {
	for name, config := range map[string]relay.Config{
		"no instance":          {TokenKey: testKey},
		"short key":            {Instance: "r", TokenKey: []byte("short")},
		"room bytes below max": {Instance: "r", TokenKey: testKey, MaxPayloadBytes: 2048, RoomBytesPerSecond: 1024},
		"queue below 2 frames": {Instance: "r", TokenKey: testKey, MaxPayloadBytes: 1024, SendQueueBytes: 1024},
	} {
		if _, err := relay.NewServer(config, nil); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// pipeListener 是内存里的 net.Listener：dial 返回 net.Pipe 的客户端一端。
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) dial() net.Conn {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
	case <-l.done:
	}
	return client
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
