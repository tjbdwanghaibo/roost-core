package relay_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// 换 host（方案 §4.6）的用例。

// expectControl 读一帧控制帧，核对操作码与槽位，返回载荷。
func (c *client) expectControl(op, slot uint32) []byte {
	c.t.Helper()
	p := c.read()
	if p.RelayChannel() != wire.RelayControl || p.MsgID != op || p.Seq != slot {
		c.t.Fatalf("slot %d got channel %d op %d seq %d payload %q; want control op %d seq %d", c.slot, p.RelayChannel(), p.MsgID, p.Seq, p.Payload, op, slot)
	}
	return p.Payload
}

// expectEpoch 读一帧只带 epoch 的控制帧（you_are_member、host_changing、host_resumed）。
func (c *client) expectEpoch(op, slot uint32, epoch uint64) {
	c.t.Helper()
	got, err := wire.DecodeRelayEpoch(c.expectControl(op, slot))
	if err != nil || got != epoch {
		c.t.Fatalf("slot %d op %d epoch %d (%v), want %d", c.slot, op, got, err, epoch)
	}
}

func (c *client) expectYouAreHost(epoch uint64) wire.RelayYouAreHost {
	c.t.Helper()
	offer, err := wire.DecodeRelayYouAreHost(c.expectControl(wire.RelayOpYouAreHost, c.slot))
	if err != nil || offer.Epoch != epoch {
		c.t.Fatalf("slot %d you_are_host %+v (%v), want epoch %d", c.slot, offer, err, epoch)
	}
	return offer
}

func (c *client) expectHostChanged(epoch uint64, slot uint32, account string) {
	c.t.Helper()
	changed, err := wire.DecodeRelayHostChanged(c.expectControl(wire.RelayOpHostChanged, slot))
	if err != nil || changed != (wire.RelayHostChanged{Epoch: epoch, Account: account}) {
		c.t.Fatalf("slot %d host_changed %+v (%v), want epoch %d account %s", c.slot, changed, err, epoch, account)
	}
}

func (c *client) expectRejected(want wire.RelayRejected) {
	c.t.Helper()
	got, err := wire.DecodeRelayRejected(c.expectControl(wire.RelayOpRejected, 0))
	if err != nil || got != want {
		c.t.Fatalf("slot %d rejected %+v (%v), want %+v", c.slot, got, err, want)
	}
}

func (c *client) ready(epoch uint64) {
	c.send(wire.FlagRelayControl, wire.RelayOpHostReady, 0, wire.EncodeRelayEpoch(epoch))
}

func (c *client) snapshot(seq uint64, data []byte) {
	c.send(wire.FlagRelayControl, wire.RelayOpSnapshot, 0, wire.EncodeRelaySnapshot(wire.RelaySnapshot{Seq: seq, Data: data}))
}

func (c *client) ping(tag string) {
	c.t.Helper()
	c.send(wire.FlagRelayControl, wire.RelayOpPing, 0, []byte(tag))
	c.expect(wire.RelayControl, wire.RelayOpPong, 0, tag)
}

func seatsOf(offer wire.RelayYouAreHost) string {
	parts := make([]string, 0, len(offer.Members))
	for _, seat := range offer.Members {
		state := "off"
		if seat.Online {
			state = "on"
		}
		parts = append(parts, seat.Account+"@"+string(rune('0'+seat.Slot))+":"+state)
	}
	return strings.Join(parts, " ")
}

