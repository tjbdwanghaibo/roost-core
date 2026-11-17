package relay

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// firstSlot 是建房连接（第一个房主）的槽位；之后进房的新账号依次取 2、3……。0 是“全体”。
const firstSlot uint32 = 1

// phase 是房间的房主状态（方案 §4.6）。
type phase uint8

const (
	// phaseStable：有在线的房主，游戏帧正常转发。
	phaseStable phase = iota
	// phaseGrace：房主断线，等它在 HostGrace 内用同一账号重连回来（§4.6.1）。成员发给房主的游戏帧丢弃。
	phaseGrace
	// phaseMigrating：正在选新房主（§4.6.4）：已给候选发 you_are_host，等它在 HostReadyTimeout 内回 host_ready。
	// 这期间没有房主，成员发给房主的游戏帧丢弃。
	phaseMigrating
)

// room 是一个房间。全部字段（以及 conn.changingSeen）由 mu 保护；方法在锁内只改状态、往连接的发送队列里放帧
// （不阻塞），报告在释放锁之后发出（room.do）。宽限期与等 host_ready 的计时器回调也在锁内执行，所以一个房间的
// 换 host 步骤（§4.6.4）是单线程的：步骤之间插不进别的事件。
//
// 角色以这里的状态为准（§4.6.5）：只有 host 指向的连接是房主，不看令牌里的角色字段；换 host 之后旧房主拿着
// 没过期的 host 令牌重连只是普通成员。
//
// 槽位属于账号，房间存续期间不变（seats）：成员掉线重连拿回原来的槽位，房主的候选顺序（successors）按槽位
// 声明也不会指错人；新账号取下一个没用过的槽位，所以槽位小的就是进房早的。被踢的账号删掉座位、不能再进。
type room struct {
	server     *Server
	id         string
	maxMembers int
	opened     Report // 建房时的心跳，admit 在放锁之后发出

	mu     sync.Mutex
	closed bool
	seq    uint64 // 最近一条报告的序号（Report.Seq），锁内递增

	phase phase
	// epoch 是 host_epoch：建房时为 0，每次进入选人阶段加 1（§4.6.4 第 1 步）；控制帧里带的就是它。
	epoch uint64
	// hostAccount / hostSlot / hostEpoch 是最近一个已就绪的房主（宽限期内是断线的那个）。报告给 lobby 的
	// Host、Epoch 用它们：选人阶段仍报旧房主与旧 epoch，新房主回了 host_ready 才报新的（§4.6.4 第 7 步）。
	hostAccount string
	hostSlot    uint32
	hostEpoch   uint64
	host        *conn // 在线的房主；宽限期与选人阶段为 nil

	members  map[uint32]*conn  // 槽位 → 在线连接，含房主
	seats    map[string]uint32 // 账号 → 槽位，含离线的；被踢的删掉
	nextSlot uint32
	muted    map[string]bool // 被禁言房间通道的账号（顶号后仍禁言）
	kicked   map[string]bool // 被踢的账号：房间存续期间不能再进（lobby 的被踢名单是权威，这里只挡报告到达之前）

	aloneSince time.Time // 只剩在线房主的起点；有成员在线、或没有在线房主时为零

	successors []uint32 // 当前房主声明的候选顺序（§4.6.2），换 host 后清空
	snapshot   snapshot // 最新一份快照（§4.6.3）

	// 选人阶段的状态。
	candidate  *conn           // 收到 you_are_host、还没回 host_ready 的候选
	tried      map[uint32]bool // 这一轮已经试过（或不能当候选）的槽位：旧房主、超时或断开的候选
	handoff    []byte          // 主动转交带来的交接数据，这一轮的每个候选都拿它
	hasHandoff bool
	changeGen  uint64 // 每次房主离开（进宽限期或选人）加 1：每个成员每次最多收到一条 host_changing

	timer    *time.Timer // 宽限期或 host_ready 期限
	timerGen uint64      // 计时器代次：停掉或重排后，已经在路上的旧回调看到代次不符就什么也不做
}

// snapshot 是房主推来的最新快照：relay 只存不解，换 host 时交给新房主。
type snapshot struct {
	held  bool
	seq   uint64
	data  []byte
	at    time.Time // relay 收到的时刻（交给新房主时换算成年龄）
	epoch uint64    // 收到时的 host_epoch：最小间隔只在同一个房主的快照之间算
}

