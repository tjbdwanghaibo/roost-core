package relay_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/relay"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	relaysvc "github.com/tjbdwanghaibo/roost-core/service/relay"
)

// 换 host 的用例（方案 §4.6；RELAY.md §4A），连真实的 relay 与 lobby（peer_test.go）。

// room 建一个房间：房主 + 若干成员（槽位依次 2、3…），等房主收到每个成员上线。
func openRoom(t *testing.T, peer *testPeer, prefix string, hostOpts relay.Options, memberOpts []relay.Options) (*player, []*player, string) {
	t.Helper()
	host := newPlayer(t, peer, prefix+"-host", hostOpts)
	ticket, err := host.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "host"})
	if err != nil {
		t.Fatal(err)
	}
	host.connect(ticket)
	members := make([]*player, len(memberOpts))
	for i, opts := range memberOpts {
		members[i] = newPlayer(t, peer, fmt.Sprintf("%s-m%d", prefix, i+2), opts)
		if w := members[i].join(ticket.Room.ID, ""); w.Slot != uint32(i+2) {
			t.Fatalf("member %d welcome %+v", i, w)
		}
		recv(t, host.joined, "member joined")
	}
	return host, members, ticket.Room.ID
}

func mustRole(t *testing.T, p *player, want relay.Role) {
	t.Helper()
	if got := p.client.Role(); got != want {
		t.Fatalf("%s role %+v, want %+v", p.account, got, want)
	}
}

// 主动转交：新房主拿到交接数据（经 Compressor 压缩再解压，原样），旧房主收到 YouAreMember，迁移期间成员发给房主的
// 游戏帧被丢、收到一次 HostChanging；新房主回 HostReady 后全体收到 HostChanged，角色随之更新，游戏帧转给新房主。
// 新房主的快照 seq 从旧房主之上接（旧房主推过 seq 1，主动转交的 YouAreHost 不带 seq）。
func TestTransferHandsTheHandoffToTheNewHost(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	host, members, _ := openRoom(t, peer, "xfer", relay.Options{}, []relay.Options{{}, {}})
	a, b := members[0], members[1]
	mustRole(t, host, relay.Role{Slot: 1, Host: true, HostSlot: 1})
	if err := host.client.Snapshot([]byte("old-host-state")); err != nil {
		t.Fatal(err)
	}
	if err := a.client.Transfer(3, []byte("x")); !errors.Is(err, relay.ErrNotRoomHost) {
		t.Fatalf("member transfer: %v", err)
	}

	handoff := []byte("handoff: seed=42, deck=[7,1,9]")
	if err := host.client.Transfer(3, handoff); err != nil {
		t.Fatal(err)
	}
	if epoch := recv(t, host.youAreMember, "old host demoted"); epoch != 1 {
		t.Fatalf("you are member epoch %d", epoch)
	}
	mustRole(t, host, relay.Role{Slot: 1, Epoch: 1})
	offer := recv(t, b.youAreHost, "you are host")
	if offer.Epoch != 1 || offer.Source != relay.StateHandoff || string(offer.State) != string(handoff) || offer.StateErr != nil ||
		offer.SnapshotSeq != 1 || len(offer.Members) != 3 || // relay 带上旧房主存下的快照 seq，新房主从它往上接
		offer.Members[0] != (relay.Seat{Slot: 1, Account: "xfer-host", Online: true}) || offer.Members[2] != (relay.Seat{Slot: 3, Account: "xfer-m3", Online: true}) {
		t.Fatalf("offer %+v", offer)
	}
	mustRole(t, b, relay.Role{Slot: 3, Candidate: true, Epoch: 1})

	// 迁移期间：A 发给房主的游戏帧被丢，A 收到一次 HostChanging（发两帧也只一次）。
	for range 2 {
		if err := a.client.SendGame(relay.All, 1, []byte("lost")); err != nil {
			t.Fatal(err)
		}
	}
	if epoch := recv(t, a.hostChanging, "host changing"); epoch != 1 {
		t.Fatalf("host changing epoch %d", epoch)
	}

	if err := b.client.HostReady(offer.Epoch); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*player{host, a, b} {
		if e := recv(t, p.hostChanged, p.account+" host changed"); e != (relay.HostEvent{Epoch: 1, HostSlot: 3, Account: "xfer-m3"}) {
			t.Fatalf("%s host changed %+v", p.account, e)
		}
	}
	none(t, a.hostChanging, "a second host changing")
	mustRole(t, b, relay.Role{Slot: 3, Host: true, HostSlot: 3, Epoch: 1})
	mustRole(t, a, relay.Role{Slot: 2, HostSlot: 3, Epoch: 1})
	mustRole(t, host, relay.Role{Slot: 1, HostSlot: 3, Epoch: 1})
	none(t, b.game, "game frames dropped during the migration")

	// 转发恢复：成员的游戏帧到新房主；新房主能推快照（seq 不低于 relay 存着的旧房主那份，否则 Rejected{stale_seq}）。
	if err := a.client.SendGame(relay.All, 2, []byte("after")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, b.game, "game frame at the new host"), wire.RelayGame, 2, 2, "after")
	if err := b.client.Snapshot([]byte("new-host-state")); err != nil {
		t.Fatal(err)
	}
	waitPong(t, b) // 同一连接按序：Pong 回来时，那份快照要被拒的话 Rejected 已经先到了
	none(t, b.rejected, "rejected snapshot from the new host")
	// 旧房主不能再踢人、转交。
	if err := host.client.Kick(2); !errors.Is(err, relay.ErrNotRoomHost) {
		t.Fatalf("old host kick: %v", err)
	}
}

