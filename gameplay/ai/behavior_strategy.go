package ai

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// ErrStageUnknown：分阶段策略里没有这个阶段。
var ErrStageUnknown = errors.New("ai: unknown stage")

// ActionEnd 是一次 actionflow 动作结束，在它发生之后的那一拍交给树。
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
	// events 保存自上一拍以来投递的事件；本拍没被取走的在拍结束时丢弃。
	events   []Event
	strategy behaviorHost
}

// behaviorHost 是 BehaviorContext 回到所属策略的入口（切阶段、轨迹），避免上下文携带策略的类型参数之外的东西。
type behaviorHost interface {
	requestStage(name string) error
	trace(path string, status Status)
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

// HasEvent 报告本拍是否有这一类事件（不取走）。
func (b *BehaviorContext[C]) HasEvent(kind uint16) bool {
	return slices.ContainsFunc(b.events, func(ev Event) bool { return ev.Kind == kind })
}

// TakeEvent 取走本拍最早的一个这一类事件。
func (b *BehaviorContext[C]) TakeEvent(kind uint16) (Event, bool) {
	for index, ev := range b.events {
		if ev.Kind == kind {
			b.events = slices.Delete(b.events, index, index+1)
			return ev, true
		}
	}
	return Event{}, false
}

// Events 返回本拍尚未取走的事件（副本，按投递顺序）。
func (b *BehaviorContext[C]) Events() []Event { return slices.Clone(b.events) }

// SetStage 请求切到另一个阶段：在这一拍的树求值结束后生效（当前树先 Reset，配了 OnInterrupt 的叶子收尾），
// 同一拍里多次请求以最后一次为准。阶段不存在时返回 ErrStageUnknown。
func (b *BehaviorContext[C]) SetStage(name string) error {
	if b.strategy == nil {
		return ErrStageUnknown
	}
	return b.strategy.requestStage(name)
}

func (b *BehaviorContext[C]) trace(path string, status Status) {
	if b.strategy != nil {
		b.strategy.trace(path, status)
	}
}

// DefaultMaxEvents 是 BehaviorStrategy 两拍之间缓冲的事件上限。
const DefaultMaxEvents = 64

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
	// MaxEvents 是两拍之间缓冲的事件上限，超出时丢弃最早的；0 取 DefaultMaxEvents。
	MaxEvents int
	// OnStage 在阶段切换生效后调用（from 是旧阶段名）。
	OnStage func(from, to string)
	// Trace 是决策轨迹钩子：带轨迹的节点（Traced，或注册表开了 TraceNodes 时 ParseTree 包上的每个节点）每次
	// Tick 后以节点路径与结果调用它，供调试命令或日志按实体采样。框架不在内存里留轨迹。
	Trace func(path string, status Status)
}

// BehaviorStrategy 把行为树作为 Strategy 运行，连接树骨架与 Controller / actionflow 执行流程：Tick 驱动当前阶段的树，
// 动作结束与事件先缓冲、交给下一拍的上下文，树完成后重置，下一拍从根重新求值。
//
// 阶段（对照 ssr ai_engine 的 stage）：一个策略可以有若干命名的树（NewStagedStrategy），同一时刻只有当前阶段
// 的树在跑；树里的 SetStage（或 JSON 的 set_stage 叶子）在本拍求值结束后切换，策略外调用 SetStage 立即切换。切换时
// Reset 旧树（运行中的 TaskflowAction 叶子经 OnInterrupt 收尾）。NewBehaviorStrategy 是只有一个阶段的特例。
type BehaviorStrategy[C any] struct {
	stages  map[string]Node[BehaviorContext[C]]
	start   string
	current string
	// requested / hasRequest 是请求的下一个阶段；ticking 为真时 SetStage 只记请求，本拍结束后生效。
	requested  string
	hasRequest bool
	ticking    bool
	options    BehaviorStrategyOptions[C]
	pending    []ActionEnd
	events     []Event
}

// NewBehaviorStrategy 用根节点与选项构造策略；Name 为空时取 "behavior_tree"。
func NewBehaviorStrategy[C any](root Node[BehaviorContext[C]], options BehaviorStrategyOptions[C]) *BehaviorStrategy[C] {
	s, _ := NewStagedStrategy(map[string]Node[BehaviorContext[C]]{"": root}, "", options)
	return s
}

