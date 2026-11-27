package decision

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
)

// Step 完全由输入和快照决定；不修改输入，即便先前根已产生意图而后续条件错误也不泄漏部分结果。
// Tick：依序执行所有根，再处理到期队列。事件：只遍历当前阶段已开启且同名的监听，不隐含 Tick。
// 两者都在整个遍历结束后按请求顺序应用阶段/订阅变更，不递归调用新阶段的根或新开启的监听。
func (p *Program) Step(s State, in Input) (State, []Intent, error) {
	if err := p.ValidateState(s); err != nil {
		return State{}, nil, err
	}
	if in.Now < s.Now {
		return State{}, nil, errors.New("logic time moved backwards")
	}
	if in.Event != "" && (!validName(in.Event) || !p.events[in.Event]) {
		return State{}, nil, errors.New("unregistered input event")
	}
	if len(in.Fields) > len(p.fields) {
		return State{}, nil, errors.New("too many input fields")
	}
	for k := range in.Fields {
		if !validName(k) || !p.fields[k] {
			return State{}, nil, errors.New("unregistered input field")
		}
	}
	s.Active = slices.Clone(s.Active)
	s.Counts = slices.Clone(s.Counts)
	s.Pending = slices.Clone(s.Pending)
	s.Now = in.Now
	x := execution{p: p, s: s, fields: in.Fields, queued: make(map[int64]bool, len(s.Pending))}
	for _, q := range s.Pending {
		x.queued[q.NodeID] = true
	}
	st := p.stages[s.Stage]
	if in.Event == "" {
		if err := x.roots(st.Tick); err != nil {
			return State{}, nil, err
		}
		x.drain()
		x.s.LastTick = in.Now
	} else {
		for i, l := range st.Events {
			if !s.Active[i] || l.Event != in.Event {
				continue
			}
			match := true
			var err error
			if l.Filter != nil {
				match, err = x.check(l.Filter)
				if err != nil {
					return State{}, nil, err
				}
			}
			if match {
				if err = x.roots(l.Body); err != nil {
					return State{}, nil, err
				}
			}
			if l.Once {
				x.controls = append(x.controls, Action{Kind: "close", Arg: l.ID})
			}
		}
	}
	x.applyControls()
	return x.s, x.out, nil
}

type execution struct {
	p        *Program
	s        State
	fields   map[string]int64
	queued   map[int64]bool
	controls []Action
	out      []Intent
}

func (x *execution) check(t *Test) (bool, error) {
	v, ok := x.fields[t.Field]
	if !ok {
		return false, fmt.Errorf("missing decision input field %q", t.Field)
	}
	switch t.Op {
	case "eq":
		return v == t.Value, nil
	case "ne":
		return v != t.Value, nil
	case "lt":
		return v < t.Value, nil
	case "le":
		return v <= t.Value, nil
	case "gt":
		return v > t.Value, nil
	case "ge":
		return v >= t.Value, nil
	}
	return false, errors.New("invalid compiled comparison")
}
func (x *execution) roots(list []Node) error {
	for i := range list {
		if _, err := x.eval(&list[i]); err != nil {
			return err
		}
	}
	return nil
}
func (x *execution) eval(n *Node) (bool, error) {
	if n.Kind == "check" {
		return x.check(n.Test)
	}
	if n.Kind != "do" {
		all := n.Kind == "all"
		ok := all // 空 all 为真，空 any 为假。
		for i := range n.Children {
			var err error
			ok, err = x.eval(&n.Children[i])
			if err != nil {
				return false, err
			}
			if ok != all {
				break
			}
		}
		if !ok {
			return false, nil
		}
	}
	for _, a := range n.Actions {
		if a.Kind != "emit" {
			x.controls = append(x.controls, a)
			continue
		}
		e := x.p.emits[a.ID]
		if e.counter >= 0 && x.s.Counts[e.counter] >= a.Limit {
			continue
		}
		if a.Delay == 0 {
			x.emit(e)
			continue
		}
		if x.queued[a.ID] {
			continue
		}
		if a.Delay > math.MaxInt64-x.s.Now {
			return false, errors.New("logical deadline overflows int64")
		}
		x.s.Pending = append(x.s.Pending, Pending{NodeID: a.ID, Due: x.s.Now + a.Delay})
		x.queued[a.ID] = true
	}
	return true, nil
}
func (x *execution) emit(e emitInfo) {
	x.out = append(x.out, Intent{NodeID: e.a.ID, Name: e.a.Name, Arg: e.a.Arg})
	if e.counter >= 0 {
		x.s.Counts[e.counter]++
	}
}
func (x *execution) drain() {
	// 保留未到期条目的入队顺序；已到期先按期限、同期限按入队顺序出队，绝不遍历 map 决定输出顺序。
	var due []Pending
	future := x.s.Pending[:0]
	for _, q := range x.s.Pending {
		if q.Due <= x.s.Now {
			due = append(due, q)
		} else {
			future = append(future, q)
		}
	}
	if len(future) == 0 {
		future = nil // 全部出队也保持 JSON 往返的规范空形状。
	}
	x.s.Pending = future
	slices.SortStableFunc(due, func(a, b Pending) int { return cmp.Compare(a.Due, b.Due) })
	for _, q := range due {
		x.emit(x.p.emits[q.NodeID])
	}
}
func (x *execution) applyControls() {
	st := x.p.stages[x.s.Stage]
	for _, a := range x.controls {
		switch a.Kind {
		case "stage":
			if x.s.Stage != a.Name {
				st = x.p.stages[a.Name]
				x.s.Stage = a.Name
				x.s.Active = st.initialActive()
			}
		case "open", "close": // 同一轮之前的切换可能已替换整个订阅集；不把旧阶段的监听带入新阶段。
			if i, ok := st.byID[a.Arg]; ok {
				x.s.Active[i] = a.Kind == "open"
			}
		}
	}
}
