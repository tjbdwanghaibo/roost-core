package ai

// 节点库。每个节点都遵守 Sequence / Selector 确立的 tick 契约：Tick 推进一步求值，Reset 把子树恢复到初始状态（完成后、
// 以及更高优先级的分支打断正在运行的分支时都会用到）。基于时间的节点读注入的 tick 而不是墙钟，权威侧的 AI 决策可以
// 回放；需要随机性的节点出于同样原因读注入的 roll 函数。

// ParallelPolicy 决定 Parallel 如何汇总子节点的结果。
type ParallelPolicy uint8

const (
	// ParallelRequireAll 在每个子节点都成功时成功，任一子节点失败时立即失败。
	ParallelRequireAll ParallelPolicy = iota
	// ParallelRequireOne 在任一子节点成功时立即成功，所有子节点都失败时失败。
	ParallelRequireOne
)

// Parallel 每一拍按顺序 Tick 每个尚未终止的子节点，直到策略的结果确定。
//
// 结果一旦确定（RequireAll 出现失败、RequireOne 出现成功）就停止本次 Tick，后面的子节点不再 Tick，随后 Reset 全部
// 子节点（打断仍在运行的）。不能把剩下的子节点也 Tick 一遍再 Reset：排在后面的 TaskflowAction 会先发起一个没人要的
// 动作、紧接着被打断，没配 OnInterrupt 时这个动作就成了孤儿。
type Parallel[C any] struct {
	Children []Node[C]
	Policy   ParallelPolicy
	states   []Status
}

func (n *Parallel[C]) Tick(ctx *C) Status {
	if n == nil || len(n.Children) == 0 {
		return StatusSuccess
	}
	if len(n.states) != len(n.Children) {
		n.states = make([]Status, len(n.Children))
	}
	successes, failures := 0, 0
	for index, child := range n.Children {
		if child == nil {
			n.states[index] = StatusSuccess
		}
		switch n.states[index] {
		case StatusSuccess:
			successes++
			continue
		case StatusFailure:
			failures++
			continue
		}
		switch child.Tick(ctx) {
		case StatusSuccess:
			n.states[index] = StatusSuccess
			successes++
		case StatusFailure:
			n.states[index] = StatusFailure
			failures++
		default:
			n.states[index] = StatusRunning
		}
		if n.decided(successes, failures) {
			break
		}
	}
	switch n.Policy {
	case ParallelRequireOne:
		if successes > 0 {
			n.Reset()
			return StatusSuccess
		}
		if failures == len(n.Children) {
			n.Reset()
			return StatusFailure
		}
	default: // ParallelRequireAll
		if failures > 0 {
			n.Reset()
			return StatusFailure
		}
		if successes == len(n.Children) {
			n.Reset()
			return StatusSuccess
		}
	}
	return StatusRunning
}

// decided 报告按策略结果是否已经确定：RequireAll 出现任一失败、RequireOne 出现任一成功。
// 全部完成的情形由循环自然结束覆盖。
func (n *Parallel[C]) decided(successes, failures int) bool {
	if n.Policy == ParallelRequireOne {
		return successes > 0
	}
	return failures > 0
}

func (n *Parallel[C]) Reset() {
	if n == nil {
		return
	}
	for _, child := range n.Children {
		if child != nil {
			child.Reset()
		}
	}
	n.states = nil
}

// Repeat 运行子节点 Count 次（Count <= 0 表示永远重复），子节点第一次失败时随之失败。
type Repeat[C any] struct {
	Child Node[C]
	Count int
	done  int
}