// 房主断线：宽限期内成员发给房主的游戏帧被丢、收到 HostChanging（宽限期内是当前 epoch 0）；期满后按候选顺序
// （SetSuccessors 声明了槽位 3，没声明时会是槽位 2）把 relay 存的快照交给候选，候选自动回 HostReady 接手。
func TestHostLossHandsTheSnapshotToTheDeclaredSuccessor(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{HostGrace: 300 * time.Millisecond})
	host, members, _ := openRoom(t, peer, "loss", relay.Options{}, []relay.Options{{}, {AutoHostReady: true}})
	a, b := members[0], members[1]
	state := []byte("only-the-host-knows: rng=123456789")
	if err := host.client.Snapshot(state); err != nil {
		t.Fatal(err)
	}
	if err := host.client.SetSuccessors([]uint32{3}); err != nil {
		t.Fatal(err)
	}
	if err := a.client.SetSuccessors([]uint32{2}); !errors.Is(err, relay.ErrNotRoomHost) {
		t.Fatalf("member successors: %v", err)
	}
	// 同一连接按序：快照与候选顺序在 Ping 之前到 relay；等 Pong 回来再断开。
	waitPong(t, host)
	sent := time.Now()
	_ = host.lastConn().Close() // 断网（relay 读到 EOF，没有 Closed）

	waitFor(t, "grace period started", func() bool {
		_ = a.client.SendGame(relay.All, 1, []byte("during grace"))
		select {
		case epoch := <-a.hostChanging:
			if epoch != 0 {
				t.Fatalf("host changing in grace epoch %d", epoch)
			}
			return true
		default:
			return false
		}
	})
	offer := recv(t, b.youAreHost, "you are host after the grace period")
	if offer.Epoch != 1 || offer.Source != relay.StateSnapshot || offer.SnapshotSeq != 1 || string(offer.State) != string(state) ||
		offer.SnapshotAge > time.Since(sent)+time.Second || len(offer.Members) != 3 || offer.Members[0].Online {
		t.Fatalf("offer %+v", offer)
	}
	none(t, a.youAreHost, "you are host at the undeclared member")
	for _, p := range []*player{a, b} {
		if e := recv(t, p.hostChanged, p.account+" host changed"); e != (relay.HostEvent{Epoch: 1, HostSlot: 3, Account: "loss-m3"}) {
			t.Fatalf("%s host changed %+v", p.account, e)
		}
	}
	mustRole(t, b, relay.Role{Slot: 3, Host: true, HostSlot: 3, Epoch: 1})
	mustRole(t, a, relay.Role{Slot: 2, HostSlot: 3, Epoch: 1})
}

// 房主离开（Leave：relay 上的 Leave 帧加 lobby 的 leave）不等宽限期；声明的候选顺序生效；没有快照时来源是 none。
func TestHostLeaveUsesTheSuccessorOrder(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{HostGrace: time.Minute})
	host, members, _ := openRoom(t, peer, "leave", relay.Options{}, []relay.Options{{}, {}, {}})
	if err := host.client.SetSuccessors([]uint32{4, 2}); err != nil {
		t.Fatal(err)
	}
	waitPong(t, host)
	if err := host.client.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	offer := recv(t, members[2].youAreHost, "you are host at slot 4")
	if offer.Epoch != 1 || offer.Source != relay.StateNone || offer.State != nil || len(offer.Members) != 4 {
		t.Fatalf("offer %+v", offer)
	}
	none(t, members[0].youAreHost, "you are host at slot 2")
	if d := recv(t, host.disconnected, "host left"); d.Reconnecting {
		t.Fatalf("host leave %+v", d)
	}
}

