package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type names map[string]bool

type compiledStage struct {
	Stage
	byID              map[int64]int
	canOpen, canClose []bool
	next              names
	reachable         bool
}

type emitInfo struct {
	a       Action
	stage   *compiledStage
	tick    bool // 这个 ID 只能由 Tick 还是事件求值
	counter int  // -1 表示不需要寿命计数
}

// Program 的所有成员只在 Compile 中写入。零值不是可执行的程序。
type Program struct {
	plan, start    string
	events, fields names
	stages         map[string]*compiledStage
	emits          map[int64]emitInfo
	limited        []emitInfo
	delayed        int
}

func validName(s string) bool {
	if len(s) == 0 || len(s) > MaxNameBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || i > 0 && (b == '.' || b == '-' || b == ':')) {
			return false
		}
	}
	return true
}

func nameSet(list []string) (names, error) {
	if len(list) > MaxRegistryNames {
		return nil, errors.New("too many registered names")
	}
	m := make(names, len(list))
	for _, s := range list {
		if !validName(s) || m[s] {
			return nil, errors.New("invalid or duplicate registered name")
		}
		m[strings.Clone(s)] = true
	}
	return m, nil
}

func Compile(d Definition, reg Registry) (*Program, error) {
	if d.Schema != Schema || !validName(d.Name) || !validName(d.Start) || len(d.Stages) == 0 || len(d.Stages) > MaxStages {
		return nil, errors.New("invalid decision schema, name, start or stage count")
	}
	events, err := nameSet(reg.Events)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	fields, err := nameSet(reg.Fields)
	if err != nil {
		return nil, fmt.Errorf("fields: %w", err)
	}
	intents, err := nameSet(reg.Intents)
	if err != nil {
		return nil, fmt.Errorf("intents: %w", err)
	}
	p := &Program{start: strings.Clone(d.Start), events: events, fields: fields, stages: make(map[string]*compiledStage), emits: make(map[int64]emitInfo)}
	c := compiler{p: p, intents: intents, ids: make(map[int64]bool), remaining: MaxItems - len(d.Stages)}
	// 两遍处理：允许前向引用；在递归拷贝/编码之前检查 Go 调用者给的总规模和深度（包括环）。
	for _, s := range d.Stages {
		if !validName(s.Name) || p.stages[s.Name] != nil || len(s.Events) > MaxListenersPerStage || !c.take(len(s.Events)) {
			return nil, errors.New("invalid/duplicate stage or too many listeners")
		}
		st := &compiledStage{byID: make(map[int64]int), canOpen: make([]bool, len(s.Events)), canClose: make([]bool, len(s.Events)), next: make(names)}
		p.stages[strings.Clone(s.Name)] = st
		for i, l := range s.Events {
			if !c.id(l.ID) || !validName(l.Event) || !events[l.Event] {
				return nil, fmt.Errorf("stage %s: invalid listener ID or event", s.Name)
			}
			st.byID[l.ID] = i
			st.canClose[i] = l.Once
		}
	}
	if p.stages[d.Start] == nil {
		return nil, errors.New("unknown start stage")
	}
	for _, s := range d.Stages {
		st := p.stages[s.Name]
		if err := c.nodes(st, s.Tick, 1, true); err != nil {
			return nil, fmt.Errorf("stage %s tick: %w", s.Name, err)
		}
		for _, l := range s.Events {
			if l.Filter != nil && !p.validTest(l.Filter) {
				return nil, fmt.Errorf("invalid listener %d filter", l.ID)
			}
			if err := c.nodes(st, l.Body, 1, false); err != nil {
				return nil, fmt.Errorf("listener %d: %w", l.ID, err)
			}
		}
	}
	// 此时树已完全有界。规范 JSON 消去可选空切片差异，保留有语义的遍历顺序；
	// 注册集合按 JSON map 的规范键序列入哈希，而不是引用或迭代调用者的切片。
	frozen, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	identity, err := json.Marshal(struct {
		Definition              json.RawMessage
		Events, Fields, Intents names
	}{frozen, events, fields, intents})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(identity)
	p.plan = hex.EncodeToString(sum[:])
	var own Definition
	if err = json.Unmarshal(frozen, &own); err != nil {
		return nil, err
	}
	for _, s := range own.Stages {
		p.stages[s.Name].Stage = s
	}
	var mark func(*compiledStage)
	mark = func(st *compiledStage) {
		if st.reachable {
			return
		}
		st.reachable = true
		for next := range st.next {
			mark(p.stages[next])
		}
	}
	mark(p.stages[d.Start])
	return p, nil
}