// 主动转交：不等；旧房主降为成员（you_are_member），交接数据原样到目标；迁移期间成员发给房主的帧丢弃并收到一次
// host_changing，旧房主的“房主帧”不再转出；新房主回 host_ready 后全体收到 host_changed、转发恢复，lobby 收到
// host_changed。之后旧房主拿没过期的 host 令牌重连只能当成员（§4.6.5）。
func TestTransferHandsTheRoomToTheTarget(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-transfer")

	host.send(wire.FlagRelayControl, wire.RelayOpTransfer, 3, []byte("handoff-state"))
	host.expectEpoch(wire.RelayOpYouAreMember, 1, 1)
	offer := b.expectYouAreHost(1)
	if offer.Source != wire.RelayStateHandoff || string(offer.State) != "handoff-state" || offer.SnapshotSeq != 0 ||
		seatsOf(offer) != "host@1:on acc-a@2:on acc-b@3:on" {
		t.Fatalf("offer %+v seats %s", offer, seatsOf(offer))
	}

	// 迁移期间：成员的游戏帧丢弃，发送者只收到一次 host_changing；旧房主的下行帧也不再转出。房间通道照常。
	a.send(wire.FlagRelayGame, 1, 0, []byte("lost-1"))
	a.send(wire.FlagRelayGame, 1, 0, []byte("lost-2"))
	a.expectEpoch(wire.RelayOpHostChanging, 0, 1)
	host.send(wire.FlagRelayGame, 2, wire.RelayAll, []byte("old host broadcast"))
	host.expectEpoch(wire.RelayOpHostChanging, 0, 1)
	host.send(wire.FlagRelayControl, wire.RelayOpKick, 2, nil) // 已经不是房主：无效
	host.snapshot(1, []byte("late"))
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectNotHost, Seq: 1})
	a.send(wire.FlagRelayRoom, 3, wire.RelayAll, []byte("chat"))
	host.expect(wire.RelayRoom, 3, 2, "chat")
	b.expect(wire.RelayRoom, 3, 2, "chat")
	a.expectSilence(quiet)
	b.expectSilence(quiet) // 候选没收到任何游戏帧

	b.send(wire.FlagRelayControl, wire.RelayOpHostReady, 0, wire.EncodeRelayEpoch(0)) // epoch 不对：忽略
	a.expectSilence(quiet)
	b.ready(1)
	for _, c := range []*client{host, a, b} {
		c.expectHostChanged(1, 3, "acc-b")
	}
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventHostChanged && r.Host == "acc-b" && r.Epoch == 1 && r.Slot == 3 &&
			slices.Equal(r.Online, []string{"host", "acc-a", "acc-b"})
	})

	// 转发恢复：成员（含旧房主）的帧到新房主，新房主的帧按目标转出。
	a.send(wire.FlagRelayGame, 4, 0, []byte("to new host"))
	b.expect(wire.RelayGame, 4, 2, "to new host")
	host.send(wire.FlagRelayGame, 5, 2, []byte("old host up"))
	b.expect(wire.RelayGame, 5, 1, "old host up")
	a.expectSilence(quiet)
	b.send(wire.FlagRelayGame, 6, wire.RelayAll, []byte("new host says"))
	a.expect(wire.RelayGame, 6, 3, "new host says")
	host.expect(wire.RelayGame, 6, 3, "new host says")

	// 旧房主拿没过期的 host 令牌重连：只是成员，顶掉自己的旧连接，新房主收到 MemberJoined{replaced}。
	again, welcome := h.connect("room-transfer", "host", token.RoleHost, 8)
	if welcome.Host || welcome.HostSlot != 3 || again.slot != 1 {
		t.Fatalf("old host with its host token: welcome %+v slot %d", welcome, again.slot)
	}
	host.expectClosed(wire.RelayReasonReplaced)
	b.expectMember(wire.RelayOpMemberJoined, 1, wire.RelayReasonReplaced, "host")
	again.send(wire.FlagRelayGame, 7, 2, []byte("tries to command a"))
	b.expect(wire.RelayGame, 7, 1, "tries to command a")
	a.expectSilence(quiet)
	again.send(wire.FlagRelayControl, wire.RelayOpKick, 2, nil)
	b.send(wire.FlagRelayGame, 8, 2, []byte("a still here"))
	a.expect(wire.RelayGame, 8, 3, "a still here")
}

// 转交的目标不在线、是自己、发送者不是房主：拒收，房间不变。
func TestTransferRejections(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, _ := h.room3("room-transfer-bad")
	host.send(wire.FlagRelayControl, wire.RelayOpTransfer, 9, nil)
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpTransfer, Code: wire.RelayRejectBadTarget})
	host.send(wire.FlagRelayControl, wire.RelayOpTransfer, 1, nil)
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpTransfer, Code: wire.RelayRejectBadTarget})
	a.send(wire.FlagRelayControl, wire.RelayOpTransfer, 3, nil)
	a.expectRejected(wire.RelayRejected{Op: wire.RelayOpTransfer, Code: wire.RelayRejectNotHost})
	a.send(wire.FlagRelayGame, 1, 0, []byte("still host"))
	host.expect(wire.RelayGame, 1, 2, "still host")
}