func newRoom(s *Server, claims token.Claims, host *conn, now time.Time) *room {
	r := &room{server: s, id: claims.Room, maxMembers: claims.MaxMembers,
		hostAccount: claims.Account, hostSlot: firstSlot, host: host,
		members: map[uint32]*conn{firstSlot: host}, seats: map[string]uint32{claims.Account: firstSlot}, nextSlot: firstSlot + 1,
		muted: map[string]bool{}, kicked: map[string]bool{}, aloneSince: now}
	host.slot = firstSlot
	host.setIdleLimit(s.config.HostIdleTimeout)
	host.enqueueControl(wire.RelayOpWelcome, firstSlot, wire.EncodeRelayWelcome(wire.RelayWelcome{Host: true, HostSlot: firstSlot}))
	r.opened = r.reportLocked(EventHeartbeat)
	return r
}

// effects 是一次锁内处理要在放锁之后做的事：发报告；房间在这次处理里关了，就从 Server 的表里摘掉。
type effects struct {
	reports []Report
}

func (e *effects) report(report Report) { e.reports = append(e.reports, report) }

// do 在锁内执行 fn，放锁后发报告；fn 关了房间时摘掉它（锁的顺序：room.mu 放掉之后才取 Server.mu）。
func (r *room) do(fn func(*effects)) {
	var eff effects
	r.mu.Lock()
	wasOpen := !r.closed
	fn(&eff)
	closedNow := wasOpen && r.closed
	r.mu.Unlock()
	r.emit(eff.reports)
	if closedNow {
		r.server.forgetRoom(r)
	}
}

// isKicked 报告账号是否被踢过（Server.checkWithLobby 在验权之前先挡一次）。
func (r *room) isKicked(account string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kicked[account]
}

// join 让一条已验签、已验权的连接进入已建成的房间。返回非 RelayReasonNone 时连接没有进房。
func (r *room) join(claims token.Claims, c *conn, now time.Time) wire.RelayReason {
	reason := wire.RelayReasonNone
	r.do(func(eff *effects) { reason = r.joinLocked(claims.Account, c, now, eff) })
	return reason
}

func (r *room) joinLocked(account string, c *conn, now time.Time, eff *effects) wire.RelayReason {
	if r.closed {
		return wire.RelayReasonRoomNotFound
	}
	if r.kicked[account] {
		return wire.RelayReasonKicked
	}
	slot, seated := r.seats[account]
	if old := r.members[slot]; seated && old != nil {
		// 同一账号已在线：新连接顶掉旧连接，槽位与角色不变。
		r.members[slot], c.slot = c, slot
		old.finish(wire.RelayReasonReplaced, true)
		if old == r.host {
			r.host = c
			c.setIdleLimit(r.server.config.HostIdleTimeout)
		}
		r.welcomeLocked(c)
		if old == r.candidate { // 候选换了连接：把 you_are_host 重新发给新连接，重新计时
			r.offerLocked(c, now)
		}
		if to := r.hostSideLocked(); to != nil && to != c {
			to.enqueueControl(wire.RelayOpMemberJoined, slot, wire.EncodeRelayMember(wire.RelayMember{Reason: wire.RelayReasonReplaced, Account: account}))
		}
		return wire.RelayReasonNone
	}
	if seated && r.phase == phaseGrace && account == r.hostAccount {
		// 宽限期内房主用同一账号回来：仍是房主，不换（§4.6.1），通知成员 host_resumed。
		r.stopTimerLocked()
		r.phase, r.host = phaseStable, c
		r.members[slot], c.slot = c, slot
		c.setIdleLimit(r.server.config.HostIdleTimeout)
		r.welcomeLocked(c)
		// 房主断线期间可能有人进出：把此刻在线的成员按槽位逐个用 MemberJoined 再告诉它一遍。
		resumed := wire.EncodeRelayEpoch(r.epoch)
		for _, seat := range r.seatsLocked() {
			if m := r.members[seat.Slot]; m != nil && m != c {
				m.enqueueControl(wire.RelayOpHostResumed, slot, resumed)
				c.enqueueControl(wire.RelayOpMemberJoined, seat.Slot, wire.EncodeRelayMember(wire.RelayMember{Account: seat.Account}))
			}
		}
		r.joinedLocked(c, now, eff)
		return wire.RelayReasonNone
	}
	reserved := 0
	if r.phase == phaseGrace {
		reserved = 1 // 宽限期内给断线的房主留着位置
	}
	if len(r.members)+reserved >= r.maxMembers {
		return wire.RelayReasonRoomFull
	}
	if !seated {
		slot = r.nextSlot
		r.nextSlot++
		r.seats[account] = slot
	}
	r.members[slot], c.slot = c, slot
	r.welcomeLocked(c)
	if to := r.hostSideLocked(); to != nil {
		to.enqueueControl(wire.RelayOpMemberJoined, slot, wire.EncodeRelayMember(wire.RelayMember{Account: account}))
	}
	r.joinedLocked(c, now, eff)
	return wire.RelayReasonNone
}

