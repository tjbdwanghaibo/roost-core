package relay

// 向 lobby 的报告（方案 §4.4“向 lobby 报告”）：每个房间每 HeartbeatInterval 一条心跳，成员进出、房间建成与关闭时立即一条。
// relay 只管发，经 NATS 送到哪个主题、怎么编码由接线决定（wiring/relay：JSON，主题 relay.report_subject）。
// 报告是尽力而为的：丢一条心跳，lobby 那边的房间记录按 TTL 续不上，房间从列表里消失，relay 上的房间不受影响。

// 报告的事件名（Report.Event）。
const (
	EventOpened    = "opened"    // 房主连上，房间建成
	EventJoined    = "joined"    // 成员上线（同一账号顶号不报）
	EventLeft      = "left"      // 成员下线（Reason 说明原因）
	EventClosed    = "closed"    // 房间关闭（Reason 说明原因），之后不再有这个房间的报告
	EventHeartbeat = "heartbeat" // 定时心跳，续 lobby 房间记录的 TTL
)

// Report 是一条房间报告。JSON 字段名是与 lobby 的约定。
type Report struct {
	Event string `json:"event"`
	// Relay 是本实例名（Config.Instance），Room 是房间号，与令牌里的两段相同。
	Relay string `json:"relay"`
	Room  string `json:"room"`
	// Host 是当前房主的账号；HostEpoch 是房主代次（这一批恒为 0，换 host 每次加 1，lobby 只接受更大的值）。
	Host      string `json:"host"`
	HostEpoch uint64 `json:"host_epoch"`
	// Members 是在线人数（含房主），MaxMembers 是房间上限（建房的 host 令牌里的值）。
	Members    int `json:"members"`
	MaxMembers int `json:"max_members"`
	// Slot / Account 只在 joined / left 上有：进出的是哪个槽位、哪个账号。
	Slot    uint32 `json:"slot,omitempty"`
	Account string `json:"account,omitempty"`
	// Reason 只在 left / closed 上有：disconnected、kicked、slow_consumer、room_closed、idle_timeout、shutdown 等。
	Reason string `json:"reason,omitempty"`
	// AtUnixMilli 是 relay 生成这条报告的时间。
	AtUnixMilli int64 `json:"at_unix_ms"`
}

// Reporter 接收房间报告。Report 在 relay 不持锁时调用，但仍在连接与房间的处理路径上，必须快速返回（NATS 的
// PublishOnce 只进本地缓冲，满足这一点）。
type Reporter interface {
	Report(Report)
}

// ReporterFunc 把函数当 Reporter 用。
type ReporterFunc func(Report)

func (f ReporterFunc) Report(report Report) { f(report) }