func (n *Repeat[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil {
		return StatusFailure
	}
	for {
		switch n.Child.Tick(ctx) {
		case StatusSuccess:
			n.Child.Reset()
			n.done++
			if n.Count > 0 && n.done >= n.Count {
				n.done = 0
				return StatusSuccess
			}
			// 永远重复（或还剩轮次）：下一拍再进入，而不是在同一拍里紧密循环。
			return StatusRunning
		case StatusFailure:
			n.Reset()
			return StatusFailure
		default:
			return StatusRunning
		}
	}
}

func (n *Repeat[C]) Reset() {
	if n == nil {
		return
	}
	if n.Child != nil {
		n.Child.Reset()
	}
	n.done = 0
}

// UntilSuccess 重试子节点直到成功。
type UntilSuccess[C any] struct{ Child Node[C] }

func (n *UntilSuccess[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil {
		return StatusFailure
	}
	switch n.Child.Tick(ctx) {
	case StatusSuccess:
		n.Child.Reset()
		return StatusSuccess
	case StatusFailure:
		n.Child.Reset()
		return StatusRunning
	default:
		return StatusRunning
	}
}

func (n *UntilSuccess[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// Succeeder 把子节点的失败映射为成功（运行中原样传出）。
type Succeeder[C any] struct{ Child Node[C] }

func (n *Succeeder[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil {
		return StatusSuccess
	}
	switch status := n.Child.Tick(ctx); status {
	case StatusFailure, StatusSuccess:
		return StatusSuccess
	default:
		return status
	}
}

func (n *Succeeder[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// Condition 是对一个谓词求值的叶子。
type Condition[C any] struct{ Check func(*C) bool }

func (n *Condition[C]) Tick(ctx *C) Status {
	if n == nil || n.Check == nil {
		return StatusFailure
	}
	if n.Check(ctx) {
		return StatusSuccess
	}
	return StatusFailure
}
func (*Condition[C]) Reset() {}

// Guard 用每拍重新检查的谓词把守子节点：谓词变为假时打断（Reset）运行中的子节点，Guard 失败。
type Guard[C any] struct {
	Check func(*C) bool
	Child Node[C]
}

func (n *Guard[C]) Tick(ctx *C) Status {
	if n == nil || n.Check == nil || n.Child == nil {
		return StatusFailure
	}
	if !n.Check(ctx) {
		n.Child.Reset()
		return StatusFailure
	}
	return n.Child.Tick(ctx)
}

func (n *Guard[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// Cooldown 在子节点上次完成距今不足 Ticks 时失败。Now 从上下文读确定性的 tick 时钟（刻意不提供墙钟选项）。
type Cooldown[C any] struct {
	Child Node[C]
	Ticks int64
	Now   func(*C) int64
	last  int64
	armed bool
}

func (n *Cooldown[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil || n.Now == nil {
		return StatusFailure
	}
	now := n.Now(ctx)
	if n.armed && now-n.last < n.Ticks {
		return StatusFailure
	}
	status := n.Child.Tick(ctx)
	if status == StatusSuccess || status == StatusFailure {
		n.armed, n.last = true, now
	}
	return status
}

// Reset 刻意保留 last / armed：策略在每次完成求值后重置整棵树，每次重置都忘掉上次触发的冷却永远挡不住任何东西。
// 所以冷却跨求值保留（也跨共享同一棵树实例的策略重装），直到节点值本身被丢弃。
func (n *Cooldown[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// TimeLimit 打断已运行 Ticks 拍或更久的子节点。
//
// 期限在 Tick 子节点之前判断：now-start >= Ticks 时直接 Reset（打断）并失败，这一拍不再
// Tick 子节点。BehaviorStrategy 把动作结束缓冲到下一拍送达，所以在最后一拍窗口里（或冻结
// 期间）已经结束、但要到期限那一拍才送达的动作，会被判为超时并收到一次 OnInterrupt；
// 冻结期间时钟照走时同理。需要宽限时把 Ticks 留出一拍，或在冻结期间停住 NowTick。
type TimeLimit[C any] struct {
	Child Node[C]
	Ticks int64
	Now   func(*C) int64
	start int64
	live  bool
}

func (n *TimeLimit[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil || n.Now == nil {
		return StatusFailure
	}
	now := n.Now(ctx)
	if !n.live {
		n.live, n.start = true, now
	}
	if now-n.start >= n.Ticks {
		n.Reset()
		return StatusFailure
	}
	status := n.Child.Tick(ctx)
	if status == StatusSuccess || status == StatusFailure {
		n.live = false
	}
	return status
}

func (n *TimeLimit[C]) Reset() {
	if n == nil {
		return
	}
	if n.Child != nil {
		n.Child.Reset()
	}
	n.live = false
}

// RandomSelector Tick 由注入的 roll 选中的一个子节点（roll 接收子节点数、返回下标）。子节点运行期间选择保持不变。
// 权威模拟上请注入确定性的 roll（combat.RollValue 一类）。
type RandomSelector[C any] struct {
	Children []Node[C]
	Roll     func(ctx *C, n int) int
	picked   int
	live     bool
}

func (n *RandomSelector[C]) Tick(ctx *C) Status {
	if n == nil || len(n.Children) == 0 || n.Roll == nil {
		return StatusFailure
	}
	if !n.live {
		index := n.Roll(ctx, len(n.Children))
		if index < 0 || index >= len(n.Children) {
			return StatusFailure
		}
		n.picked, n.live = index, true
	}
	child := n.Children[n.picked]
	if child == nil {
		n.live = false
		return StatusFailure
	}
	status := child.Tick(ctx)
	if status == StatusSuccess || status == StatusFailure {
		child.Reset()
		n.live = false
	}
	return status
}

func (n *RandomSelector[C]) Reset() {
	if n == nil {
		return
	}
	for _, child := range n.Children {
		if child != nil {
			child.Reset()
		}
	}
	n.live = false
}
