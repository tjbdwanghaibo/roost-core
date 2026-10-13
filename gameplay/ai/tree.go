// Package ai 在 roost-core 的 AI 策略与 actionflow 执行契约之上提供行为树：节点库（组合、装饰、基于确定性 tick 的
// 计时、注入的随机性），在 Controller 内运行树的 BehaviorStrategy 桥接，让树发起并等待 actionflow 动作的 TaskflowAction
// 叶子，以及严格的 JSON 树组装（ParseTree + Registry），快速失败并给出带路径的诊断。
//
// 范围：JSON 文档是数据驱动树的唯一来源——这里不定义可视化编辑器格式。也没有 utility / GOAP 规划器、没有感知 /
// 转向节点库（游戏自己注册条件 / 动作词表），没有跨 agent 的调度。
package ai

// Status 是节点一次 Tick 的结果。
type Status uint8

const (
	StatusReady Status = iota
	StatusRunning
	StatusSuccess
	StatusFailure
)

// Node 是行为树节点：Tick 推进一步，Reset 回到初始状态。
type Node[C any] interface {
	Tick(*C) Status
	Reset()
}

// FuncNode 用一个函数实现叶子节点，Reset 是空操作。
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
