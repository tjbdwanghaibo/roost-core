package lobby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// relay ↔ lobby 的 NATS 契约。<subject> 是 lobby.subject（relay 侧配置同一个值）。
//
//	relay → lobby   <subject>.events            RoomEvent（JSON），lobby 用队列组 EventQueue 订阅，多个 lobby 实例只有一个处理
//	lobby → relay   <subject>.relay.<instance>  RelayCommand（JSON），relay 实例订阅自己名字的主题
//
// 一个 subject 对应一份房间目录（一个 lobby.key_prefix）：两个 lobby 服务共用 relay 时，它们必须共用 key_prefix 与
// subject（各自只服务自己的游戏，房间按 game 隔开）；subject 相同而 key_prefix 不同时，事件会被另一个目录的实例
// 收走、找不到房间而丢弃。
const (
	EventQueue = "lobby"

	// EventHeartbeat 每个房间定时发一次（间隔要明显短于 lobby.room_ttl，推荐 room_ttl/3）：给房间续命（再活一个 room TTL），带全量快照。
	EventHeartbeat = "room.heartbeat"
	// EventMemberJoined / EventMemberLeft 在成员进出时立即发（Account 是进出的人），同样带全量快照。
	EventMemberJoined = "room.member_joined"
	EventMemberLeft   = "room.member_left"
	// EventHostChanged 在换 host 完成（新房主回了 host_ready，§4.6.4 第 7 步）时立即发。
	EventHostChanged = "room.host_changed"
	// EventClosed 在房间关闭时发；lobby 把记录标成已关闭，记录随 TTL 消失。
	EventClosed = "room.closed"

	// CommandLeave 让 relay 把 Account 从 Room 里移出（等同于他自己发 leave / 断开）；是房主时 relay 走换 host。
	CommandLeave = "leave"

	// ReasonKicked 是 member_left 报告里“被房主踢出”的原因（relay 的 wire.RelayReasonKicked 的名字）。
	ReasonKicked = "kicked"
	// ReasonLeft 是 member_left 报告里“主动离开”的原因（wire.RelayReasonLeft：lobby 的 leave 或 RelayOpLeave）。
	// 其余原因（disconnected、idle_timeout 等）都是掉线：已开始的房间里掉线的人席位留着。
	ReasonLeft = "left"

	// VerifyQueue 是验权请求的队列组：多个 lobby 实例只有一个答复。
	VerifyQueue = "lobby"
)

// RoomEvent 是 relay 发给 lobby 的房间报告。除 closed 以外，每一条都带房间的全量快照（Online、Host、Epoch），
// lobby 不依赖消息不丢、不乱序：
//   - Seq：同一房间内由 relay 严格递增，从 1 开始。lobby 只用序号更大的报告改写在线名单，迟到的旧报告只续 TTL；
//   - Host / Epoch：relay 的当前房主与 host_epoch（建房时的房主是 epoch 0，每次换 host 加 1）。lobby 按 epoch 做 CAS，
//     只接受更大的 epoch（§4.6.4 第 7 步），与消息类型无关——心跳也带着，丢了一条 host_changed 下一次心跳补上；
//   - Online：此刻连在 relay 上的账号（含房主）。
//
// Relay 必须等于房间记录里的 relay 实例，不等就整条忽略（不是这个房间的 relay 发的）。
type RoomEvent struct {
	Type    string   `json:"type"`
	Room    string   `json:"room"`
	Relay   string   `json:"relay"`
	Seq     uint64   `json:"seq"`
	Online  []string `json:"online,omitempty"`
	Host    string   `json:"host,omitempty"`
	Epoch   uint64   `json:"epoch"`
	Account string   `json:"account,omitempty"`
	// Reason 只在 member_left / closed 上有；lobby 只认 member_left 的两种：ReasonKicked 把 Account 记进被踢名单，
	// ReasonLeft 清掉他的 Connected（主动离开，已开始的房间也不再给他留席位）。
	Reason string `json:"reason,omitempty"`
}

// RelayCommand 是 lobby 发给 relay 的命令。
type RelayCommand struct {
	Type    string `json:"type"`
	Room    string `json:"room"`
	Account string `json:"account"`
}

// EventSubject 是 relay 报告房间事件的主题。
func EventSubject(subject string) string { return subject + ".events" }

// RelayCommandSubject 是发给某个 relay 实例的命令主题。
func RelayCommandSubject(subject, instance string) string { return subject + ".relay." + instance }

