// Package ssrimport 只转换 SSR ai_engine 平铺编辑器配置的可证等价子集。
// 它不是 bt_engine/trigger_engine 解析器，也不内置业务 CID。
package ssrimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
)

// Bindings 由了解源业务 worker 的调用方显式声明。Conditions 必须只读输入快照；
// emit 不得被本次遍历的其他节点观测，stage 只能请求引擎延后切换。
// 一个 CID 在其类别中只有一种语义，不能用自定义解析回调放宽此约束。
type Bindings struct {
	Events     map[uint32]EventBinding
	Conditions map[uint32]string // 无左参数的纯 int64 快照字段
	Actions    map[uint32]ActionBinding
}

// EventBinding.Field 为空时只允许零参数事件；否则要求一个 int64 参数并显式过滤。
type EventBinding struct{ Name, Field string }

// ActionBinding.Kind 仅支持 emit（Name 为意图；一个 int 字面量）及 stage
// （Name 须为空；一个目标阶段字符串字面量）。
type ActionBinding struct{ Kind, Name string }

const (
	root = iota
	condition
	selector
	action
	sequence
	event
)

const (
	maxBytes = 1 << 20
	maxNodes = 1024
	maxDepth = 48
)

type entry struct {
	ID       int64           `json:"id"`
	CID      string          `json:"cid"`
	Type     int             `json:"type"`
	Name     string          `json:"unique_name"`
	Single   bool            `json:"mode"`
	Open     bool            `json:"enable"`
	Time     int64           `json:"time"`
	Delay    int64           `json:"delay"`
	Root     bool            `json:"root"`
	Children []int64         `json:"child"`
	Args     json.RawMessage `json:"args"`
}

// AIEngine 是已核实的输入方言标签，不是 SSR JSON 自带的字段。
const AIEngine = "ssr.ai_engine/854fe4f7"

// Source 必须由宿主根据原配置来源提供。数值节点类型在其他编辑器中会重叠，无法仅靠 JSON 推断。
type Source struct{ Dialect, Name, Start string }

