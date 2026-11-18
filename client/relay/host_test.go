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
	if _, err := host.client.Leave(context.Background()); err != nil {
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
	if _, err := host.client.Leave(context.Background()); err != nil {
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

// 旧任期的快照不覆盖新任期（v1.54.0 审查 RV-20261118-02，三份 SDK 一致）：A 在 epoch 0 开始准备一份快照（Snapshot 的
// 压缩，或定时快照的 SnapshotState），没准备完时房间转给 B、B 又转回 A（epoch 2），A 推出 epoch-two-state。旧的那份
// 这时才准备好：它属于 epoch 0，返回 ErrSnapshotStale，不发、不占 seq；A 离开后 B 恢复的是 epoch-two-state。
func TestASnapshotPreparedInAnEarlierHostTermIsDropped(t *testing.T) {
	for _, path := range []string{"Snapshot", "SnapshotState"} {
		t.Run(path, func(t *testing.T) {
			peer := startPeer(t, relaysvc.Config{SnapshotMinInterval: 10 * time.Millisecond})
			entered, release := make(chan struct{}), make(chan struct{})
			hold := func() {
				close(entered)
				<-release
			}
			// Snapshot：卡在压缩里；SnapshotState：卡在向游戏要状态里（定时快照在心跳 goroutine 上）。
			compressHold := func(state []byte) {
				if path == "Snapshot" && string(state) == "epoch-zero-state" {
					hold()
				}
			}
			hostOpts := relay.Options{AutoHostReady: true, SnapshotMinInterval: time.Millisecond, Compressor: gatedCompressor{compressHold}}
			var stale chan error
			if path == "SnapshotState" {
				// 定时快照只要第一份（epoch 0 的）；之后返回 nil 不推，epoch 2 的那份由测试手动推。
				var calls atomic.Int32
				hostOpts.SnapshotInterval = time.Hour
				hostOpts.SnapshotState = func() []byte {
					if calls.Add(1) > 1 {
						return nil
					}
					hold()
					return []byte("epoch-zero-state")
				}
			}
			a, members, _ := openRoom(t, peer, "term-"+path, hostOpts, []relay.Options{{AutoHostReady: true}})
			b := members[0]
			if path == "Snapshot" {
				stale = make(chan error, 1)
				go func() { stale <- a.client.Snapshot([]byte("epoch-zero-state")) }()
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the epoch-0 snapshot never started")
			}

			if err := a.client.Transfer(2, []byte("first transfer")); err != nil {
				t.Fatal(err)
			}
			recv(t, b.youAreHost, "B chosen")
			recv(t, b.hostChanged, "B host")
			if err := b.client.Transfer(1, []byte("back to A")); err != nil {
				t.Fatal(err)
			}
			recv(t, a.youAreHost, "A chosen again")
			recv(t, a.hostChanged, "A saw B as host")
			if e := recv(t, a.hostChanged, "A host again"); e.Epoch != 2 || e.HostSlot != 1 {
				t.Fatalf("A host again: %+v", e)
			}
			if err := a.client.Snapshot([]byte("epoch-two-state")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(30 * time.Millisecond) // 过了两边的最小间隔：旧的那份若被放行，不会因间隔被拦
			close(release)
			if stale != nil {
				if err := <-stale; !errors.Is(err, relay.ErrSnapshotStale) {
					t.Errorf("the epoch-0 snapshot returned %v, want ErrSnapshotStale", err)
				}
			}
			waitPong(t, a)
			waitPong(t, a) // 定时快照在心跳 goroutine 上：再一轮 Pong 回来时它已处理完
			none(t, a.rejected, "a rejected snapshot")

			if _, err := a.client.Leave(context.Background()); err != nil {
				t.Fatal(err)
			}
			offer := recv(t, b.youAreHost, "B recovers after A leaves")
			if offer.Source != relay.StateSnapshot || offer.SnapshotSeq != 2<<32+1 || string(offer.State) != "epoch-two-state" {
				t.Fatalf("B recovered seq %d state %q (source %d), want seq %d epoch-two-state", offer.SnapshotSeq, offer.State, offer.Source, uint64(2<<32+1))
			}
		})
	}
}

// 连接也绑定：压缩时房主断线、宽限期内自动重连回来（同一任期仍是房主），旧连接上准备的那份不在新连接上发出
// （ErrSnapshotStale）、不占 seq：新连接上的下一份是 seq 1，relay 不回 Rejected。
func TestASnapshotPreparedOnAnEarlierConnectionIsDropped(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{HostGrace: 5 * time.Second, SnapshotMinInterval: 10 * time.Millisecond})
	entered, release := make(chan struct{}), make(chan struct{})
	hold := func(state []byte) {
		if string(state) == "old-connection-state" {
			close(entered)
			<-release
		}
	}
	reconnect := relay.ReconnectPolicy{MaxAttempts: 5, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	host, members, _ := openRoom(t, peer, "term-conn", relay.Options{Reconnect: reconnect, SnapshotMinInterval: time.Millisecond,
		Compressor: gatedCompressor{hold}}, []relay.Options{{}})
	stale := make(chan error, 1)
	go func() { stale <- host.client.Snapshot([]byte("old-connection-state")) }()
	recv(t, entered, "the snapshot is being compressed")
	_ = host.lastConn().Close()
	recv(t, host.reconnected, "host reconnected")
	recv(t, members[0].hostResumed, "host resumed")
	mustRole(t, host, relay.Role{Slot: 1, Host: true, HostSlot: 1})
	close(release)
	if err := <-stale; !errors.Is(err, relay.ErrSnapshotStale) {
		t.Fatalf("the snapshot from the old connection returned %v, want ErrSnapshotStale", err)
	}
	if err := host.client.Snapshot([]byte("new-connection-state")); err != nil {
		t.Fatal(err)
	}
	waitPong(t, host)
	none(t, host.rejected, "a rejected snapshot (the dropped one must not use up seq 1)")
	if _, err := host.client.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	offer := recv(t, members[0].youAreHost, "member recovers after the host leaves")
	if offer.SnapshotSeq != 1 || string(offer.State) != "new-connection-state" {
		t.Fatalf("recovered seq %d state %q, want seq 1 new-connection-state", offer.SnapshotSeq, offer.State)
	}
}

// gatedCompressor 不压缩，压缩前先调 hold（测试用它把某一份卡在压缩里）。
type gatedCompressor struct{ hold func([]byte) }

func (gatedCompressor) ID() byte { return relay.NoCompression.ID() }

func (g gatedCompressor) Compress(data []byte) ([]byte, error) {
	g.hold(data)
	return relay.NoCompression.Compress(data)
}

func (gatedCompressor) Decompress(data []byte, limit int) ([]byte, error) {
	return relay.NoCompression.Decompress(data, limit)
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
// Welcome 带 epoch：换过 host 之后才进房的人、离线期间换了 host 又回来的人（Rejoin），Role().Epoch 都是房间此刻的
// epoch，不必等下一次换 host。
func TestLateJoinerAndRejoinSeeTheRoomEpoch(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	host, members, roomID := openRoom(t, peer, "epoch", relay.Options{}, []relay.Options{{}, {}})
	a, b := members[0], members[1]
	_ = a.lastConn().Close() // a 掉线（不自动重连），座位还在
	recv(t, a.disconnected, "a disconnected")
	recv(t, host.left, "a left")

	if err := host.client.Transfer(3, nil); err != nil {
		t.Fatal(err)
	}
	offer := recv(t, b.youAreHost, "you are host")
	if err := b.client.HostReady(offer.Epoch); err != nil {
		t.Fatal(err)
	}
	recv(t, host.hostChanged, "old host sees host changed")

	late := newPlayer(t, peer, "epoch-late", relay.Options{})
	if w := late.join(roomID, ""); w != (relay.Welcome{Slot: 4, HostSlot: 3, Epoch: 1}) {
		t.Fatalf("late welcome %+v", w)
	}
	mustRole(t, late, relay.Role{Slot: 4, HostSlot: 3, Epoch: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.client.Rejoin(ctx); err != nil {
		t.Fatal(err)
	}
	mustRole(t, a, relay.Role{Slot: 2, HostSlot: 3, Epoch: 1})
}

// 前台上报：成员 SetForeground 经 relay 只到房主（OnMemberPresence 的槽位是成员的）；换 host 后每个设过状态的人
// （含降为成员的旧房主）收到 HostChanged 自动重发一次，新房主拿到全部；成员自动重连成功后也重发。
func TestPresenceIsResentAfterHostChangedAndReconnect(t *testing.T) {
	peer := startPeer(t, relaysvc.Config{})
	reconnect := relay.ReconnectPolicy{MaxAttempts: 5, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}
	host, members, _ := openRoom(t, peer, "presence", relay.Options{}, []relay.Options{{}, {Reconnect: reconnect}})
	a, b := members[0], members[1]
	for _, step := range []struct {
		p          *player
		foreground bool
	}{{host, true}, {a, false}, {b, true}} {
		if err := step.p.client.SetForeground(step.foreground); err != nil {
			t.Fatal(err)
		}
	}
	got := map[uint32]bool{}
	for range 2 {
		m := recv(t, host.presence, "member presence at the host")
		got[m.Slot] = m.Foreground
	}
	if len(got) != 2 || got[2] || !got[3] {
		t.Fatalf("presence at the first host %v", got)
	}
	none(t, a.presence, "presence at a member")

	if err := host.client.Transfer(2, nil); err != nil {
		t.Fatal(err)
	}
	offer := recv(t, a.youAreHost, "you are host")
	if err := a.client.HostReady(offer.Epoch); err != nil {
		t.Fatal(err)
	}
	got = map[uint32]bool{}
	for range 2 {
		m := recv(t, a.presence, "presence re-sent to the new host")
		got[m.Slot] = m.Foreground
	}
	if len(got) != 2 || !got[1] || !got[3] {
		t.Fatalf("presence at the new host %v", got)
	}

	_ = b.lastConn().Close()
	recv(t, b.reconnected, "b reconnected")
	if m := recv(t, a.presence, "presence after b reconnected"); m != (relay.MemberPresence{Slot: 3, Foreground: true}) {
		t.Fatalf("presence after reconnect %+v", m)
	}
}

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

// ---- client/relay/README.md“接入自己的 zstd / LZ4”的示例代码：原样放在这里编一遍（SDK 不引第三方库，zstdCodec 是接口桩）。----

// zstdCodec 代表游戏自带的 zstd 库（例如 klauspost/compress/zstd 的 Encoder 与 Decoder）；SDK 不带它。
type zstdCodec interface {
	EncodeAll(src, dst []byte) []byte
	DecodeAll(src, dst []byte) ([]byte, error)
}

// zstdCompressor 把它接成 relay.Compressor。ID 取 2：0、1 归内置的 NoCompression 与 Deflate，LZ4 照样接、取 3。
type zstdCompressor struct{ codec zstdCodec }

func (zstdCompressor) ID() byte { return 2 }

func (z zstdCompressor) Compress(data []byte) ([]byte, error) {
	return z.codec.EncodeAll(data, nil), nil
}

// Decompress 要守住解压上限（防解压炸弹）：库自己能限就在库里限（klauspost 的 zstd.WithDecoderMaxMemory），这里再核一次。
func (z zstdCompressor) Decompress(data []byte, limit int) ([]byte, error) {
	out, err := z.codec.DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	if len(out) > limit {
		return nil, fmt.Errorf("zstd: state exceeds %d bytes", limit)
	}
	return out, nil
}

// README 示例的最后一步（只编译，不连对端）：房间里每个客户端都配上同一个 Compressor（内置的 Deflate、NoCompression
// 谁都解得开，游戏自带的要都配上，否则新房主拿到 relay.ErrUnknownCompression）。
func newZstdClient(lobby *relay.Lobby, codec zstdCodec) *relay.Client {
	return relay.NewClient(lobby, relay.Options{Game: "party", Compressor: zstdCompressor{codec}})
}