// joinedLocked 记一条进房报告。
func (r *room) joinedLocked(c *conn, now time.Time, eff *effects) {
	r.updateAloneLocked(now)
	report := r.reportLocked(EventMemberJoined)
	report.Slot, report.Account = c.slot, c.account
	eff.report(report)
}

// welcomeLocked 告诉刚进房的连接它的槽位与角色、当前房主的槽位（选人阶段为 0）；被禁言的账号再告诉它一次禁言状态。
func (r *room) welcomeLocked(c *conn) {
	hostSlot := r.hostSlot
	if r.phase == phaseMigrating {
		hostSlot = 0
	}
	c.enqueueControl(wire.RelayOpWelcome, c.slot, wire.EncodeRelayWelcome(wire.RelayWelcome{Host: c == r.host, HostSlot: hostSlot}))
	if r.muted[c.account] {
		c.enqueueControl(wire.RelayOpMuted, c.slot, []byte{1})
	}
}

// hostSideLocked 是成员进出通知的收件人：在线的房主；选人阶段是候选（它已拿到成员名单，之后的变化逐条告诉它）；
// 宽限期没有收件人。
func (r *room) hostSideLocked() *conn {
	switch r.phase {
	case phaseStable:
		return r.host
	case phaseMigrating:
		return r.candidate
	}
	return nil
}

// leave 在连接的读循环结束后调用。已被顶号、踢出或房间已关的连接不再是房间的当前连接，这里什么也不做。
func (r *room) leave(c *conn) {
	r.do(func(eff *effects) {
		if r.closed || r.members[c.slot] != c {
			return
		}
		r.connGoneLocked(c, c.closeReason(), eff)
	})
}

// connGoneLocked 处理一条在线连接的离开：房主走了进宽限期或换 host；候选走了换下一个候选；其余是普通成员离开。
func (r *room) connGoneLocked(c *conn, reason wire.RelayReason, eff *effects) {
	switch c {
	case r.host:
		r.hostLostLocked(c, reason, eff)
	case r.candidate:
		r.removeLocked(c, reason, eff)
		r.tried[c.slot] = true
		r.candidate = nil
		r.offerNextLocked(eff)
	default:
		r.removeLocked(c, reason, eff)
	}
}

// hostLostLocked 处理房主连接的结束（§4.6.1）：
//   - 房主离开（RelayOpLeave 或 lobby 的 leave）：不等，立即用 relay 存的快照换 host；
//   - 断线（读写出错、心跳超时、超限被断开）：没有在线成员就关房；否则进宽限期，期间同一账号重连回来仍是房主，
//     期满再换 host。
func (r *room) hostLostLocked(c *conn, reason wire.RelayReason, eff *effects) {
	delete(r.members, c.slot)
	r.host = nil
	now := r.server.now()
	if len(r.members) == 0 {
		r.closeLocked(wire.RelayReasonRoomClosed, eff)
		return
	}
	report := r.reportLocked(EventMemberLeft)
	report.Slot, report.Account, report.Reason = c.slot, c.account, reason.String()
	eff.report(report)
	if reason == wire.RelayReasonLeft {
		r.startMigrationLocked(nil, 0, nil, false, now, eff)
		return
	}
	r.phase = phaseGrace
	r.changeGen++
	r.updateAloneLocked(now)
	r.scheduleLocked(r.server.config.HostGrace, func(gen uint64) {
		r.do(func(eff *effects) {
			if !r.closed && r.phase == phaseGrace && gen == r.timerGen {
				r.startMigrationLocked(nil, 0, nil, false, r.server.now(), eff)
			}
		})
	})
}