// Import 将只含 node（及可选空 parameter）的 JSON 编译为独立的原生程序。
// 名称和启动阶段由宿主明确指定，不用 JSON 顺序猜测。任何不支持的语义均报错。
func Import(raw []byte, src Source, b Bindings, reg decision.Registry) (*decision.Program, error) {
	if src.Dialect != AIEngine {
		return nil, fmt.Errorf("unsupported or unspecified source dialect %q", src.Dialect)
	}
	if len(raw) > maxBytes || !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid or oversized editor JSON")
	}
	if err := uniqueKeys(raw); err != nil {
		return nil, err
	}
	doc, err := object(raw, []string{"node"}, "parameter")
	if err != nil {
		return nil, fmt.Errorf("editor: %w", err)
	}
	if p, ok := doc["parameter"]; ok && !emptyArray(p) {
		return nil, fmt.Errorf("editor parameters are unsupported")
	}
	var rows []json.RawMessage
	if err = json.Unmarshal(doc["node"], &rows); err != nil || len(rows) == 0 || len(rows) > maxNodes {
		return nil, fmt.Errorf("expected 1..%d flat nodes", maxNodes)
	}
	c := converter{nodes: make(map[int64]entry, len(rows)), bindings: b, eventNames: map[string]uint32{}}
	parents := map[int64]int64{}
	stages := map[string]bool{}
	var roots []int64
	for i, row := range rows {
		_, err = object(row, []string{"id", "type"}, "cid", "unique_name", "mode", "enable", "time", "delay", "root", "child", "args")
		if err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		var n entry
		if err = json.Unmarshal(row, &n); err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		if _, dup := c.nodes[n.ID]; dup || n.ID <= 0 {
			return nil, fmt.Errorf("duplicate or invalid node id %d", n.ID)
		}
		if n.Type < root || n.Type > event {
			return nil, fmt.Errorf("node %d: unknown type %d", n.ID, n.Type)
		}
		if n.Root != (n.Type == root) || (n.Type == root && (n.Name == "" || stages[n.Name])) {
			return nil, fmt.Errorf("node %d: invalid or duplicate stage root", n.ID)
		}
		if n.Type == root {
			stages[n.Name] = true
			roots = append(roots, n.ID)
		}
		if n.Type != event && (n.Single || n.Open) {
			return nil, fmt.Errorf("node %d: mode/enable only supported for listeners", n.ID)
		}
		if n.Delay != 0 || n.Time != 0 {
			return nil, fmt.Errorf("node %d: action delay/count is not in the verified subset", n.ID)
		}
		switch n.Type {
		case root, selector, sequence:
			if (n.CID != "" && n.CID != "0") || (n.Args != nil && !emptyArray(n.Args)) {
				return nil, fmt.Errorf("node %d: unsupported structural CID/args", n.ID)
			}
		case condition, action:
			if len(n.Children) != 0 {
				return nil, fmt.Errorf("node %d: leaf has children", n.ID)
			}
		}
		if n.Type != condition && n.Type != action && len(n.Children) == 0 {
			return nil, fmt.Errorf("node %d: empty branch", n.ID)
		}
		for _, kid := range n.Children {
			if _, dup := parents[kid]; dup {
				return nil, fmt.Errorf("node %d: shared or repeated child %d", n.ID, kid)
			}
			parents[kid] = n.ID
		}
		c.nodes[n.ID] = n
	}
	for kid, parent := range parents {
		if _, ok := c.nodes[kid]; !ok {
			return nil, fmt.Errorf("node %d: unknown child %d", parent, kid)
		}
	}
	if !stages[src.Start] {
		return nil, fmt.Errorf("unknown start stage %q", src.Start)
	}
	// With single ownership, a visited child cannot be reached twice from roots.
	// Check detached cycles as well as orphans before converting anything.
	visited := map[int64]bool{}
	var walk func(int64, int) error
	walk = func(id int64, d int) error {
		if visited[id] || d > maxDepth {
			return fmt.Errorf("cyclic or too-deep tree at %d", id)
		}
		visited[id] = true
		for _, kid := range c.nodes[id].Children {
			if err := walk(kid, d+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range roots {
		if _, has := parents[id]; has {
			return nil, fmt.Errorf("root %d has parent", id)
		}
		if err := walk(id, 1); err != nil {
			return nil, err
		}
	}
	if len(visited) != len(c.nodes) {
		return nil, fmt.Errorf("detached cycle or orphan node")
	}
	def := decision.Definition{Schema: decision.Schema, Name: src.Name, Start: src.Start}
	for _, id := range roots {
		st, err := c.stage(c.nodes[id])
		if err != nil {
			return nil, err
		}
		def.Stages = append(def.Stages, st)
	}
	return decision.Compile(def, reg)
}

type converter struct {
	nodes      map[int64]entry
	bindings   Bindings
	eventNames map[string]uint32
	stageName  string
	hasSwitch  bool
}

func (c *converter) stage(r entry) (decision.Stage, error) {
	c.stageName, c.hasSwitch = r.Name, false
	st := decision.Stage{Name: r.Name}
	listeners := map[string]bool{}
	for _, id := range r.Children {
		n := c.nodes[id]
		if n.Type == event {
			l, err := c.listener(n)
			if err != nil {
				return st, fmt.Errorf("event %d: %w", id, err)
			}
			if listeners[l.Event] {
				return st, fmt.Errorf("stage %q: unordered listeners for %q", st.Name, l.Event)
			}
			listeners[l.Event] = true
			st.Events = append(st.Events, l)
		} else {
			v, err := c.bodyNode(n)
			if err != nil {
				return st, err
			}
			st.Tick = append(st.Tick, v)
		}
	}
	return st, nil
}
func (c *converter) bodyNode(n entry) (decision.Node, error) {
	if n.Type != action && n.Type != sequence && n.Type != selector {
		return decision.Node{}, fmt.Errorf("node %d: invalid root/event child", n.ID)
	}
	return c.branch(n)
}
func (c *converter) branch(n entry) (decision.Node, error) {
	var v decision.Node
	switch n.Type {
	case condition:
		t, err := c.test(n)
		if err != nil {
			return v, fmt.Errorf("condition %d: %w", n.ID, err)
		}
		v.Kind = "check"
		v.Test = t
	case action:
		a, err := c.act(n)
		if err != nil {
			return v, fmt.Errorf("action %d: %w", n.ID, err)
		}
		v.Kind = "do"
		v.Actions = []decision.Action{a}
	case sequence, selector:
		v.Kind = "all"
		if n.Type == selector {
			v.Kind = "any"
		}
		// SSR 的组合节点把动作与决策子节点分组；动作只在组合成功后按原相对顺序执行。
		for _, kid := range n.Children {
			child := c.nodes[kid]
			if child.Type == action {
				a, err := c.act(child)
				if err != nil {
					return v, fmt.Errorf("action %d: %w", kid, err)
				}
				v.Actions = append(v.Actions, a)
			} else {
				sub, err := c.branch(child)
				if err != nil {
					return v, err
				}
				v.Children = append(v.Children, sub)
			}
		}
	default:
		return v, fmt.Errorf("node %d: invalid composite child", n.ID)
	}
	return v, nil
}
func (n entry) cid() (uint32, error) {
	v, err := strconv.ParseUint(n.CID, 10, 32)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != n.CID {
		return 0, fmt.Errorf("invalid CID %q", n.CID)
	}
	return uint32(v), nil
}
func (c *converter) listener(n entry) (decision.Listener, error) {
	l := decision.Listener{ID: n.ID, Enabled: n.Open, Once: n.Single}
	id, err := n.cid()
	if err != nil {
		return l, err
	}
	b, ok := c.bindings.Events[id]
	if !ok || b.Name == "" {
		return l, fmt.Errorf("unmapped event CID %d", id)
	}
	if old, used := c.eventNames[b.Name]; used && old != id {
		return l, fmt.Errorf("event aliases across distinct CIDs for %q", b.Name)
	}
	c.eventNames[b.Name] = id
	l.Event = b.Name
	if b.Field == "" {
		if !emptyArray(n.Args) {
			return l, fmt.Errorf("expected zero event parameters")
		}
	} else {
		a, err := oneArg(n.Args, true)
		if err != nil {
			return l, err
		}
		op := ops[a.Op]
		if a.Type != "int" || op == "" || !a.Enable {
			return l, fmt.Errorf("expected enabled unary int event filter")
		}
		value, err := strconv.ParseInt(a.Value, 10, 64)
		if err != nil {
			return l, err
		}
		l.Filter = &decision.Test{Field: b.Field, Op: op, Value: value}
	}
	for _, kid := range n.Children {
		v, err := c.bodyNode(c.nodes[kid])
		if err != nil {
			return l, err
		}
		l.Body = append(l.Body, v)
	}
	return l, nil
}

var ops = map[string]string{"eq": "eq", "equal": "eq", "lt": "lt", "less": "lt", "lte": "le", "gt": "gt", "greater": "gt", "gte": "ge"}

func (c *converter) test(n entry) (*decision.Test, error) {
	id, err := n.cid()
	if err != nil {
		return nil, err
	}
	f, ok := c.bindings.Conditions[id]
	if !ok || f == "" {
		return nil, fmt.Errorf("unmapped condition CID %d", id)
	}
	m, err := object(n.Args, []string{"left_args", "right_args", "op"})
	if err != nil {
		return nil, err
	}
	var op string
	if !emptyArray(m["left_args"]) || json.Unmarshal(m["op"], &op) != nil || ops[op] == "" {
		return nil, fmt.Errorf("expected unary field comparison")
	}
	a, err := oneArg(m["right_args"], false)
	if err != nil {
		return nil, err
	}
	if a.Type != "int" {
		return nil, fmt.Errorf("expected int literal")
	}
	v, err := strconv.ParseInt(a.Value, 10, 64)
	if err != nil {
		return nil, err
	}
	return &decision.Test{Field: f, Op: ops[op], Value: v}, nil
}
func (c *converter) act(n entry) (decision.Action, error) {
	a := decision.Action{ID: n.ID}
	id, err := n.cid()
	if err != nil {
		return a, err
	}
	b, ok := c.bindings.Actions[id]
	if !ok {
		return a, fmt.Errorf("unmapped action CID %d", id)
	}
	arg, err := oneArg(n.Args, false)
	if err != nil {
		return a, err
	}
	a.Kind = b.Kind
	switch b.Kind {
	case "emit":
		if b.Name == "" || arg.Type != "int" {
			return a, fmt.Errorf("expected named emit of int literal")
		}
		a.Name = b.Name
		a.Arg, err = strconv.ParseInt(arg.Value, 10, 64)
	case "stage":
		if b.Name != "" || arg.Type != "string" || arg.Value == "" || arg.Value != strings.TrimSpace(arg.Value) {
			return a, fmt.Errorf("expected literal stage name without surrounding whitespace")
		}
		// 源提交的 root.copy 不复制名字，自切并非可靠的空操作。也不接受同次遍历可能多次切换。
		if arg.Value == c.stageName || c.hasSwitch {
			return a, fmt.Errorf("self or multiple stage transitions in stage %q", c.stageName)
		}
		c.hasSwitch = true
		a.Name = arg.Value
	default:
		return a, fmt.Errorf("unsupported worker semantics %q", b.Kind)
	}
	return a, err
}

type arg struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	Op     string `json:"op"`
	Enable bool   `json:"enable"`
}

func oneArg(raw []byte, filter bool) (arg, error) {
	var a arg
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		return a, fmt.Errorf("expected exactly one argument")
	}
	keys := []string{"type", "value"}
	if filter {
		keys = append(keys, "op", "enable")
	}
	if _, err := object(items[0], keys); err != nil {
		return a, err
	}
	err := json.Unmarshal(items[0], &a)
	return a, err
}
func emptyArray(raw []byte) bool {
	var a []json.RawMessage
	return json.Unmarshal(raw, &a) == nil && a != nil && len(a) == 0
}
func object(raw []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("expected object")
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("missing field %q", k)
		}
	}
	for k := range m {
		seen := false
		for _, s := range required {
			if k == s {
				seen = true
				break
			}
		}
		if !seen {
			for _, s := range optional {
				if k == s {
					seen = true
					break
				}
			}
		}
		if !seen {
			return nil, fmt.Errorf("unknown field %q", k)
		}
	}
	return m, nil
}

// encoding/json normally accepts duplicate names and silently maps null to Go zero values.
func uniqueKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("JSON nesting limit")
		}
		v, err := d.Token()
		if err != nil {
			return err
		}
		switch v {
		case nil:
			return fmt.Errorf("null is not supported")
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return fmt.Errorf("invalid or duplicate JSON member %q", k)
				}
				seen[k] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
