package relay

// 向 lobby 的报告（方案 §4.4“向 lobby 报告”；契约与 service/lobby 的 RoomEvent 一致）：每个房间每 HeartbeatInterval
// 一条心跳，房间建成（房主连上）时立即一条心跳，成员进出、房间关闭时立即一条。relay 只管发，经 NATS 送到哪个主题由
// 接线决定（wiring/relay：JSON，主题 <relay.subject>.events）。
//
// 每一条（closed 除外）都带房间的全量快照：Online、Host、Epoch。Seq 在同一房间内严格递增、从 1 开始，lobby 只用
// 序号更大的报告改写在线名单，所以丢一条、乱序都不要紧，下一次心跳补齐。报告是尽力而为的：心跳一直丢，lobby 的房间
// 记录按 TTL 过期、房间从列表里消失，relay 上的房间不受影响。

// 报告类型（Report.Type），与 service/lobby 的 Event* 同值。
const (
	EventHeartbeat    = "room.heartbeat"
	EventMemberJoined = "room.member_joined" // 同一账号顶号不报（在线名单没变）
	EventMemberLeft   = "room.member_left"
	EventHostChanged  = "room.host_changed" // 换 host（下一批）
	EventClosed       = "room.closed"       // 之后不再有这个房间的报告
)

// Report 是一条房间报告。JSON 字段名是与 lobby 的约定（service/lobby.RoomEvent）；Slot、Reason 是附加字段，
// lobby 不读，留给日志与排查。
type Report struct {
	Type  string `json:"type"`
	Room  string `json:"room"`
	Relay string `json:"relay"` // 本实例名（Config.Instance），与令牌里的实例名相同
	Seq   uint64 `json:"seq"`
	// Online 是此刻在线的账号（含房主），按槽位排列。
	Online []string `json:"online,omitempty"`
	// Host 是当前房主的账号；Epoch 是房主代次（建房时为 0，换 host 每次加 1，lobby 只接受更大的值）。
	Host  string `json:"host,omitempty"`
	Epoch uint64 `json:"epoch"`
	// Account / Slot 只在 member_joined / member_left 上有：进出的是谁、哪个槽位。
	Account string `json:"account,omitempty"`
	Slot    uint32 `json:"slot,omitempty"`
	// Reason 只在 member_left / closed 上有：disconnected、kicked、left、slow_consumer、room_closed、idle_timeout 等
	// （wire.RelayReason 的名字）。
	Reason string `json:"reason,omitempty"`
}

// Reporter 接收房间报告。Report 在 relay 不持锁时调用，但仍在连接与房间的处理路径上，必须快速返回（NATS 的
// PublishOnce 只进本地缓冲，满足这一点）。同一房间的报告可能从不同 goroutine 并发调用，先后以 Seq 为准。
type Reporter interface {
	Report(Report)
}

// ReporterFunc 把函数当 Reporter 用。
type ReporterFunc func(Report)

func (f ReporterFunc) Report(report Report) { f(report) }