// 宽限期内同一账号重连回来仍是房主：成员的游戏帧在宽限期内丢弃并收到 host_changing；房主回来后成员收到
// host_resumed，房主收到在线成员名单，epoch 不变，lobby 看到房主下线又上线、房主不变。
func TestHostReturnsWithinGrace(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) { c.HostGrace = time.Hour })
	host, a, b := h.room3("room-grace")
	_ = host.conn.Close()
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "host" && r.Host == "host" && slices.Equal(r.Online, []string{"acc-a", "acc-b"})
	})
	a.send(wire.FlagRelayGame, 1, 0, []byte("lost"))
	a.expectEpoch(wire.RelayOpHostChanging, 0, 0)
	b.expectSilence(quiet)

	back, welcome := h.connect("room-grace", "host", token.RoleHost, 8)
	if !welcome.Host || back.slot != 1 {
		t.Fatalf("returning host welcome %+v slot %d", welcome, back.slot)
	}
	back.expectMember(wire.RelayOpMemberJoined, 2, wire.RelayReasonNone, "acc-a")
	back.expectMember(wire.RelayOpMemberJoined, 3, wire.RelayReasonNone, "acc-b")
	a.expectEpoch(wire.RelayOpHostResumed, 1, 0)
	b.expectEpoch(wire.RelayOpHostResumed, 1, 0)
	a.send(wire.FlagRelayGame, 2, 0, []byte("resumed"))
	back.expect(wire.RelayGame, 2, 2, "resumed")
	report := h.reports.wait(t, func(r relay.Report) bool { return r.Type == relay.EventMemberJoined && r.Account == "host" })
	if report.Epoch != 0 || report.Host != "host" {
		t.Fatalf("resume report %+v", report)
	}
	if n := h.reports.count(func(r relay.Report) bool { return r.Type == relay.EventHostChanged }); n != 0 {
		t.Fatalf("%d host_changed reports for a host that came back", n)
	}

	// 宽限期内房主再断开，又经 lobby 的 leave 离开：不再等宽限期，立即换 host。
	_ = back.conn.Close()
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "host" && r.Seq > report.Seq
	})
	a.send(wire.FlagRelayGame, 3, 0, []byte("lost again"))
	a.expectEpoch(wire.RelayOpHostChanging, 0, 0)
	h.server.Leave("room-grace", "host")
	a.expectYouAreHost(1)
}

// 宽限期满按候选顺序选人：房主声明了 [3, 2] 就先给 3；快照（seq 与年龄）交给新房主；迁移期间成员的帧丢弃并
// 收到新 epoch 的 host_changing。
func TestGraceExpiresAndSuccessorsAreHonoured(t *testing.T) {
	grace := 200 * time.Millisecond
	h := newHarness(t, false, func(c *relay.Config) { c.HostGrace = grace; c.HostReadyTimeout = time.Hour })
	host, a, b := h.room3("room-successors")
	host.send(wire.FlagRelayControl, wire.RelayOpSuccessors, 0, wire.EncodeRelaySuccessors([]uint32{3, 2}))
	host.snapshot(5, []byte("snap-5"))
	host.ping("sync") // 快照与候选顺序已处理
	lost := time.Now()
	_ = host.conn.Close()

	offer := b.expectYouAreHost(1)
	if waited := time.Since(lost); waited < grace {
		t.Fatalf("offer after %v, before the %v grace", waited, grace)
	}
	if offer.Source != wire.RelayStateSnapshot || offer.SnapshotSeq != 5 || string(offer.State) != "snap-5" ||
		offer.SnapshotAgeMillis < uint32(grace/time.Millisecond) || seatsOf(offer) != "host@1:off acc-a@2:on acc-b@3:on" {
		t.Fatalf("offer %+v seats %s", offer, seatsOf(offer))
	}
	a.send(wire.FlagRelayGame, 1, 0, []byte("lost"))
	a.expectEpoch(wire.RelayOpHostChanging, 0, 1)
	b.ready(1)
	a.expectHostChanged(1, 3, "acc-b")
	b.expectHostChanged(1, 3, "acc-b")
	a.send(wire.FlagRelayGame, 2, 0, []byte("back on"))
	b.expect(wire.RelayGame, 2, 2, "back on")

	// 新房主从旧快照的 seq 往上接：不大于 5 的被拒，6 收下。
	b.snapshot(5, []byte("x"))
	b.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectStaleSeq, Seq: 5})
	b.snapshot(6, []byte("snap-6"))
	b.ping("ok")
}