// HandleRelayEventData 解码并处理一条 relay 报告（NATS 回调直接调它）。
func (s *Service) HandleRelayEventData(ctx context.Context, data []byte) error {
	var event RoomEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("%w: relay event is not JSON: %v", ErrBadRequest, err)
	}
	return s.HandleRelayEvent(ctx, event)
}

// HandleRelayEvent 把一条 relay 报告写进房间目录（见 RoomEvent 的规则）。房间已经不在、已关闭、或不是这个 relay
// 的报告时返回 nil：这些都是正常的迟到消息。
func (s *Service) HandleRelayEvent(ctx context.Context, event RoomEvent) error {
	switch event.Type {
	case EventHeartbeat, EventMemberJoined, EventMemberLeft, EventHostChanged, EventClosed:
	default:
		return fmt.Errorf("%w: unknown relay event type %q", ErrBadRequest, event.Type)
	}
	if event.Room == "" || event.Relay == "" {
		return fmt.Errorf("%w: relay event without room or relay", ErrBadRequest)
	}
	now := s.cfg.Now()
	var stillOnline []string // 经 lobby 离开、更新的报告里仍在线的人：放锁后再发一次 leave 命令
	_, err := s.cfg.Rooms.Update(ctx, event.Room, func(room *Room) (bool, error) {
		stillOnline = stillOnline[:0]
		if room.gone(now) || room.Relay.Instance != event.Relay {
			return false, nil
		}
		if event.Type == EventClosed {
			room.Closed = true
			return true, nil
		}
		// 被踢：记进被踢名单、让出席位。踢出是永久的事实，与序号无关，迟到也照记。
		if event.Type == EventMemberLeft && event.Reason == ReasonKicked && event.Account != "" && event.Account != room.Host {
			if !room.kicked(event.Account) {
				room.Kicked = append(room.Kicked, event.Account)
			}
			if i := room.memberIndex(event.Account); i >= 0 {
				room.Members = append(room.Members[:i], room.Members[i+1:]...)
			}
		}
		// 房主：按 epoch 做 CAS，只认更大的。
		if event.Epoch > room.HostEpoch && event.Host != "" {
			room.Host, room.HostEpoch = event.Host, event.Epoch
		}
		// 在线名单：只认序号更大的报告。主动离开也只认序号更大的：迟到的旧 left 不能清掉回座后又连上的人。
		if event.Seq > room.RelaySeq {
			room.RelaySeq = event.Seq
			if event.Type == EventMemberLeft && event.Reason == ReasonLeft {
				if i := room.memberIndex(event.Account); i >= 0 {
					room.Members[i].Connected = false
				}
			}
			// relay 确认这个人走了（任何原因）：经 lobby 离开的记录可以清掉。
			if event.Type == EventMemberLeft && event.Account != "" {
				room.clearLeaving(event.Account)
			}
			for _, account := range event.Online {
				if room.leaving(account) {
					stillOnline = append(stillOnline, account)
				}
			}
			room.applyOnline(event.Online, now)
		}
		if room.memberIndex(room.Host) < 0 {
			room.Members = append(room.Members, Member{Account: room.Host})
		}
		// 每一条被接受的报告都续命（只有 relay 的报告续命，见 Room.AliveUntilUnixMilli），也写一次记录。
		room.keepAlive(now, s.cfg.Rooms.TTL())
		return true, nil
	})
	if errors.Is(err, ErrRoomMissing) {
		return nil
	}
	if err == nil {
		for _, account := range stillOnline {
			s.sendRelayCommand(event.Relay, RelayCommand{Type: CommandLeave, Room: event.Room, Account: account})
		}
	}
	return err
}

// applyOnline 按 relay 的在线名单重算席位：在线的记下 Connected、清掉预留；不在线的按 holdsSeat 决定留不留。
// relay 报告在线、目录里却没有的人补上（relay 是“谁连着”的权威）。
func (r *Room) applyOnline(online []string, now time.Time) {
	connected := make(map[string]bool, len(online))
	for _, account := range online {
		if account != "" {
			connected[account] = true
		}
	}
	kept := r.Members[:0]
	for _, member := range r.Members {
		member.Online = connected[member.Account]
		if member.Online {
			member.ReservedUntilUnix = 0
			if !r.leaving(member.Account) { // 经 lobby 离开的人（房主）：迟到的名单不把他重新记成连上过
				member.Connected = true
			}
			delete(connected, member.Account)
		}
		if r.holdsSeat(member, now) {
			kept = append(kept, member)
		}
	}
	r.Members = kept
	for _, account := range online {
		if connected[account] && !r.kicked(account) && !r.leaving(account) {
			r.Members = append(r.Members, Member{Account: account, Online: true, Connected: true})
			delete(connected, account)
		}
	}
}

