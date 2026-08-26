package ai

import (
	"time"

	coreai "github.com/tjbdwanghaibo/cube-core/ai"
	coreflow "github.com/tjbdwanghaibo/cube-core/taskflow"
)

// ActionEnd is one taskflow action completion delivered to the tree on the
// tick after it happened.
type ActionEnd struct {
	ID     int64
	Kind   coreflow.ActionKind
	Reason coreflow.ActionReason
}

// BehaviorContext is the tick context behavior-tree nodes receive when the
// tree runs as a strategy: the core AI context (owner entity, action list),
// the business data block, and the deterministic tick clock.
type BehaviorContext[C any] struct {
	Core    *coreai.Context
	Data    *C
	NowTick int64
	// actionEnds holds the completions that arrived since the previous tick;
	// TaskflowAction leaves consume them by id.
	actionEnds []ActionEnd
}

// TakeActionEnd consumes the pending completion for one action id.
func (b *BehaviorContext[C]) TakeActionEnd(id int64) (ActionEnd, bool) {
	for index, end := range b.actionEnds {
		if end.ID == id {
			b.actionEnds = append(b.actionEnds[:index], b.actionEnds[index+1:]...)
			return end, true
		}
	}
	return ActionEnd{}, false
}

// NowTicks adapts the context's tick clock to the node library's Now
// signature (Cooldown/TimeLimit).
func NowTicks[C any](ctx *BehaviorContext[C]) int64 { return ctx.NowTick }

// BehaviorStrategyOptions configures the tree-to-strategy bridge.
type BehaviorStrategyOptions[C any] struct {
	Name string
	Data *C
	// NowTick supplies the deterministic tick clock the tree sees. Required
	// when the tree contains time-based nodes; a nil source reads as 0.
	NowTick func() int64
	// CanStop answers Strategy.CanStopByNext; nil always allows replacement.
	CanStop func(next coreai.Strategy) bool
	// OnResult observes each completed tree evaluation (Success/Failure).
	OnResult func(Status)
}

// BehaviorStrategy runs a behavior tree as a coreai.Strategy, closing the
// gap between the tree skeleton and the Controller/taskflow execution flow:
// Tick drives the tree, action/mission completions are buffered and handed
// to the next tick's context, and a finished tree resets so the next tick
// re-evaluates from the root.
type BehaviorStrategy[C any] struct {
	root    Node[BehaviorContext[C]]
	options BehaviorStrategyOptions[C]
	pending []ActionEnd
}

func NewBehaviorStrategy[C any](root Node[BehaviorContext[C]], options BehaviorStrategyOptions[C]) *BehaviorStrategy[C] {
	if options.Name == "" {
		options.Name = "behavior_tree"
	}
	return &BehaviorStrategy[C]{root: root, options: options}
}

func (s *BehaviorStrategy[C]) Name() string { return s.options.Name }

func (s *BehaviorStrategy[C]) Init(*coreai.Context) error {
	if s.root != nil {
		s.root.Reset()
	}
	s.pending = nil
	return nil
}

func (s *BehaviorStrategy[C]) Tick(ctx *coreai.Context, _ time.Time) {
	if s.root == nil {
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

func (s *BehaviorStrategy[C]) OnActionEnd(_ *coreai.Context, actionID int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	s.pending = append(s.pending, ActionEnd{ID: actionID, Kind: kind, Reason: reason})
}

func (s *BehaviorStrategy[C]) OnMissionEnd(*coreai.Context, coreflow.Mission, coreflow.ActionReason) {
}

func (s *BehaviorStrategy[C]) CanStopByNext(next coreai.Strategy) bool {
	if s.options.CanStop != nil {
		return s.options.CanStop(next)
	}
	return true
}

// Stop resets the tree. Ending outstanding actions stays with the
// Controller's EndActions hook, which already runs on strategy replacement.
func (s *BehaviorStrategy[C]) Stop(*coreai.Context, string) {
	if s.root != nil {
		s.root.Reset()
	}
	s.pending = nil
}

// TaskflowAction is the leaf that closes the tree/taskflow loop: the tree
// decides, taskflow executes. On first tick it launches an action through
// the injected Launch (CreateAction, EnqueueAction, or SetMission — the
// business picks the taskflow verb), then stays Running until the matching
// ActionEnd arrives, mapping its reason through Succeeded.
//
// An interrupted branch (Reset while running) does NOT cancel the underlying
// action by itself — Reset carries no context. Supply OnInterrupt (a closure
// over the entity's ActionList) when interruption must end the action.
type TaskflowAction[C any] struct {
	Launch func(ctx *BehaviorContext[C]) (int64, error)
	// Succeeded maps the completion reason to a tree result; nil treats
	// every completion as success.
	Succeeded func(reason coreflow.ActionReason) bool
	// OnInterrupt runs when the branch is reset while the action is in
	// flight (optional).
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