// 没有声明候选、或候选都不在线时，取在线成员里槽位最小的；旧房主的离线座位不当候选。
func TestCandidateFallsBackToTheSmallestOnlineSlot(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-fallback")
	c, _ := h.connect("room-fallback", "acc-c", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 4, wire.RelayReasonNone, "acc-c")
	host.send(wire.FlagRelayControl, wire.RelayOpSuccessors, 0, wire.EncodeRelaySuccessors([]uint32{9, 2}))
	_ = a.conn.Close() // 声明的 2 不在线了
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonDisconnected, "acc-a")
	host.send(wire.FlagRelayControl, wire.RelayOpLeave, 0, nil)
	host.expectClosed(wire.RelayReasonLeft)
	offer := b.expectYouAreHost(1) // 不等宽限期
	if offer.Source != wire.RelayStateNone || seatsOf(offer) != "host@1:off acc-a@2:off acc-b@3:on acc-c@4:on" {
		t.Fatalf("offer %+v seats %s", offer, seatsOf(offer))
	}
	c.expectSilence(quiet)
}

// 踢离线座位（方案 §6 第 12 条）：有座位但不在线的账号也能踢——记下账号、删座位、报 member_left{kicked}，房主收到
// MemberLeft{kicked} 确认；它再来被拒。换 host 时候选顺序里的这个槽位选不到它，you_are_host 的成员名单里也没有它。
func TestKickingAnOfflineSeat(t *testing.T) {
	h := newHarness(t, false, nil)
	host, a, b := h.room3("room-kick-offline")
	c, _ := h.connect("room-kick-offline", "acc-c", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 4, wire.RelayReasonNone, "acc-c")
	host.send(wire.FlagRelayControl, wire.RelayOpSuccessors, 0, wire.EncodeRelaySuccessors([]uint32{2, 4}))
	_ = a.conn.Close()
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonDisconnected, "acc-a")

	host.send(wire.FlagRelayControl, wire.RelayOpKick, 2, nil)
	host.expectMember(wire.RelayOpMemberLeft, 2, wire.RelayReasonKicked, "acc-a")
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "acc-a" && r.Slot == 2 && r.Reason == "kicked"
	})
	h.rejected(h.token("room-kick-offline", "acc-a", token.RoleMember, 8), wire.RelayReasonKicked)
	host.send(wire.FlagRelayControl, wire.RelayOpKick, 2, nil) // 座位已经没了：再踢什么也不做
	host.ping("kick2")

	host.send(wire.FlagRelayControl, wire.RelayOpLeave, 0, nil)
	host.expectClosed(wire.RelayReasonLeft)
	offer := c.expectYouAreHost(1) // 候选顺序 [2, 4]：2 被踢，轮到 4
	if seatsOf(offer) != "host@1:off acc-b@3:on acc-c@4:on" {
		t.Fatalf("offer seats %s, want the kicked acc-a gone", seatsOf(offer))
	}
	b.expectSilence(quiet)
}

// 新房主不在时限内回 host_ready：它收到 you_are_member，换下一个；候选断开也换下一个；都不行就关房。迟到的
// host_ready 不生效。
func TestHostReadyTimeoutMovesOnAndAllFailingCloses(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) { c.HostReadyTimeout = 200 * time.Millisecond })
	host, a, b := h.room3("room-ready")
	c, _ := h.connect("room-ready", "acc-c", token.RoleMember, 8)
	host.expectMember(wire.RelayOpMemberJoined, 4, wire.RelayReasonNone, "acc-c")
	h.server.Leave("room-ready", "host")
	host.expectClosed(wire.RelayReasonLeft)

	a.expectYouAreHost(1)
	a.expectEpoch(wire.RelayOpYouAreMember, 2, 1) // 不回：超时
	a.ready(1)                                    // 迟到：不生效
	offer := b.expectYouAreHost(1)
	if seatsOf(offer) != "host@1:off acc-a@2:on acc-b@3:on acc-c@4:on" {
		t.Fatalf("second offer seats %s", seatsOf(offer))
	}
	_ = b.conn.Close() // 候选断开：换下一个
	offer = c.expectYouAreHost(1)
	// 选人阶段报给 lobby 的仍是旧房主与旧 epoch：新房主回 host_ready 之前 lobby 不改房主（§4.6.4 第 7 步）。
	left := h.reports.wait(t, func(r relay.Report) bool { return r.Type == relay.EventMemberLeft && r.Account == "acc-b" })
	if left.Host != "host" || left.Epoch != 0 {
		t.Fatalf("report during the migration names host %q epoch %d", left.Host, left.Epoch)
	}
	if seatsOf(offer) != "host@1:off acc-a@2:on acc-b@3:off acc-c@4:on" {
		t.Fatalf("third offer seats %s", seatsOf(offer))
	}
	c.expectEpoch(wire.RelayOpYouAreMember, 4, 1)
	a.expectClosed(wire.RelayReasonRoomClosed)
	c.expectClosed(wire.RelayReasonRoomClosed)
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventClosed && r.Room == "room-ready" && r.Reason == "room_closed"
	})
	if n := h.reports.count(func(r relay.Report) bool { return r.Type == relay.EventHostChanged }); n != 0 {
		t.Fatalf("%d host_changed reports for a failed migration", n)
	}
}

