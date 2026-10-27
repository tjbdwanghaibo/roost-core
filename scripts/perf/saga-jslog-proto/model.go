// Package jslogproto 是“Saga 协调器以 JetStream 为权威日志”的原型（设计稿
// docs/maintenance/PROPOSAL-2026-10-27-saga-jetstream-log.md）。只用于语义验证与压测，不进生产路径：
// 不改 framework/saga 的 Store / 引擎 / wiring。
//
// 形状：saga 按 ID hash 到 P 个分区；协调器推进一次状态 = 往分区 subject 发一条事件（新状态 + 要派发的命令），
// 发布带“分区事件过滤的期望序号”做单写者；参与方按 subject 过滤取命令，在 Nest 事务里改实体并写操作回执，
// WAL 落盘后把结果直接发回分区；协调器状态按分区放内存，从 KV 快照 + 之后的消息重放。
package jslogproto

import (
	"fmt"
	"strings"
)

// WireVersion 是事件 / 命令 / 结果的线上格式版本；不认识的版本是确定性错误。
const WireVersion = 1

type Status string

const (
	StatusRunning      Status = "running"
	StatusCompensating Status = "compensating"
	StatusCompleted    Status = "completed"
	StatusCompensated  Status = "compensated"
	StatusFailed       Status = "failed"
	StatusManual       Status = "manual_required"
)

func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusCompensated || s == StatusFailed || s == StatusManual
}

type Phase string

const (
	PhaseForward    Phase = "f"
	PhaseCompensate Phase = "c"
)

// 操作关闭方式（tombstone）：带结果关闭 / 放弃关闭。放弃关闭之后到达的正向成功要补偿（现行 saga 契约第 4 条）。
const (
	ClosedWithResult = "result"
	ClosedAbandoned  = "abandoned"
)

// TransferData 是原型唯一的业务：两步转账（扣 From / 加 To），RefuseCredit 让第二步被业务拒绝（补偿场景）。
type TransferData struct {
	From         int64 `json:"from"`
	To           int64 `json:"to"`
	Amount       int64 `json:"amount"`
	RefuseCredit bool  `json:"refuse_credit,omitempty"`
}

// Saga 是一笔 saga 的完整状态；每条事件携带它的新版本，重放 = 按序覆盖。
type Saga struct {
	ID         string       `json:"id"`
	Part       int          `json:"part"`
	Status     Status       `json:"status"`
	Data       TransferData `json:"data"`
	Step       int          `json:"step"`
	Phase      Phase        `json:"phase"`
	Attempt    int          `json:"attempt"` // 当前操作已派发的尝试数
	Waiting    bool         `json:"waiting"` // 有一次尝试在等结果
	CmdID      string       `json:"cmd_id,omitempty"`
	DeadlineAt int64        `json:"deadline_at,omitempty"` // 当前尝试截止（unix ms）
	NextRunAt  int64        `json:"next_run_at,omitempty"` // 退避后的下一次派发（unix ms）
	// Completed 是已生效、尚未补偿的正向步骤（按生效顺序）；补偿从末尾取。
	Completed []int `json:"completed,omitempty"`
	// Closed 是操作 tombstone：opKey → ClosedWithResult / ClosedAbandoned。
	Closed    map[string]string `json:"closed,omitempty"`
	Version   uint64            `json:"version"`
	CreatedAt int64             `json:"created_at"`
	UpdatedAt int64             `json:"updated_at"`
	Reopened  int               `json:"reopened,omitempty"`
	LateAlarm int               `json:"late_alarm,omitempty"` // 补偿方向放弃后迟到的成功（只告警）
}

func (s *Saga) clone() *Saga {
	c := *s
	c.Completed = append([]int(nil), s.Completed...)
	if s.Closed != nil {
		c.Closed = make(map[string]string, len(s.Closed))
		for k, v := range s.Closed {
			c.Closed[k] = v
		}
	}
	return &c
}

// OpKey 是操作实例（saga + 方向 + 步骤），对应现行 Command.IdempotencyKey；参与方按它记回执，跨尝试至多生效一次。
func OpKey(sagaID string, phase Phase, step int) string {
	return fmt.Sprintf("%s:%s:%d", sagaID, phase, step)
}

// CmdID 是一次尝试的身份（操作 + 尝试号）。
func CmdID(op string, attempt int) string { return fmt.Sprintf("%s:%d", op, attempt) }

// Command 是派发给参与方的一次尝试，嵌在事件里（与状态同一条消息，天然原子）。
type Command struct {
	SagaID     string `json:"saga_id"`
	Part       int    `json:"part"`
	Op         string `json:"op"`
	CmdID      string `json:"cmd_id"`
	Phase      Phase  `json:"phase"`
	Step       int    `json:"step"`
	Attempt    int    `json:"attempt"`
	Action     string `json:"action"` // debit / credit
	Account    int64  `json:"account"`
	Amount     int64  `json:"amount"`
	Refuse     bool   `json:"refuse,omitempty"`
	DeadlineAt int64  `json:"deadline_at"`
}