type compiler struct {
	p         *Program
	intents   names
	ids       map[int64]bool
	remaining int
}

func (c *compiler) take(n int) bool {
	if n > c.remaining {
		return false
	}
	c.remaining -= n
	return true
}
func (c *compiler) id(id int64) bool {
	if id <= 0 || c.ids[id] {
		return false
	}
	c.ids[id] = true
	return true
}
func (p *Program) validTest(t *Test) bool {
	if t == nil || !validName(t.Field) || !p.fields[t.Field] {
		return false
	}
	switch t.Op {
	case "eq", "ne", "lt", "le", "gt", "ge":
		return true
	}
	return false
}
func (c *compiler) nodes(st *compiledStage, nodes []Node, depth int, tick bool) error {
	if len(nodes) == 0 {
		return nil
	}
	if depth > MaxDepth || !c.take(len(nodes)) {
		return errors.New("decision tree exceeds depth or item limit")
	}
	for _, n := range nodes {
		switch n.Kind {
		case "check":
			if !c.p.validTest(n.Test) || n.Children != nil || n.Actions != nil {
				return errors.New("check requires only a valid test")
			}
		case "do":
			if n.Test != nil || n.Children != nil || len(n.Actions) == 0 {
				return errors.New("do requires only nonempty actions")
			}
		case "all", "any":
			if n.Test != nil || len(n.Children) == 0 && len(n.Actions) == 0 {
				return errors.New("all/any requires children or actions, not a test")
			}
		default:
			return errors.New("unknown decision node kind")
		}
		if err := c.nodes(st, n.Children, depth+1, tick); err != nil {
			return err
		}
		if !c.take(len(n.Actions)) {
			return errors.New("decision tree exceeds item limit")
		}
		for _, a := range n.Actions {
			if err := c.action(st, a, tick); err != nil {
				return fmt.Errorf("action %d: %w", a.ID, err)
			}
		}
	}
	return nil
}
func (c *compiler) action(st *compiledStage, a Action, tick bool) error {
	if !c.id(a.ID) {
		return errors.New("invalid or duplicate ID")
	}
	if a.Kind != "emit" && (a.Delay != 0 || a.Limit != 0) {
		return errors.New("only emit supports delay or limit")
	}
	switch a.Kind {
	case "emit":
		if !validName(a.Name) || !c.intents[a.Name] || a.Delay < 0 || a.Limit < 0 {
			return errors.New("invalid intent, delay or limit")
		}
		a.Name = strings.Clone(a.Name)
		a.Kind = "emit"
		e := emitInfo{a: a, stage: st, tick: tick, counter: -1}
		if a.Limit > 0 {
			e.counter = len(c.p.limited)
			c.p.limited = append(c.p.limited, e)
		}
		c.p.emits[a.ID] = e
		if a.Delay > 0 {
			c.p.delayed++
		}
	case "stage":
		if !validName(a.Name) || c.p.stages[a.Name] == nil || a.Arg != 0 {
			return errors.New("stage requires only a known stage name")
		}
		st.next[strings.Clone(a.Name)] = true
	case "open", "close":
		i, ok := st.byID[a.Arg]
		if !ok || a.Name != "" {
			return errors.New("control requires only a listener ID in its stage")
		}
		if a.Kind == "open" {
			st.canOpen[i] = true
		} else {
			st.canClose[i] = true
		}
	default:
		return errors.New("unknown action kind")
	}
	return nil
}