// 房主连接单独的心跳超时（HostIdleTimeout，§4.6.3 推荐 2s）：房主不发帧就当断线进宽限期；成员仍按 IdleTimeout。
func TestHostHeartbeatTimeoutStartsTheGrace(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) { c.HostIdleTimeout = 500 * time.Millisecond; c.HostGrace = time.Hour })
	host, a, _ := h.room3("room-host-idle")
	host.expectClosed(wire.RelayReasonIdleTimeout)
	h.reports.wait(t, func(r relay.Report) bool {
		return r.Type == relay.EventMemberLeft && r.Account == "host" && r.Reason == "idle_timeout"
	})
	a.ping("member alive")
	a.send(wire.FlagRelayGame, 1, 0, nil)
	a.expectEpoch(wire.RelayOpHostChanging, 0, 0)
}

// 快照：只认房主的；seq 必须递增；同一房主两份之间不少于最小间隔；超过上限拒收、连接保留，超过两倍按超大帧断开；
// 只留最新一份，换 host 时交出去。转交的交接数据同样受上限。
func TestSnapshotLimits(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) {
		c.SnapshotMaxBytes = 1024
		c.SnapshotMinInterval = 300 * time.Millisecond
		c.MaxPayloadBytes = 512
		c.RoomBytesPerSecond = 1024
		c.HostGrace = 100 * time.Millisecond
	})
	host, a, b := h.room3("room-snapshot")

	host.snapshot(1, []byte("one"))
	host.snapshot(2, []byte("too soon"))
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooSoon, Seq: 2})
	time.Sleep(350 * time.Millisecond)
	host.snapshot(1, []byte("stale"))
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectStaleSeq, Seq: 1})
	host.snapshot(3, bytes.Repeat([]byte{7}, 1025)) // 大于 MaxPayloadBytes 也能发；超过 SnapshotMaxBytes 拒收
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectTooBig, Seq: 3})
	host.send(wire.FlagRelayControl, wire.RelayOpTransfer, 2, bytes.Repeat([]byte{7}, 1025))
	host.expectRejected(wire.RelayRejected{Op: wire.RelayOpTransfer, Code: wire.RelayRejectTooBig})
	full := bytes.Repeat([]byte{9}, 1024)
	host.snapshot(4, full) // 正好上限：收下
	host.ping("kept")
	a.snapshot(9, []byte("member")) // 成员发的快照拒收
	a.expectRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: wire.RelayRejectNotHost, Seq: 9})
	b.expectSilence(quiet)

	// 超过两倍上限：当超大帧断开（房主断线，进宽限期，期满换 host，交出去的是最新一份 seq 4）。
	host.snapshot(5, make([]byte, 2*1024+1))
	host.expectClosed(wire.RelayReasonFrameTooBig)
	offer := a.expectYouAreHost(1)
	if offer.Source != wire.RelayStateSnapshot || offer.SnapshotSeq != 4 || !bytes.Equal(offer.State, full) {
		t.Fatalf("offer seq %d source %d state %d bytes", offer.SnapshotSeq, offer.Source, len(offer.State))
	}
}

// 新房主候选在拿到 you_are_host 后顶号重连（例如刷新页面）：you_are_host 重新发给新连接，计时重新开始。
func TestCandidateReconnectGetsTheOfferAgain(t *testing.T) {
	h := newHarness(t, false, func(c *relay.Config) { c.HostReadyTimeout = time.Hour })
	host, a, _ := h.room3("room-candidate-replace")
	host.send(wire.FlagRelayControl, wire.RelayOpTransfer, 2, []byte("h"))
	host.expectEpoch(wire.RelayOpYouAreMember, 1, 1)
	a.expectYouAreHost(1)
	a2, welcome := h.connect("room-candidate-replace", "acc-a", token.RoleMember, 8)
	if welcome.Host || welcome.HostSlot != 0 {
		t.Fatalf("candidate reconnect welcome %+v", welcome)
	}
	a.expectClosed(wire.RelayReasonReplaced)
	if offer := a2.expectYouAreHost(1); string(offer.State) != "h" {
		t.Fatalf("re-offer %+v", offer)
	}
	a2.ready(1)
	a2.expectHostChanged(1, 2, "acc-a")
	host.expectHostChanged(1, 2, "acc-a")
}