// startMigrationLocked 是 §4.6.4 第 1～2、4 步：epoch 加 1、进入选人阶段；旧房主的连接还在（主动转交）就降为成员
// 并收到 you_are_member；然后把 you_are_host 发给目标（主动转交）或按候选顺序选出的人。选不出人就关房。
func (r *room) startMigrationLocked(old *conn, target uint32, handoff []byte, hasHandoff bool, now time.Time, eff *effects) {
	r.stopTimerLocked()
	r.phase = phaseMigrating
	r.epoch++
	r.changeGen++
	r.host = nil
	r.tried = map[uint32]bool{r.hostSlot: true} // 旧房主不当这一轮的候选
	r.handoff, r.hasHandoff = handoff, hasHandoff
	r.aloneSince = time.Time{}
	if old != nil {
		old.setIdleLimit(r.server.config.IdleTimeout)
		old.enqueueControl(wire.RelayOpYouAreMember, old.slot, wire.EncodeRelayEpoch(r.epoch))
	}
	if c := r.members[target]; target != 0 && c != nil {
		r.offerLocked(c, now)
		return
	}
	r.offerNextLocked(eff)
}

// offerNextLocked 按候选顺序选下一个人（§4.6.2）；没人可选就关房（§4.6.4 第 5 步“都失败”）。
func (r *room) offerNextLocked(eff *effects) {
	if c := r.pickCandidateLocked(); c != nil {
		r.offerLocked(c, r.server.now())
		return
	}
	r.closeLocked(wire.RelayReasonRoomClosed, eff)
}

// pickCandidateLocked：按房主声明的候选顺序取第一个此刻在线、这一轮没试过的成员；没有声明或候选都不在时，
// 取在线成员里槽位最小的（最早进房的）。只看在线与否，不看延迟或玩法状态。
func (r *room) pickCandidateLocked() *conn {
	for _, slot := range r.successors {
		if c := r.members[slot]; c != nil && !r.tried[slot] {
			return c
		}
	}
	var best *conn
	for slot, c := range r.members {
		if !r.tried[slot] && (best == nil || slot < best.slot) {
			best = c
		}
	}
	return best
}

// offerLocked 是 §4.6.4 第 4 步：把 you_are_host 发给候选（成员名单、交接数据或快照），开始等 host_ready。
func (r *room) offerLocked(c *conn, now time.Time) {
	r.candidate = c
	offer := wire.RelayYouAreHost{Epoch: r.epoch, Members: r.seatsLocked()}
	// 快照 seq 在房间内跨房主递增：不管这一轮交出的是交接数据还是快照，都带上 relay 存着的最新 seq，
	// 新房主从它往上接，否则主动转交后新房主的第一份快照会被当成 stale_seq 拒收。
	if r.snapshot.held {
		offer.SnapshotSeq = r.snapshot.seq
	}
	switch {
	case r.hasHandoff:
		offer.Source, offer.State = wire.RelayStateHandoff, r.handoff
	case r.snapshot.held:
		age := now.Sub(r.snapshot.at).Milliseconds()
		offer.Source, offer.State = wire.RelayStateSnapshot, r.snapshot.data
		offer.SnapshotAgeMillis = uint32(min(max(age, 0), math.MaxUint32))
	}
	c.enqueueControl(wire.RelayOpYouAreHost, c.slot, wire.EncodeRelayYouAreHost(offer))
	r.scheduleLocked(r.server.config.HostReadyTimeout, func(gen uint64) {
		r.do(func(eff *effects) {
			if r.closed || r.phase != phaseMigrating || r.candidate != c || gen != r.timerGen {
				return
			}
			// 超时：这个候选不行了，告诉它仍是成员，换下一个（§4.6.4 第 5 步）。
			r.tried[c.slot] = true
			r.candidate = nil
			c.enqueueControl(wire.RelayOpYouAreMember, c.slot, wire.EncodeRelayEpoch(r.epoch))
			r.offerNextLocked(eff)
		})
	})
}

// seatsLocked 是 you_are_host 里的成员名单：房间里有座位的账号（含离线的、不含被踢的），按槽位排列。
func (r *room) seatsLocked() []wire.RelaySeat {
	seats := make([]wire.RelaySeat, 0, len(r.seats))
	for account, slot := range r.seats {
		seats = append(seats, wire.RelaySeat{Slot: slot, Account: account, Online: r.members[slot] != nil})
	}
	slices.SortFunc(seats, func(a, b wire.RelaySeat) int { return int(a.Slot) - int(b.Slot) })
	return seats
}