// 宽限期内房主（自动重连）用同一账号回来：仍是房主，成员收到 HostResumed，房主再收到在线成员的 MemberJoined。
func TestHostResumesWithinTheGracePeriod(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{HostGrace: 5 * time.Second})
	reconnect := relay.ReconnectPolicy{MaxAttempts: 5, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	host, members, _ := openRoom(t, peer, "resume", relay.Options{Reconnect: reconnect}, []relay.Options{{}})
	a := members[0]
	_ = host.lastConn().Close()
	if d := recv(t, host.disconnected, "host transport lost"); !d.Reconnecting {
		t.Fatalf("host disconnect %+v", d)
	}
	if w := recv(t, host.reconnected, "host reconnected"); !w.Host || w.Slot != 1 || w.HostSlot != 1 {
		t.Fatalf("host welcome %+v", w)
	}
	if e := recv(t, a.hostResumed, "host resumed"); e != (relay.HostEvent{Epoch: 0, HostSlot: 1}) {
		t.Fatalf("host resumed %+v", e)
	}
	if m := recv(t, host.joined, "online member told again"); m.Slot != 2 || m.Account != "resume-m2" {
		t.Fatalf("joined %+v", m)
	}
	mustRole(t, host, relay.Role{Slot: 1, Host: true, HostSlot: 1})
	mustRole(t, a, relay.Role{Slot: 2, HostSlot: 1})
	none(t, a.youAreHost, "you are host")
	if err := a.client.SendGame(relay.All, 5, []byte("still here")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "game frame after resume"), wire.RelayGame, 2, 5, "still here")
}

// 快照节奏：离上一份不到 SnapshotMinInterval 的 SDK 不发（ErrSnapshotTooSoon）；SDK 的最小间隔比 relay 的小时，
// relay 拒收并回 Rejected{too_soon, seq}；转交给不在线的槽位回 Rejected{bad_target}；压缩后太大的不发。
func TestSnapshotPacingAndRejections(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{SnapshotMinInterval: time.Second})
	host, members, _ := openRoom(t, peer, "pace", relay.Options{}, []relay.Options{{}})
	if err := host.client.Snapshot([]byte("s1")); err != nil {
		t.Fatal(err)
	}
	if err := host.client.Snapshot([]byte("s2")); !errors.Is(err, relay.ErrSnapshotTooSoon) {
		t.Fatalf("second snapshot within a second: %v", err)
	}
	if err := members[0].client.Snapshot([]byte("m")); !errors.Is(err, relay.ErrNotRoomHost) {
		t.Fatalf("member snapshot: %v", err)
	}
	if err := host.client.Transfer(9, []byte("h")); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, host.rejected, "bad target"); r != (relay.Rejected{Op: wire.RelayOpTransfer, Code: wire.RelayRejectBadTarget}) {
		t.Fatalf("rejected %+v", r)
	}
	big := make([]byte, 300<<10)
	_, _ = rand.Read(big) // 随机字节压不小
	if err := host.client.Snapshot(big); !errors.Is(err, relay.ErrStateTooBig) {
		t.Fatalf("oversized snapshot: %v", err)
	}

	eager := newPlayer(t, peer, "pace-eager", relay.Options{SnapshotMinInterval: time.Millisecond})
	ticket, err := eager.lobby.CreateRoom(context.Background(), relay.CreateRoomRequest{Game: game, Mode: "eager"})
	if err != nil {
		t.Fatal(err)
	}
	eager.connect(ticket)
	if err := eager.client.Snapshot([]byte("e1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := eager.client.Snapshot([]byte("e2")); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, eager.rejected, "too soon at the relay"); r != (relay.Rejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooSoon, Seq: 2}) {
		t.Fatalf("rejected %+v", r)
	}
}

