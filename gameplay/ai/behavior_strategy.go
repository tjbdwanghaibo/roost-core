package ai

import (
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// ActionEnd 是一次 taskflow 动作结束，在它发生之后的那一拍交给树。
type ActionEnd struct {
	ID     int64
	Kind   coreflow.ActionKind
	Reason coreflow.ActionReason
}

// BehaviorContext 是树作为策略运行时节点收到的 tick 上下文：核心 AI 上下文（拥有者实体、动作列表）、业务数据块与
// 确定性的 tick 时钟。
type BehaviorContext[C any] struct {
	Core    *Context
	Data    *C
	NowTick int64
	// actionEnds 保存自上一拍以来送达的动作结束；TaskflowAction 叶子按 ID 取走。
	actionEnds []ActionEnd
}

// TakeActionEnd 取走一个动作 ID 的待处理结束。
func (b *BehaviorContext[C]) TakeActionEnd(id int64) (ActionEnd, bool) {
	for index, end := range b.actionEnds {
		if end.ID == id {
			b.actionEnds = append(b.actionEnds[:index], b.actionEnds[index+1:]...)
			return end, true
		}
	}
	return ActionEnd{}, false
}

// NowTicks 把上下文的 tick 时钟适配成节点库 Now 的签名（Cooldown / TimeLimit）。
func NowTicks[C any](ctx *BehaviorContext[C]) int64 { return ctx.NowTick }

// BehaviorStrategyOptions 配置树到策略的桥接。
type BehaviorStrategyOptions[C any] struct {
	Name string
	Data *C
	// NowTick 提供树看到的确定性 tick 时钟。树里有基于时间的节点时必填；为 nil 时读作 0。
	NowTick func() int64
	// CanStop 回答 Strategy.CanStopByNext；nil 表示总是允许替换。
	CanStop func(next Strategy) bool
	// OnResult 观察每次完成的树求值（Success / Failure）。
	OnResult func(Status)
}

// BehaviorStrategy 把行为树作为 Strategy 运行，连接树骨架与 Controller / taskflow 执行流程：Tick 驱动树，动作 /
// 任务结束先缓冲、交给下一拍的上下文，树完成后重置，下一拍从根重新求值。
type BehaviorStrategy[C any] struct {
	root    Node[BehaviorContext[C]]
	options BehaviorStrategyOptions[C]
	pending []ActionEnd
}

// NewBehaviorStrategy 用根节点与选项构造策略；Name 为空时取 "behavior_tree"。
func NewBehaviorStrategy[C any](root Node[BehaviorContext[C]], options BehaviorStrategyOptions[C]) *BehaviorStrategy[C] {
	if options.Name == "" {
		options.Name = "behavior_tree"
	}
	return &BehaviorStrategy[C]{root: root, options: options}
}

func (s *BehaviorStrategy[C]) Name() string { return s.options.Name }

func (s *BehaviorStrategy[C]) Init(*Context) error {
	if s.root != nil {
		s.root.Reset()
	}
	s.pending = nil
	return nil
}

func (s *BehaviorStrategy[C]) Tick(ctx *Context, _ time.Time) {
	if s.root == nil {
		// 没有树消费这些结束；丢掉它们，否则会无限累积。
		s.pending = nil
		return
	}
	behavior := BehaviorContext[C]{Core: ctx, Data: s.options.Data, actionEnds: s.pending}
	s.pending = nil
	if s.options.NowTick != nil {
		behavior.NowTick = s.options.NowTick()
	}
	status := s.root.Tick(&behavior)
	if status == StatusSuccess || status == StatusFailure {
		s.root.Reset()
		if s.options.OnResult != nil {
			s.options.OnResult(status)
		}
	}
}

func (s *BehaviorStrategy[C]) OnActionEnd(_ *Context, actionID int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	s.pending = append(s.pending, ActionEnd{ID: actionID, Kind: kind, Reason: reason})
}

func (s *BehaviorStrategy[C]) OnMissionEnd(*Context, coreflow.Mission, coreflow.ActionReason) {
}

func (s *BehaviorStrategy[C]) CanStopByNext(next Strategy) bool {
	if s.options.CanStop != nil {
		return s.options.CanStop(next)
	}
	return true
}

// Stop 重置树。结束在途动作仍由 Controller 的 EndActions 钩子负责，它在策略替换时已经运行。
func (s *BehaviorStrategy[C]) Stop(*Context, string) {
	if s.root != nil {
		s.root.Reset()
	}
	s.pending = nil
}

// TaskflowAction 是闭合树 / taskflow 循环的叶子：树决策，taskflow 执行。第一次 tick 时经注入的 Launch 发起一个
// 动作（CreateAction 或 EnqueueAction——由业务选择并返回动作 ID），之后保持 Running，直到对应的 ActionEnd 到达，
// 再经 Succeeded 把结束原因映射成结果。
//
// 只按动作 ID 匹配 OnActionEnd 送来的结束。任务（SetMission）不能用这个叶子等待：SetMission
// 不返回 ID，任务结束走 OnMissionEnd，BehaviorStrategy 不把它交给叶子，而且任务 ID 与动作
// ID 是两套计数、会撞号；等任务请用读 MissionManager.InMission 的 Condition。
//
// Launch 可以在 actionflow.ActionRunner 的回调里被调用（例如业务在 OnEnded 里立刻再
// Tick 一次 AI）：runner 把回调里的变更延后到回调返回后执行，Start / Enqueue 返回的
// 已分配 ID 与 nil 照常可用，延后执行时无法启动也会以该 ID 发 OnEnded，叶子不会挂住。
//
// 被打断的分支（运行中被 Reset）不会自行取消底层动作——Reset 没有上下文。打断必须结束动作时，提供 OnInterrupt
// （一个持有实体 ActionList 的闭包）。
type TaskflowAction[C any] struct {
	Launch func(ctx *BehaviorContext[C]) (int64, error)
	// Succeeded 把结束原因映射成树的结果；nil 时每次结束都算成功。
	Succeeded func(reason coreflow.ActionReason) bool
	// OnInterrupt 在动作仍在途、分支被重置时运行（可选）。
	OnInterrupt func(actionID int64)

	actionID int64
	launched bool
}

func (n *TaskflowAction[C]) Tick(ctx *BehaviorContext[C]) Status {
	if n == nil || n.Launch == nil {
		return StatusFailure
	}
	if !n.launched {
		id, err := n.Launch(ctx)
		if err != nil {
			return StatusFailure
		}
		n.actionID, n.launched = id, true
		return StatusRunning
	}
	end, done := ctx.TakeActionEnd(n.actionID)
	if !done {
		return StatusRunning
	}
	n.launched = false
	if n.Succeeded == nil || n.Succeeded(end.Reason) {
		return StatusSuccess
	}
	return StatusFailure
}

func (n *TaskflowAction[C]) Reset() {
	if n == nil {
		return
	}
	if n.launched && n.OnInterrupt != nil {
		n.OnInterrupt(n.actionID)
	}
	n.launched = false
}
