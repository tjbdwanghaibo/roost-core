package relay

import (
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// hostSlot 是房主的槽位：建房的连接占 1 号，成员从 2 号起取最小的空槽位。0 是“全体”。
const hostSlot uint32 = 1

// room 是一个房间。全部字段由 mu 保护；方法在锁内只改状态、往连接的发送队列里放帧（不阻塞），
// 报告在释放锁之后经 Server.report 发出。
//
// 角色以这里的状态为准（§4.6.5）：账号等于 hostAccount 的连接就是房主，不看它令牌里的角色字段。
type room struct {
	server     *Server
	id         string
	maxMembers int
	opened     Report // 建房时的报告，admit 在放锁之后发出

	mu          sync.Mutex
	closed      bool
	hostAccount string
	host        *conn
	members     map[uint32]*conn  // 槽位 → 在线连接，含房主
	slots       map[string]uint32 // 账号 → 槽位，只含在线的
	muted       map[string]bool   // 被禁言房间通道的账号（顶号后仍禁言）
	kicked      map[string]bool   // 被踢的账号：房间存续期间不能再进
	aloneSince  time.Time         // 只剩房主的起点；有成员在线时为零
}

func newRoom(s *Server, claims token.Claims, host *conn, now time.Time) *room {
	r := &room{server: s, id: claims.Room, maxMembers: claims.MaxMembers, hostAccount: claims.Account, host: host,
		members: map[uint32]*conn{hostSlot: host}, slots: map[string]uint32{claims.Account: hostSlot},
		muted: map[string]bool{}, kicked: map[string]bool{}, aloneSince: now}
	host.slot = hostSlot
	host.enqueueControl(wire.RelayOpWelcome, hostSlot, wire.EncodeRelayWelcome(wire.RelayWelcome{Host: true, HostSlot: hostSlot}))
	r.opened = r.reportLocked(EventOpened, now)
	return r
}

// join 让一条已验签的连接进入已建成的房间。同一账号已在线时新连接顶掉旧连接，槽位与角色不变；
// 否则按在线人数与上限分配最小的空槽位。返回非 RelayReasonNone 时连接没有进房。
func (r *room) join(claims token.Claims, c *conn, now time.Time) wire.RelayReason {
	var reports []Report
	r.mu.Lock()
	reason := r.joinLocked(claims.Account, c, now, &reports)
	r.mu.Unlock()
	r.emit(reports)
	return reason
}

func (r *room) joinLocked(account string, c *conn, now time.Time, reports *[]Report) wire.RelayReason {
	if r.closed {
		return wire.RelayReasonRoomNotFound
	}
	if r.kicked[account] {
		return wire.RelayReasonKicked
	}
	if slot, online := r.slots[account]; online {
		old := r.members[slot]
		r.members[slot], c.slot = c, slot
		old.finish(wire.RelayReasonReplaced, true)
		if slot == hostSlot {
			r.host = c
		}
		r.welcomeLocked(c)
		if slot != hostSlot {
			r.host.enqueueControl(wire.RelayOpMemberJoined, slot, wire.EncodeRelayMember(wire.RelayMember{Reason: wire.RelayReasonReplaced, Account: account}))
		}
		return wire.RelayReasonNone
	}
	if len(r.members) >= r.maxMembers {
		return wire.RelayReasonRoomFull
	}
	slot := hostSlot + 1
	for r.members[slot] != nil {
		slot++
	}
	r.members[slot], r.slots[account], c.slot = c, slot, slot
	r.aloneSince = time.Time{}
	r.welcomeLocked(c)
	r.host.enqueueControl(wire.RelayOpMemberJoined, slot, wire.EncodeRelayMember(wire.RelayMember{Account: account}))
	report := r.reportLocked(EventJoined, now)
	report.Slot, report.Account = slot, account
	*reports = append(*reports, report)
	return wire.RelayReasonNone
}

// welcomeLocked 告诉刚进房的连接它的槽位与角色；被禁言的账号重连后再告诉它一次禁言状态。
func (r *room) welcomeLocked(c *conn) {
	c.enqueueControl(wire.RelayOpWelcome, c.slot, wire.EncodeRelayWelcome(wire.RelayWelcome{Host: c == r.host, HostSlot: hostSlot}))
	if r.muted[c.account] {
		c.enqueueControl(wire.RelayOpMuted, c.slot, []byte{1})
	}
}

// leave 在连接的读循环结束后调用。已被顶号、踢出或房间已关的连接不再是房间的当前连接，这里什么也不做。
func (r *room) leave(c *conn) {
	var reports []Report
	closed := false
	r.mu.Lock()
	if !r.closed && r.members[c.slot] == c {
		if c == r.host {
			closed = r.hostLostLocked(c.closeReason(), &reports)
		} else {
			r.removeLocked(c, c.closeReason(), &reports)
		}
	}
	r.mu.Unlock()
	r.emit(reports)
	if closed {
		r.server.forgetRoom(r)
	}
}

// hostLostLocked 处理房主连接的结束（断开、超限被断开）。
//
// 这一批：直接关房，断开全部成员（方案 §4.4 原来的做法）。下一批在这里接换 host（§4.6）：进入宽限期、
// 期满后按候选顺序选新房主，只有选不出人时才关房。返回 true 表示房间已关闭。
func (r *room) hostLostLocked(_ wire.RelayReason, reports *[]Report) bool {
	r.closeLocked(wire.RelayReasonRoomClosed, reports)
	return true
}

// removeLocked 把一个成员移出房间并通知房主。
func (r *room) removeLocked(c *conn, reason wire.RelayReason, reports *[]Report) {
	account := c.account
	delete(r.members, c.slot)
	delete(r.slots, account)
	if len(r.members) == 1 {
		r.aloneSince = r.server.now()
	}
	r.host.enqueueControl(wire.RelayOpMemberLeft, c.slot, wire.EncodeRelayMember(wire.RelayMember{Reason: reason, Account: account}))
	report := r.reportLocked(EventLeft, r.server.now())
	report.Slot, report.Account, report.Reason = c.slot, account, reasonName(reason)
	*reports = append(*reports, report)
}

// close 从外部关房（停机、空房超时）。
func (r *room) close(reason wire.RelayReason) {
	var reports []Report
	r.mu.Lock()
	wasOpen := !r.closed
	r.closeLocked(reason, &reports)
	r.mu.Unlock()
	r.emit(reports)
	if wasOpen {
		r.server.forgetRoom(r)
	}
}

func (r *room) closeLocked(reason wire.RelayReason, reports *[]Report) {
	if r.closed {
		return
	}
	report := r.reportLocked(EventClosed, r.server.now())
	report.Members, report.Reason = 0, reasonName(reason)
	*reports = append(*reports, report)
	r.closed = true
	for _, c := range r.members {
		c.finish(reason, true)
	}
	clear(r.members)
	clear(r.slots)
}

// heartbeat 发一条心跳；只剩房主超过 EmptyRoomTimeout 时改为关房。
func (r *room) heartbeat(now time.Time) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if !r.aloneSince.IsZero() && now.Sub(r.aloneSince) >= r.server.config.EmptyRoomTimeout {
		r.mu.Unlock()
		r.close(wire.RelayReasonIdleTimeout)
		return
	}
	report := r.reportLocked(EventHeartbeat, now)
	r.mu.Unlock()
	r.server.report.Report(report)
}