// 定时快照：当房主时 SDK 按 SnapshotInterval 向游戏要状态、压缩后推；房主离开后候选拿到的是最近一份，seq 与推的次数一致。
func TestAutoSnapshotIsPushedWhileHost(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{SnapshotMinInterval: 50 * time.Millisecond})
	var pushed, candidatePushed atomic.Int32
	state := func() []byte { return []byte(fmt.Sprintf("auto-%d", pushed.Add(1))) }
	candidateState := func() []byte { candidatePushed.Add(1); return []byte("candidate") }
	host, members, _ := openRoom(t, peer, "auto", relay.Options{SnapshotState: state, SnapshotInterval: 100 * time.Millisecond,
		SnapshotMinInterval: 50 * time.Millisecond}, []relay.Options{{SnapshotState: candidateState, SnapshotInterval: 100 * time.Millisecond}})
	waitFor(t, "a few automatic snapshots", func() bool { return pushed.Load() >= 3 })
	waitPong(t, host)
	if err := host.client.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := pushed.Load()
	offer := recv(t, members[0].youAreHost, "you are host")
	if offer.Source != relay.StateSnapshot || offer.SnapshotSeq < 3 || string(offer.State) != fmt.Sprintf("auto-%d", offer.SnapshotSeq) || int32(offer.SnapshotSeq) > n {
		t.Fatalf("offer seq %d state %q (pushed %d)", offer.SnapshotSeq, offer.State, n)
	}
	// 候选还没回 HostReady：不是房主，不向游戏要状态、不定时推。
	time.Sleep(300 * time.Millisecond)
	if got := candidatePushed.Load(); got != 0 {
		t.Fatalf("candidate asked for %d snapshots before HostReady", got)
	}
	// 回 HostReady、收到 HostChanged 之后才按房主推。
	if err := members[0].client.HostReady(offer.Epoch); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new host pushes snapshots", func() bool { return candidatePushed.Load() >= 1 })
}

// 房主心跳：relay 的 host_idle_timeout 是缺省的 2s，房主不发游戏帧，只靠 SDK 的心跳（缺省 600ms）；5s 以上都不被当成
// 断线，成员收不到任何换 host 的通知，游戏帧照常到房主。成员仍是 10s 一次的心跳。
func TestHostHeartbeatKeepsTheHostAliveUnderTheHostIdleTimeout(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	if got := peer.relay.Config().HostIdleTimeout; got != 2*time.Second {
		t.Fatalf("relay host_idle_timeout %v, want the 2s default", got)
	}
	host, members, _ := openRoom(t, peer, "beat", relay.Options{}, []relay.Options{{}})
	a := members[0]
	time.Sleep(5500 * time.Millisecond)
	none(t, host.disconnected, "host disconnect")
	none(t, a.hostChanging, "host changing")
	none(t, a.youAreHost, "you are host")
	none(t, a.hostChanged, "host changed")
	if err := a.client.SendGame(relay.All, 1, []byte("ping the host")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "game frame at the host"), wire.RelayGame, 2, 1, "ping the host")
	none(t, a.hostChanging, "host changing")
	mustRole(t, host, relay.Role{Slot: 1, Host: true, HostSlot: 1})
}

