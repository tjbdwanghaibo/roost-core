// Package ai provides behavior trees over the cube-core AI strategy and
// taskflow execution contracts: a node library (composites, decorators,
// deterministic tick-based timing, injected randomness), a BehaviorStrategy
// bridge that runs a tree inside the Controller, a TaskflowAction leaf that
// lets trees launch and await taskflow actions, and strict JSON tree
// assembly (ParseTree + Registry) with fail-fast, path-addressed diagnostics.
//
// Scope: JSON documents are the source of truth for data-driven trees — no
// visual-editor format is defined here. There is still no utility/GOAP
// planner, no perception/steering node library (games register their own
// condition/action vocabulary), and no cross-agent scheduling.
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