// hostReady 是 §4.6.4 第 5～7 步的收尾：当前候选回了这一轮的 host_ready，它就是新房主；向房间里每个人广播
// host_changed、恢复转发，并报告 lobby（room.host_changed）。迟到的、别的连接发的、epoch 不对的都忽略。
func (r *room) hostReady(c *conn, epoch uint64) {
	r.do(func(eff *effects) {
		if r.closed || r.phase != phaseMigrating || r.candidate != c || epoch != r.epoch {
			return
		}
		r.stopTimerLocked()
		r.phase, r.host, r.candidate = phaseStable, c, nil
		r.hostAccount, r.hostSlot, r.hostEpoch = c.account, c.slot, r.epoch
		r.handoff, r.hasHandoff, r.tried, r.successors = nil, false, nil, nil
		c.setIdleLimit(r.server.config.HostIdleTimeout)
		changed := wire.EncodeRelayHostChanged(wire.RelayHostChanged{Epoch: r.epoch, Account: c.account})
		for _, m := range r.members {
			m.enqueueControl(wire.RelayOpHostChanged, c.slot, changed)
		}
		r.updateAloneLocked(r.server.now())
		report := r.reportLocked(EventHostChanged)
		report.Slot, report.Account = c.slot, c.account
		eff.report(report)
	})
}

// transfer 是房主的主动转交（§4.6.1）：不等，旧房主降为成员，交接数据原样交给目标。目标不在线、是自己、
// 发送者此刻不是房主时拒收（RelayOpRejected），房间不变。交接数据的大小在读帧时已检查过。
func (r *room) transfer(from *conn, target uint32, handoff []byte) {
	r.do(func(eff *effects) {
		if r.closed || r.members[from.slot] != from {
			return
		}
		code := wire.RelayRejectCode(0)
		switch {
		case r.phase != phaseStable || from != r.host:
			code = wire.RelayRejectNotHost
		case target == from.slot || r.members[target] == nil:
			code = wire.RelayRejectBadTarget
		}
		if code != 0 {
			from.enqueueControl(wire.RelayOpRejected, 0, wire.EncodeRelayRejected(wire.RelayRejected{Op: wire.RelayOpTransfer, Code: code}))
			return
		}
		r.startMigrationLocked(from, target, handoff, true, r.server.now(), eff)
	})
}

// setSuccessors 记下房主声明的候选顺序（只记最新一份，不判断合不合理）。不是房主发的忽略。
func (r *room) setSuccessors(from *conn, slots []uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.phase == phaseStable && from == r.host {
		r.successors = slots
	}
}

// storeSnapshot 存房主推来的快照（§4.6.3）：只留最新一份，seq 必须递增（跨房主），同一房主两份之间不少于
// SnapshotMinInterval；不满足的拒收并告诉房主。大小在读帧时已检查过。
func (r *room) storeSnapshot(from *conn, s wire.RelaySnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.members[from.slot] != from {
		return
	}
	now := r.server.now()
	code := wire.RelayRejectCode(0)
	switch {
	case r.phase != phaseStable || from != r.host:
		code = wire.RelayRejectNotHost
	case s.Seq <= r.snapshot.seq:
		code = wire.RelayRejectStaleSeq
	case r.snapshot.held && r.snapshot.epoch == r.epoch && now.Sub(r.snapshot.at) < r.server.config.SnapshotMinInterval:
		code = wire.RelayRejectTooSoon
	}
	if code != 0 {
		from.enqueueControl(wire.RelayOpRejected, 0, wire.EncodeRelayRejected(wire.RelayRejected{Op: wire.RelayOpSnapshot, Code: code, Seq: s.Seq}))
		return
	}
	// s.Data 是读帧时新分配的载荷的一段，之后没人再写它，直接留着。
	r.snapshot = snapshot{held: true, seq: s.Seq, data: s.Data, at: now, epoch: r.epoch}
}

// removeLocked 把一个成员移出在线名单（座位留着，重连拿回原来的槽位）并通知房主一侧。
func (r *room) removeLocked(c *conn, reason wire.RelayReason, eff *effects) {
	delete(r.members, c.slot)
	if to := r.hostSideLocked(); to != nil && to != c {
		to.enqueueControl(wire.RelayOpMemberLeft, c.slot, wire.EncodeRelayMember(wire.RelayMember{Reason: reason, Account: c.account}))
	}
	r.updateAloneLocked(r.server.now())
	report := r.reportLocked(EventMemberLeft)
	report.Slot, report.Account, report.Reason = c.slot, c.account, reason.String()
	eff.report(report)
}

