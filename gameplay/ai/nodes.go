package ai

// Node library. Every node follows the tick contract established by
// Sequence/Selector: Tick drives one evaluation step, Reset returns the
// subtree to its initial state (used both after completion and when a
// higher-priority branch interrupts a running one). Time-based nodes take an
// injected tick reader instead of wall clock, keeping authoritative-side AI
// decisions replayable; nodes that need randomness take an injected roll
// function for the same reason.

// ParallelPolicy selects how a Parallel aggregates its children.
type ParallelPolicy uint8

const (
	// ParallelRequireAll succeeds when every child succeeded and fails as
	// soon as any child fails.
	ParallelRequireAll ParallelPolicy = iota
	// ParallelRequireOne succeeds as soon as any child succeeds and fails
	// when every child failed.
	ParallelRequireOne
)

// Parallel ticks every non-terminal child each tick, in order, until the
// policy's outcome is decided.
//
// 结果一旦确定（RequireAll 出现失败、RequireOne 出现成功）就停止本次 Tick，后面的子节点
// 不再 Tick，随后 Reset 全部子节点（打断仍在运行的）。之前会把剩下的子节点也 Tick 一遍再
// Reset：排在后面的 TaskflowAction 先发起一个没人要的动作、紧接着被打断，没配 OnInterrupt
// 时这个动作成了孤儿（RR-20261005-NC-240）。
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

// Repeat runs its child Count times (Count <= 0 repeats forever), failing
// through on the child's first failure.
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
			// Forever (or more rounds left): re-enter next tick, not in a
			// tight loop within one tick.
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

// UntilSuccess retries its child until it succeeds.
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

// Succeeder maps its child's failure to success (running passes through).
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

// Condition is a leaf evaluating a predicate.
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

// Guard gates its child behind a predicate re-checked every tick: a running
// child whose guard turns false is interrupted (Reset) and the guard fails.
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

// Cooldown fails while the child's last completion is fresher than Ticks.
// Now reads the deterministic tick clock from the context (wall clock is
// deliberately not an option).
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

// Reset deliberately keeps last/armed: the strategy resets the whole tree
// after every completed evaluation, and a cooldown that forgot its last
// firing on each reset would never gate anything. The cooldown therefore
// persists across evaluations (and across strategy re-installs sharing the
// same tree instance) until the node value itself is discarded.
func (n *Cooldown[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// TimeLimit interrupts a child that has been running for Ticks or more.
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

// RandomSelector ticks one child chosen by the injected roll (roll receives
// the child count and returns an index). The choice is pinned while the
// child runs. Inject a deterministic roll (combat.RollValue-style) on
// authoritative simulations.
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