// relay 每条连接进房前的验权（方案 §4.3）：relay 本地验签通过后经 NATS request 发到 VerifySubject，lobby 按房间
// 目录答复能不能进。只核对能不能进房，不核对角色——换 host 之后目录里的房主可能比 relay 晚一步，角色以 relay 为准。

// VerifySubject 是验权请求的主题（lobby 用队列组 VerifyQueue 订阅）。
func VerifySubject(subject string) string { return subject + ".verify" }

// RelayVerifyRequest 是 relay 发来的验权请求。Create 为 true 表示 relay 上还没有这个房间、要用这条连接建房。
type RelayVerifyRequest struct {
	Room    string `json:"room"`
	Account string `json:"account"`
	Relay   string `json:"relay"`
	Create  bool   `json:"create,omitempty"`
}

// RelayVerifyReply 是验权的答复。OK 为 false 时 Reason 是下面的 Verify* 之一；VerifyUnavailable 表示 lobby 自己
// 出了错（Redis 不可达），relay 按 lobby 不可用处理。
type RelayVerifyReply struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// 验权拒绝的原因（RelayVerifyReply.Reason）。
const (
	VerifyRoomNotFound = "room_not_found" // 房间不存在、已过期或已关闭
	VerifyWrongRelay   = "wrong_relay"    // 房间不分在发请求的 relay 实例上
	VerifyKicked       = "kicked"         // 账号在被踢名单里
	VerifyNotMember    = "not_member"     // 账号不占席位（没 join，或已离开、席位已让出）
	VerifyNotHost      = "not_host"       // 建房的连接不是目录里的房主
	VerifyUnavailable  = "unavailable"    // lobby 读目录出错
	VerifyBadRequest   = "bad_request"    // 请求解不开或缺字段
)

// VerifyRelayConnection 按房间目录核对四件事：房间还在、分在这个 relay 上、账号没被踢、账号占着席位；建房时
// 账号还必须是目录里的房主，且房间还没被 relay 报告过（报告过说明 relay 重启过，不重建）。只读，不写目录。
func (s *Service) VerifyRelayConnection(ctx context.Context, req RelayVerifyRequest) RelayVerifyReply {
	if req.Room == "" || req.Account == "" || req.Relay == "" {
		return RelayVerifyReply{Reason: VerifyBadRequest}
	}
	room, found, err := s.cfg.Rooms.Get(ctx, req.Room)
	now := s.cfg.Now()
	switch {
	case err != nil:
		slog.Warn("lobby: relay verification could not read the room", "room", req.Room, "err", err)
		return RelayVerifyReply{Reason: VerifyUnavailable}
	case !found || room.gone(now):
		return RelayVerifyReply{Reason: VerifyRoomNotFound}
	case room.Relay.Instance != req.Relay:
		return RelayVerifyReply{Reason: VerifyWrongRelay}
	case room.kicked(req.Account):
		return RelayVerifyReply{Reason: VerifyKicked}
	case req.Create && req.Account != room.Host:
		return RelayVerifyReply{Reason: VerifyNotHost}
	case req.Create && room.RelaySeq > 0:
		// relay 已经报告过这个房间，却又要建房：relay 在 room_ttl 内重启过，房间随旧进程丢了。不许在新进程上重建
		// 同一个房间号（维护者 2026-11-17 选 A），房间按“relay 崩溃时房间丢失”处理，目录在 room_ttl 内消失。
		return RelayVerifyReply{Reason: VerifyRoomNotFound}
	}
	room.pruneReservations(now)
	if room.memberIndex(req.Account) < 0 {
		return RelayVerifyReply{Reason: VerifyNotMember}
	}
	return RelayVerifyReply{OK: true}
}

// HandleRelayVerifyData 解码一条验权请求并返回编码好的答复（NATS 回调直接调它）。
func (s *Service) HandleRelayVerifyData(ctx context.Context, data []byte) []byte {
	var req RelayVerifyRequest
	reply := RelayVerifyReply{Reason: VerifyBadRequest}
	if err := json.Unmarshal(data, &req); err == nil {
		reply = s.VerifyRelayConnection(ctx, req)
	}
	out, _ := json.Marshal(reply)
	return out
}