// Completion 是参与方发回协调器分区的结果。CmdID 是生效那次尝试的（回放时原样带回）。
type Completion struct {
	V         int    `json:"v"`
	SagaID    string `json:"saga_id"`
	Op        string `json:"op"`
	CmdID     string `json:"cmd_id"`
	Phase     Phase  `json:"phase"`
	Step      int    `json:"step"`
	Success   bool   `json:"success"`
	Retryable bool   `json:"retryable,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// StartRequest 是发起方写到分区 in.start 的启动意图（MsgID = saga ID，流内去重）。
type StartRequest struct {
	V    int          `json:"v"`
	ID   string       `json:"id"`
	Data TransferData `json:"data"`
}

// Event 是分区日志里协调器写的一条事件：一笔 saga 的新状态 + 可选命令。In 是写这批事件时已处理到的最大输入序号
// （分区内输入按序处理，重放后只处理 In 之后的输入）。
type Event struct {
	V      int      `json:"v"`
	Part   int      `json:"part"`
	Writer string   `json:"writer"`
	In     uint64   `json:"in"`
	At     int64    `json:"at"`
	Kind   string   `json:"kind"`
	Saga   *Saga    `json:"saga"`
	Cmd    *Command `json:"cmd,omitempty"`
}

// Layout 固定 subject / 流 / KV 的命名。一条流装全部分区：
//
//	<prefix>.<k>.ev.<dest>   协调器事件（dest = 命令目标 acct；done = 终态；none = 无命令）
//	<prefix>.<k>.in.start    启动意图
//	<prefix>.<k>.in.res      参与方结果
//
// 单写者检查的过滤是 <prefix>.<k>.ev.>；参与方的消费者过滤 <prefix>.*.ev.acct。
type Layout struct {
	Prefix     string
	Partitions int
	// StreamGroups 是分区分到几条流（各自的 Raft 组与 leader，按 k % G）；subject 不变。
	// 0 = 缺省 4 条（维护者 2026-10-27 决定 3A），分区不足 4 个时取分区数；1 = 单流（保留为可选）。
	StreamGroups int
}

// DefaultStreamGroups 是缺省的流数。
const DefaultStreamGroups = 4

func (l Layout) groups() int {
	g := l.StreamGroups
	if g == 0 {
		g = DefaultStreamGroups
	}
	if g > l.Partitions {
		g = l.Partitions
	}
	if g < 1 {
		g = 1
	}
	return g
}

// StreamOf 是分区 k 所在的流。
func (l Layout) StreamOf(k int) string {
	if l.groups() == 1 {
		return l.Stream()
	}
	return fmt.Sprintf("%s_G%d", l.Stream(), k%l.groups())
}

// Streams 列出全部流。
func (l Layout) Streams() []string {
	var out []string
	for g := 0; g < l.groups(); g++ {
		out = append(out, l.StreamOf(g))
	}
	return out
}

func (l Layout) streamSubjects(g int) []string {
	if l.groups() == 1 {
		return []string{l.Prefix + ".>"}
	}
	var out []string
	for k := g; k < l.Partitions; k += l.groups() {
		out = append(out, l.PartFilter(k))
	}
	return out
}

func (l Layout) Stream() string         { return "SAGAJS_" + strings.ToUpper(l.Prefix) }
func (l Layout) SnapshotBucket() string { return "SAGAJS_" + strings.ToUpper(l.Prefix) + "_SNAP" }
func (l Layout) OwnerBucket() string    { return "SAGAJS_" + strings.ToUpper(l.Prefix) + "_OWN" }
func (l Layout) EvSubject(k int, dest string) string {
	return fmt.Sprintf("%s.%d.ev.%s", l.Prefix, k, dest)
}
func (l Layout) EvFilter(k int) string      { return fmt.Sprintf("%s.%d.ev.>", l.Prefix, k) }
func (l Layout) InFilter(k int) string      { return fmt.Sprintf("%s.%d.in.>", l.Prefix, k) }
func (l Layout) PartFilter(k int) string    { return fmt.Sprintf("%s.%d.>", l.Prefix, k) }
func (l Layout) StartSubject(k int) string  { return fmt.Sprintf("%s.%d.in.start", l.Prefix, k) }
func (l Layout) ResultSubject(k int) string { return fmt.Sprintf("%s.%d.in.res", l.Prefix, k) }
func (l Layout) DestFilter(dest string) string {
	return fmt.Sprintf("%s.*.ev.%s", l.Prefix, dest)
}
func (l Layout) SnapshotKey(k int) string { return fmt.Sprintf("p%d", k) }

// PartitionOf 把 saga ID 映射到分区（FNV-1a）。
func (l Layout) PartitionOf(id string) int {
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return int(h % uint32(l.Partitions))
}