// NewStagedStrategy 用若干命名的树构造分阶段策略，从 start 开始；stages 为空或 start 不在其中时返回 ErrStageUnknown。
func NewStagedStrategy[C any](stages map[string]Node[BehaviorContext[C]], start string, options BehaviorStrategyOptions[C]) (*BehaviorStrategy[C], error) {
	if _, ok := stages[start]; !ok {
		return nil, fmt.Errorf("%w: start stage %q", ErrStageUnknown, start)
	}
	if options.Name == "" {
		options.Name = "behavior_tree"
	}
	if options.MaxEvents <= 0 {
		options.MaxEvents = DefaultMaxEvents
	}
	return &BehaviorStrategy[C]{stages: maps.Clone(stages), start: start, current: start, options: options}, nil
}

func (s *BehaviorStrategy[C]) Name() string { return s.options.Name }

// Stage 返回当前阶段名（单树策略是空串）。
func (s *BehaviorStrategy[C]) Stage() string { return s.current }

// SetStage 切换阶段：在这个策略的树求值期间调用时记为请求、本拍结束后生效；否则立即生效。阶段不存在时返回
// ErrStageUnknown；切到当前阶段什么都不做。
func (s *BehaviorStrategy[C]) SetStage(name string) error {
	if err := s.requestStage(name); err != nil {
		return err
	}
	if !s.ticking {
		s.applyStage()
	}
	return nil
}

func (s *BehaviorStrategy[C]) requestStage(name string) error {
	if _, ok := s.stages[name]; !ok {
		return fmt.Errorf("%w: %q", ErrStageUnknown, name)
	}
	s.requested, s.hasRequest = name, true
	return nil
}

// applyStage 让请求的阶段生效：Reset 旧树、换当前阶段。
func (s *BehaviorStrategy[C]) applyStage() {
	next := s.requested
	s.requested, s.hasRequest = "", false
	if next == s.current {
		return
	}
	if root := s.stages[s.current]; root != nil {
		root.Reset()
	}
	from := s.current
	s.current = next
	if s.options.OnStage != nil {
		s.options.OnStage(from, next)
	}
}

func (s *BehaviorStrategy[C]) Init(*Context) error {
	for _, root := range s.stages {
		if root != nil {
			root.Reset()
		}
	}
	s.current, s.requested, s.hasRequest = s.start, "", false
	s.pending, s.events = nil, nil
	return nil
}

func (s *BehaviorStrategy[C]) Tick(ctx *Context, _ time.Time) {
	root := s.stages[s.current]
	if root == nil {
		// 没有树消费这些结束与事件；丢掉它们，否则会无限累积。
		s.pending, s.events = nil, nil
		return
	}
	behavior := BehaviorContext[C]{Core: ctx, Data: s.options.Data, actionEnds: s.pending, events: s.events, strategy: s}
	s.pending, s.events = nil, nil
	if s.options.NowTick != nil {
		behavior.NowTick = s.options.NowTick()
	}
	s.ticking = true
	status := root.Tick(&behavior)
	s.ticking = false
	if status == StatusSuccess || status == StatusFailure {
		root.Reset()
		if s.options.OnResult != nil {
			s.options.OnResult(status)
		}
	}
	if s.hasRequest {
		s.applyStage()
	}
}

func (s *BehaviorStrategy[C]) OnActionEnd(_ *Context, actionID int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	s.pending = append(s.pending, ActionEnd{ID: actionID, Kind: kind, Reason: reason})
}

func (s *BehaviorStrategy[C]) OnMissionEnd(*Context, coreflow.Mission, coreflow.ActionReason) {
}

// OnEvent 缓冲一个事件，交给下一拍的上下文；超过 MaxEvents 时丢弃最早的。
func (s *BehaviorStrategy[C]) OnEvent(_ *Context, ev Event) {
	if len(s.events) >= s.options.MaxEvents {
		s.events = slices.Delete(s.events, 0, len(s.events)-s.options.MaxEvents+1)
	}
	s.events = append(s.events, ev)
}

func (s *BehaviorStrategy[C]) CanStopByNext(next Strategy) bool {
	if s.options.CanStop != nil {
		return s.options.CanStop(next)
	}
	return true
}

// Stop 重置当前阶段的树。结束在途动作仍由 Controller 的 EndActions 钩子负责，它在策略替换时已经运行。
func (s *BehaviorStrategy[C]) Stop(*Context, string) {
	if root := s.stages[s.current]; root != nil {
		root.Reset()
	}
	s.pending, s.events, s.requested, s.hasRequest = nil, nil, "", false
}

func (s *BehaviorStrategy[C]) trace(path string, status Status) {
	if s.options.Trace != nil {
		s.options.Trace(path, status)
	}
}

// TaskflowAction 是闭合树 / actionflow 循环的叶子：树决策，actionflow 执行（类型名沿用旧包名）。第一次 tick 时经注入的 Launch 发起一个
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