// remove 是 lobby 的 leave 命令（玩家调了 lobby 的 leave）：把账号移出房间，等同于它自己离开。是在线房主时按
// 房主离开处理（不等宽限期，立即换 host）；是宽限期里断线的房主时同样立即换 host。别的不在线账号什么也不做。
func (r *room) remove(account string) {
	r.do(func(eff *effects) {
		if r.closed {
			return
		}
		slot, seated := r.seats[account]
		c := r.members[slot]
		switch {
		case !seated:
		case c != nil:
			c.finish(wire.RelayReasonLeft, true)
			r.connGoneLocked(c, wire.RelayReasonLeft, eff)
		case r.phase == phaseGrace && account == r.hostAccount:
			r.startMigrationLocked(nil, 0, nil, false, r.server.now(), eff)
		}
	})
}

// close 从外部关房（停机、空房超时）。
func (r *room) close(reason wire.RelayReason) {
	r.do(func(eff *effects) { r.closeLocked(reason, eff) })
}

func (r *room) closeLocked(reason wire.RelayReason, eff *effects) {
	if r.closed {
		return
	}
	r.closed = true
	r.stopTimerLocked()
	report := r.reportLocked(EventClosed)
	report.Online, report.Reason = nil, reason.String()
	eff.report(report)
	for _, c := range r.members {
		c.finish(reason, true)
	}
	clear(r.members)
	r.host, r.candidate, r.snapshot, r.handoff = nil, nil, snapshot{}, nil
}

// heartbeat 发一条心跳；只剩房主超过 EmptyRoomTimeout 时改为关房。
func (r *room) heartbeat(now time.Time) {
	r.do(func(eff *effects) {
		if r.closed {
			return
		}
		if !r.aloneSince.IsZero() && now.Sub(r.aloneSince) >= r.server.config.EmptyRoomTimeout {
			r.closeLocked(wire.RelayReasonIdleTimeout, eff)
			return
		}
		eff.report(r.reportLocked(EventHeartbeat))
	})
}

// updateAloneLocked 维护“只剩房主”的起点：有在线房主且没有在线成员时开始计时，否则清零。
func (r *room) updateAloneLocked(now time.Time) {
	switch {
	case r.phase != phaseStable || r.host == nil || len(r.members) > 1:
		r.aloneSince = time.Time{}
	case r.aloneSince.IsZero():
		r.aloneSince = now
	}
}

// forwardGame 转发游戏通道的帧。有房主时：成员的帧只到房主（Seq 改写为成员槽位，成员写的目标不看）；房主的帧按
// Seq 的目标转给某个成员或全体成员。房主不在（宽限期、选人阶段）时成员的帧丢弃，发送者收到一次 host_changing
// （§4.6.4 第 3 步，relay 不缓存）；降为成员的旧房主、还没回 host_ready 的候选发的帧同样不再按房主帧转出。
func (r *room) forwardGame(from *conn, p *wire.Packet) {
	data, ok := encodeDown(p, from.slot)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.members[from.slot] != from {
		return
	}
	if r.phase == phaseStable {
		if from == r.host {
			r.sendLocked(from, p.Seq, data)
		} else {
			r.host.enqueue(data)
		}
		return
	}
	if from != r.candidate && from.changingSeen != r.changeGen {
		from.changingSeen = r.changeGen
		from.enqueueControl(wire.RelayOpHostChanging, 0, wire.EncodeRelayEpoch(r.epoch))
	}
}

// forwardRoom 转发房间通道的帧：按 Seq 的目标直达某个槽位或全体（不含发送者），房主也是收件人；换 host 期间
// 照常转发（不需要房主）。被禁言的账号的房间帧丢弃。限额在连接的读循环里先扣过。
func (r *room) forwardRoom(from *conn, p *wire.Packet) {
	data, ok := encodeDown(p, from.slot)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.members[from.slot] != from || r.muted[from.account] {
		return
	}
	r.sendLocked(from, p.Seq, data)
}

// sendLocked 把 data 放进目标槽位（RelayAll 为发送者以外的全体）的发送队列。
func (r *room) sendLocked(from *conn, target uint32, data []byte) {
	if target != wire.RelayAll {
		if to := r.members[target]; to != nil && to != from {
			to.enqueue(data)
		}
		return
	}
	for _, to := range r.members {
		if to != from {
			to.enqueue(data)
		}
	}
}

