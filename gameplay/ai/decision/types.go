// Package decision 是可重放的纯决策内核。Program 可跨实体并发共享；State 由单个宿主
// 版本化保存。Step 只计算新快照和有序意图；宿主负责把两者原子提交，之后才执行副作用。
// 内核不取系统时间、随机数，不调用业务回调，不调度 goroutine。
package decision

const Schema = "roost.ai.decision/v1"

// 限额同时约束原生 JSON 与直接构造的 Go 树。总项数含阶段、监听、节点和动作。
const (
	MaxJSONBytes         = 1 << 20
	MaxItems             = 4096
	MaxDepth             = 32 // 从根节点（深度 1）起算
	MaxStages            = 64
	MaxListenersPerStage = 256
	MaxRegistryNames     = 256 // 每个独立命名空间
	MaxNameBytes         = 64
)

// 名称由 ASCII 字母、数字、下划线、点、短横线或冒号组成，以字母、数字或下划线开头。
type Definition struct {
	Schema string  `json:"schema"`
	Name   string  `json:"name"`
	Start  string  `json:"start"`
	Stages []Stage `json:"stages"`
}
type Stage struct {
	Name   string     `json:"name"`
	Tick   []Node     `json:"tick,omitempty"`
	Events []Listener `json:"events,omitempty"`
}
type Listener struct {
	ID      int64  `json:"id"`
	Event   string `json:"event"`
	Enabled bool   `json:"enabled,omitempty"`
	Once    bool   `json:"once,omitempty"`
	Filter  *Test  `json:"filter,omitempty"`
	Body    []Node `json:"body,omitempty"`
}
type Test struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value int64  `json:"value"`
}
type Node struct {
	Kind     string   `json:"kind"`
	Test     *Test    `json:"test,omitempty"`
	Children []Node   `json:"children,omitempty"`
	Actions  []Action `json:"actions,omitempty"`
}
type Action struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name,omitempty"`
	Arg   int64  `json:"arg,omitempty"`
	Delay int64  `json:"delay,omitempty"`
	Limit int64  `json:"limit,omitempty"`
}
type Registry struct {
	Events  []string `json:"events"`
	Fields  []string `json:"fields"`
	Intents []string `json:"intents"`
}

// Event 为空表示 Tick；Now 是调用者提供的非负、单调不减逻辑 tick。相等时仍是两次输入。
type Input struct {
	Now    int64            `json:"now"`
	Event  string           `json:"event,omitempty"`
	Fields map[string]int64 `json:"fields,omitempty"`
}
type Intent struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Arg    int64  `json:"arg"`
}

// State 是宿主快照而非持久化服务。Plan 是定义（含顺序）和注册表的 SHA-256；
// Active 按本阶段监听顺序，Counts 按整个定义中有上限的 emit 的编译遍历顺序排列。
// LastTick=-1 表示尚无 Tick；晚到的事件只推进 Now。恢复 JSON 后必须再 ValidateState。
type State struct {
	Plan     string    `json:"plan"`
	Stage    string    `json:"stage"`
	Now      int64     `json:"now"`
	LastTick int64     `json:"last_tick"`
	Active   []bool    `json:"active,omitempty"`
	Pending  []Pending `json:"pending,omitempty"`
	Counts   []int64   `json:"counts,omitempty"`
}

// Pending 只存身份和逻辑期限，不能由快照注入意图或载荷；切换阶段不清除该队列。
type Pending struct {
	NodeID int64 `json:"node_id"`
	Due    int64 `json:"due"`
}
