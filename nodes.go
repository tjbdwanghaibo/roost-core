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

// Parallel ticks every non-terminal child each tick.
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

func (n *Cooldown[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}

// TimeLimit interrupts a child that has been running for more than Ticks.
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