// kick 是房主的踢人控制帧：踢出槽位 slot 上的成员，删掉它的座位并记下账号，房间存续期间不让它再进。
// 报告里的原因是 kicked，lobby 据此把账号写进被踢名单（之后 join 与验权都拒绝它）。
//
// 有座位但不在线的账号也能踢（方案 §6 第 12 条：已开始的房间里掉线的人一直占着 lobby 的席位，要房主踢掉才空出来）：
// 同样记下账号、删座位、报 member_left{kicked}，房主收到 MemberLeft{kicked} 作为确认。座位删掉后它的槽位不会再
// 有连接（槽位不复用、账号进不来），所以候选顺序里留着这个槽位也选不到它。
func (r *room) kick(from *conn, slot uint32) {
	r.do(func(eff *effects) {
		if r.closed || r.phase != phaseStable || from != r.host || slot == r.hostSlot {
			return
		}
		if target := r.members[slot]; target != nil {
			r.kicked[target.account] = true
			delete(r.seats, target.account)
			target.finish(wire.RelayReasonKicked, true)
			r.removeLocked(target, wire.RelayReasonKicked, eff)
			return
		}
		for account, seat := range r.seats {
			if seat != slot {
				continue
			}
			r.kicked[account] = true
			delete(r.seats, account)
			from.enqueueControl(wire.RelayOpMemberLeft, slot, wire.EncodeRelayMember(wire.RelayMember{Reason: wire.RelayReasonKicked, Account: account}))
			report := r.reportLocked(EventMemberLeft)
			report.Slot, report.Account, report.Reason = slot, account, wire.RelayReasonKicked.String()
			eff.report(report)
			return
		}
	})
}

// mute 是房主的禁言控制帧：on 为 true 时槽位 slot 上的账号发不出房间通道的帧，false 解除。被禁言者收到 RelayOpMuted。
func (r *room) mute(from *conn, slot uint32, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.phase != phaseStable || from != r.host || slot == r.hostSlot {
		return
	}
	target := r.members[slot]
	if target == nil {
		return
	}
	if on {
		r.muted[target.account] = true
	} else {
		delete(r.muted, target.account)
	}
	flag := byte(0)
	if on {
		flag = 1
	}
	target.enqueueControl(wire.RelayOpMuted, slot, []byte{flag})
}

// scheduleLocked 排一个房间计时器（同一时刻只有一个：宽限期或 host_ready 期限）。fire 在计时器 goroutine 上调用，
// 自己取锁，并用 gen 核对计时器没被停掉或重排。
func (r *room) scheduleLocked(d time.Duration, fire func(gen uint64)) {
	r.stopTimerLocked()
	gen := r.timerGen
	r.timer = time.AfterFunc(d, func() { fire(gen) })
}

func (r *room) stopTimerLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.timerGen++
}

// reportLocked 生成一条带全量快照的报告并占用下一个序号。Host / Epoch 是最近一个已就绪的房主（选人阶段仍是旧的）。
func (r *room) reportLocked(event string) Report {
	r.seq++
	online := make([]string, 0, len(r.members))
	slots := make([]uint32, 0, len(r.members))
	for slot := range r.members {
		slots = append(slots, slot)
	}
	slices.Sort(slots)
	for _, slot := range slots {
		online = append(online, r.members[slot].account)
	}
	return Report{Type: event, Room: r.id, Relay: r.server.config.Instance, Seq: r.seq, Online: online, Host: r.hostAccount, Epoch: r.hostEpoch}
}

func (r *room) emit(reports []Report) {
	for _, report := range reports {
		r.server.report.Report(report)
	}
}

// encodeDown 把客户端发来的数据帧编码成转出的帧：通道、推送位、载荷类型、message_id、载荷原样，Seq 改写为发送者槽位。
// 编码一次，多个收件人共用同一份只读字节。帧头在读入时已按同一规则校验过，编码失败只是防御。
func encodeDown(p *wire.Packet, fromSlot uint32) ([]byte, bool) {
	data, err := wire.EncodeRelay([]*wire.Packet{{Flags: p.Flags, MsgID: p.MsgID, Seq: fromSlot, Payload: p.Payload}}, max(len(p.Payload), 1))
	return data, err == nil
}
