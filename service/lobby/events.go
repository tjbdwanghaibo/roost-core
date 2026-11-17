package lobby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// EventHeartbeat 每个房间定时发一次（间隔要明显短于 lobby.room_ttl，推荐 room_ttl/3）：续房间记录的 TTL，带全量快照。
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
	_, err := s.cfg.Rooms.Update(ctx, event.Room, func(room *Room) (bool, error) {
		if room.Closed || room.Relay.Instance != event.Relay {
			return false, nil
		}
		if event.Type == EventClosed {
			room.Closed = true
			return true, nil
		}
		// 房主：按 epoch 做 CAS，只认更大的。
		if event.Epoch > room.HostEpoch && event.Host != "" {
			room.Host, room.HostEpoch = event.Host, event.Epoch
		}
		// 在线名单：只认序号更大的报告。
		if event.Seq > room.RelaySeq {
			room.RelaySeq = event.Seq
			room.applyOnline(event.Online, now)
		}
		if room.memberIndex(room.Host) < 0 {
			room.Members = append(room.Members, Member{Account: room.Host})
		}
		// 每一条报告都写：写入就是续 TTL。
		return true, nil
	})
	if errors.Is(err, ErrRoomMissing) {
		return nil
	}
	return err
}

// applyOnline 按 relay 的在线名单重算席位：在线的留下并清掉预留；不在线的，房主与预留期内的留下，其余让出。
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
			delete(connected, member.Account)
		}
		if member.Online || member.Account == r.Host || member.ReservedUntilUnix > now.Unix() {
			kept = append(kept, member)
		}
	}
	r.Members = kept
	for _, account := range online {
		if connected[account] {
			r.Members = append(r.Members, Member{Account: account, Online: true})
			delete(connected, account)
		}
	}
}
