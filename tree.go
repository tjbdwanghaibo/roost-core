// Package ai provides a minimal behavior-tree skeleton: composite/decorator
// nodes over a Status state machine, a Blackboard, and a tick Controller.
//
// Scope: this is deliberately a skeleton, not an AI middleware. There is no
// node library (no pathfinding/steering/perception nodes), no visual editor
// format, no utility/GOAP planner, and no built-in scheduling across agents.
// Games compose their own node vocabulary on top; anything beyond "tick a
// tree against a blackboard" stays with the caller.
package ai

type Status uint8

const (
	StatusReady Status = iota
	StatusRunning
	StatusSuccess
	StatusFailure
)

type Node[C any] interface {
	Tick(*C) Status
	Reset()
}

type FuncNode[C any] struct{ Run func(*C) Status }

func (n *FuncNode[C]) Tick(ctx *C) Status {
	if n == nil || n.Run == nil {
		return StatusFailure
	}
	return n.Run(ctx)
}
func (*FuncNode[C]) Reset() {}

type Sequence[C any] struct {
	Children []Node[C]
	cursor   int
}

func (n *Sequence[C]) Tick(ctx *C) Status {
	if n == nil || len(n.Children) == 0 {
		return StatusSuccess
	}
	for n.cursor < len(n.Children) {
		child := n.Children[n.cursor]
		if child == nil {
			n.cursor++
			continue
		}
		switch child.Tick(ctx) {
		case StatusReady, StatusRunning:
			return StatusRunning
		case StatusFailure:
			n.Reset()
			return StatusFailure
		case StatusSuccess:
			n.cursor++
		default:
			n.Reset()
			return StatusFailure
		}
	}
	n.Reset()
	return StatusSuccess
}
func (n *Sequence[C]) Reset() {
	if n == nil {
		return
	}
	for _, child := range n.Children {
		if child != nil {
			child.Reset()
		}
	}
	n.cursor = 0
}

type Selector[C any] struct {
	Children []Node[C]
	cursor   int
}

func (n *Selector[C]) Tick(ctx *C) Status {
	if n == nil || len(n.Children) == 0 {
		return StatusFailure
	}
	for n.cursor < len(n.Children) {
		child := n.Children[n.cursor]
		if child == nil {
			n.cursor++
			continue
		}
		switch child.Tick(ctx) {
		case StatusReady, StatusRunning:
			return StatusRunning
		case StatusSuccess:
			n.Reset()
			return StatusSuccess
		case StatusFailure:
			n.cursor++
		default:
			n.Reset()
			return StatusFailure
		}
	}
	n.Reset()
	return StatusFailure
}
func (n *Selector[C]) Reset() {
	if n == nil {
		return
	}
	for _, child := range n.Children {
		if child != nil {
			child.Reset()
		}
	}
	n.cursor = 0
}

type Inverter[C any] struct{ Child Node[C] }

func (n *Inverter[C]) Tick(ctx *C) Status {
	if n == nil || n.Child == nil {
		return StatusFailure
	}
	switch status := n.Child.Tick(ctx); status {
	case StatusSuccess:
		return StatusFailure
	case StatusFailure:
		return StatusSuccess
	default:
		return status
	}
}
func (n *Inverter[C]) Reset() {
	if n != nil && n.Child != nil {
		n.Child.Reset()
	}
}