// 自动重连遇到 relay 向 lobby 验权失败：lobby 不可用（Closed{lobby_unavailable}）按退避一直试，恢复后连回来；
// lobby 不让进（Closed{lobby_rejected}）立即停止（只试一次），最后一条 Disconnect 的原因是 lobby_rejected。
func TestReconnectRetriesWhenTheLobbyIsDownAndStopsWhenItRejects(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	reconnect := relay.ReconnectPolicy{MaxAttempts: 50, MinBackoff: 20 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
	_, members, _ := openRoom(t, peer, "admit", relay.Options{}, []relay.Options{{Reconnect: reconnect}})
	m := members[0]

	if err := peer.SetAdmission("down"); err != nil {
		t.Fatal(err)
	}
	joins := m.joins.Load()
	_ = m.lastConn().Close()
	if d := recv(t, m.disconnected, "transport lost"); !d.Reconnecting {
		t.Fatalf("disconnect %+v", d)
	}
	waitFor(t, "several attempts while the lobby is down", func() bool { return m.joins.Load()-joins >= 3 })
	none(t, m.disconnected, "giving up while the lobby is down")
	if err := peer.SetAdmission("ok"); err != nil {
		t.Fatal(err)
	}
	if w := recv(t, m.reconnected, "reconnected after the lobby came back"); w.Slot != 2 {
		t.Fatalf("welcome %+v", w)
	}

	if err := peer.SetAdmission("reject"); err != nil {
		t.Fatal(err)
	}
	joins = m.joins.Load()
	_ = m.lastConn().Close()
	if d := recv(t, m.disconnected, "transport lost again"); !d.Reconnecting {
		t.Fatalf("disconnect %+v", d)
	}
	d := recv(t, m.disconnected, "final disconnect")
	if d.Reconnecting || d.Reason != wire.RelayReasonLobbyRejected || !isClosed(d.Err, wire.RelayReasonLobbyRejected) {
		t.Fatalf("final disconnect %+v", d)
	}
	if got := m.joins.Load() - joins; got != 1 {
		t.Fatalf("%d attempts after lobby_rejected, want 1", got)
	}
}

// waitPong 发一个 Ping 并等它的 Pong：同一连接按序，之前发出的控制帧都已被 relay 处理。
func waitPong(t *testing.T, p *player) {
	t.Helper()
	got := make(chan struct{}, 1)
	p.client.OnControl(wire.RelayOpPong, func(f relay.Frame) {
		if len(f.Payload) != 1 || f.Payload[0] != 9 { // 心跳的 Pong（8 字节）不算
			return
		}
		select {
		case got <- struct{}{}:
		default:
		}
	})
	defer p.client.OnControl(wire.RelayOpPong, nil)
	if err := p.client.SendControl(wire.RelayOpPing, 0, []byte{9}); err != nil {
		t.Fatal(err)
	}
	recv(t, got, p.account+" pong")
}

// 开局后（lobby 的房间状态是 started）：
//   - 成员掉线后自动重连照常（lobby 的 join 对席位还在的人幂等，不受“已开始”限制），回到原槽位；
//   - 关掉自动重连的成员掉线后，自己 Rejoin 也回到原座；
//   - 房主可以踢有座位但不在线的槽位：房主收到这个槽位的第二条 MemberLeft（原因 kicked），之后它的 join 答 403 kicked；
//   - 新来的人 join 答 room_started。
func TestReconnectAfterTheRoomStartedAndKickingAnOfflineSeat(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	reconnect := relay.ReconnectPolicy{MaxAttempts: 5, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	host, members, roomID := openRoom(t, peer, "started", relay.Options{}, []relay.Options{{Reconnect: reconnect}, {}})
	a, b := members[0], members[1]
	if room, err := host.lobby.SetRoomState(context.Background(), roomID, relay.SetRoomStateRequest{Game: game, State: relay.StateStarted}); err != nil || room.State != relay.StateStarted {
		t.Fatalf("start: %+v %v", room, err)
	}

	_ = a.lastConn().Close()
	if d := recv(t, a.disconnected, "a transport lost"); !d.Reconnecting {
		t.Fatalf("a disconnect %+v", d)
	}
	if w := recv(t, a.reconnected, "a reconnected in a started room"); w.Slot != 2 || w.HostSlot != 1 {
		t.Fatalf("a welcome %+v", w)
	}

	_ = b.lastConn().Close()
	if d := recv(t, b.disconnected, "b transport lost"); d.Reconnecting {
		t.Fatalf("b disconnect %+v", d)
	}
	waitFor(t, "host sees b offline", func() bool {
		for {
			select {
			case m := <-host.left:
				if m.Slot == 3 {
					return m.Reason == wire.RelayReasonDisconnected
				}
			default:
				return false
			}
		}
	})
	if w, err := b.client.Rejoin(context.Background()); err != nil || w.Slot != 3 {
		t.Fatalf("b rejoin in a started room: %+v %v", w, err)
	}
	waitFor(t, "host sees b back", func() bool {
		select {
		case m := <-host.joined:
			return m.Slot == 3
		default:
			return false
		}
	})

	// b 再掉线（座位留着），房主踢这个离线槽位：房主收到第二条 MemberLeft{kicked}。
	_ = b.lastConn().Close()
	recv(t, b.disconnected, "b transport lost again")
	if m := recv(t, host.left, "b offline again"); m.Slot != 3 || m.Reason != wire.RelayReasonDisconnected {
		t.Fatalf("left %+v", m)
	}
	if err := host.client.Kick(3); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, host.left, "offline seat kicked"); m != (relay.Member{Slot: 3, Account: "started-m3", Reason: wire.RelayReasonKicked}) {
		t.Fatalf("kicked offline seat %+v", m)
	}
	waitFor(t, "lobby records the kick", func() bool {
		_, err := b.client.Rejoin(context.Background())
		return errors.Is(err, relay.ErrKicked)
	})

	newcomer := newPlayer(t, peer, "started-new", relay.Options{})
	if _, err := newcomer.lobby.JoinRoom(context.Background(), roomID, relay.JoinRoomRequest{Game: game}); !errors.Is(err, relay.ErrRoomStarted) {
		t.Fatalf("newcomer: %v", err)
	}
	// a 仍在：游戏通道照常。
	if err := a.client.SendGame(relay.All, 7, []byte("still in")); err != nil {
		t.Fatal(err)
	}
	mustFrame(t, recv(t, host.game, "a frame after the kick"), wire.RelayGame, 2, 7, "still in")
}