// forwardGame 转发游戏通道的帧。成员的帧只到房主，Seq 改写为成员槽位（成员写的目标不看）；
// 房主的帧按 Seq 的目标转给某个成员或全体成员，Seq 改写为房主槽位。目标不在线的帧丢弃。
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
	if from != r.host {
		r.host.enqueue(data)
		return
	}
	r.sendLocked(from, p.Seq, data)
}

// forwardRoom 转发房间通道的帧：按 Seq 的目标直达某个槽位或全体（不含发送者），房主也是收件人；
// 被禁言的账号的房间帧丢弃。限额在连接的读循环里先扣过。
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

// kick 是房主的踢人控制帧：踢出槽位 slot 上的成员，并记下账号，房间存续期间不让它再进。
func (r *room) kick(from *conn, slot uint32) {
	var reports []Report
	r.mu.Lock()
	if !r.closed && from == r.host && slot != hostSlot {
		if target := r.members[slot]; target != nil {
			r.kicked[target.account] = true
			target.finish(wire.RelayReasonKicked, true)
			r.removeLocked(target, wire.RelayReasonKicked, &reports)
		}
	}
	r.mu.Unlock()
	r.emit(reports)
}

// mute 是房主的禁言控制帧：on 为 true 时槽位 slot 上的账号发不出房间通道的帧，false 解除。被禁言者收到 RelayOpMuted。
func (r *room) mute(from *conn, slot uint32, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || from != r.host || slot == hostSlot {
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

func (r *room) reportLocked(event string, now time.Time) Report {
	return Report{Event: event, Relay: r.server.config.Instance, Room: r.id, Host: r.hostAccount,
		Members: len(r.members), MaxMembers: r.maxMembers, AtUnixMilli: now.UnixMilli()}
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

func reasonName(reason wire.RelayReason) string { return reason.String() }
