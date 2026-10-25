package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// TreeSchema 是 JSON 行为树的 wire schema 标识。
const TreeSchema = "roost.ai/v1"

// maxTreeDepth 限制递归深度，恶意或损坏的文档不能撑爆栈。
const maxTreeDepth = 64

// wire 错误。
var (
	ErrTreeSchema  = errors.New("ai: unsupported tree schema")
	ErrTreeInvalid = errors.New("ai: invalid tree document")
)

// Registry 提供 JSON 树组装用的叶子词表。组合与装饰节点（sequence、selector、parallel、inverter、succeeder、
// until_success、repeat、cooldown、time_limit、guard）是内置的；业务只注册自己的条件与动作叶子。工厂收到叶子原始的
// "args"，必须自己严格解析。
type Registry[C any] struct {
	// now 供内置的时间节点（cooldown / time_limit）使用；文档用到它们时必填。
	now func(*C) int64
	// traceNodes 为真时 ParseTree / ParseStages 用 Traced 包住每个节点（路径是 JSON 路径）。
	traceNodes bool
	conditions map[string]func(args json.RawMessage) (Node[C], error)
	actions    map[string]func(args json.RawMessage) (Node[C], error)
}

// NewRegistry 构造叶子词表；now 供内置时间节点使用，文档不用时间节点时可以为 nil。
func NewRegistry[C any](now func(*C) int64) *Registry[C] {
	return &Registry[C]{
		now:        now,
		conditions: make(map[string]func(args json.RawMessage) (Node[C], error)),
		actions:    make(map[string]func(args json.RawMessage) (Node[C], error)),
	}
}

// TraceNodes 让之后解析的树每个节点都带轨迹（Traced，路径是 JSON 路径），配合 BehaviorStrategyOptions.Trace 使用。
// 只影响之后的解析。
func (r *Registry[C]) TraceNodes() { r.traceNodes = true }

// RegisterCondition 注册条件叶子工厂；名字为空、工厂为 nil 或重复注册时返回包装 ErrTreeInvalid 的错误。
func (r *Registry[C]) RegisterCondition(name string, build func(args json.RawMessage) (Node[C], error)) error {
	if name == "" || build == nil {
		return fmt.Errorf("%w: condition registration requires a name and factory", ErrTreeInvalid)
	}
	if _, exists := r.conditions[name]; exists {
		return fmt.Errorf("%w: duplicate condition %q", ErrTreeInvalid, name)
	}
	r.conditions[name] = build
	return nil
}

// RegisterAction 注册动作叶子工厂；种类为空、工厂为 nil 或重复注册时返回包装 ErrTreeInvalid 的错误。
func (r *Registry[C]) RegisterAction(kind string, build func(args json.RawMessage) (Node[C], error)) error {
	if kind == "" || build == nil {
		return fmt.Errorf("%w: action registration requires a kind and factory", ErrTreeInvalid)
	}
	if _, exists := r.actions[kind]; exists {
		return fmt.Errorf("%w: duplicate action %q", ErrTreeInvalid, kind)
	}
	r.actions[kind] = build
	return nil
}

// ParseTree 从 JSON 文档组装行为树。解析沿用 skillv2 wire 的严格传统：未知字段、未知节点名、子节点数量不符与深度
// 超限都在组装期拒绝，并点名出错的 JSON 路径——损坏的文档绝不会变成一棵静默出错的树。
//
// 返回的树带有活的求值状态，只属于一个策略：在策略（或 agent）之间共享会让它们的游标与计时状态交叉——每个使用方
// 各解析一次。解析失败不影响当前已安装的策略，这一点 Controller.SetStrategy 已经保证。
func ParseTree[C any](data []byte, registry *Registry[C]) (Node[C], error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry is required", ErrTreeInvalid)
	}
	var document struct {
		Schema string          `json:"schema"`
		Root   json.RawMessage `json:"root"`
	}
	if err := decodeStrictJSON(data, &document); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTreeInvalid, err)
	}
	if document.Schema != TreeSchema {
		return nil, fmt.Errorf("%w: %q", ErrTreeSchema, document.Schema)
	}
	if len(document.Root) == 0 {
		return nil, fmt.Errorf("%w: $.root is required (a document with stages is parsed by ParseStages)", ErrTreeInvalid)
	}
	parser := &treeParser[C]{registry: registry}
	root, err := parser.node(document.Root, "$.root", 0)
	if err != nil {
		return nil, err
	}
	if len(parser.stageRefs) > 0 {
		return nil, fmt.Errorf("%w: %s: set_stage needs a document with stages (ParseStages)", ErrTreeInvalid, parser.stageRefs[0].path)
	}
	return root, nil
}

// Stages 是分阶段文档解析的结果：每个阶段一棵树，与起始阶段（交给 NewStagedStrategy）。
type Stages[C any] struct {
	Roots map[string]Node[C]
	Start string
}

// ParseStages 解析分阶段的 JSON 文档：{"schema": "roost.ai/v1", "start": "<阶段>", "stages": {"<阶段>": <节点>, …}}。
// 规则与 ParseTree 相同，另外校验起始阶段与每个 set_stage 叶子指向的阶段都存在。C 是节点的上下文类型（通常是
// BehaviorContext[D]），用到 set_stage 时它必须能切阶段。
func ParseStages[C any](data []byte, registry *Registry[C]) (Stages[C], error) {
	if registry == nil {
		return Stages[C]{}, fmt.Errorf("%w: registry is required", ErrTreeInvalid)
	}
	var document struct {
		Schema string                     `json:"schema"`
		Start  string                     `json:"start"`
		Stages map[string]json.RawMessage `json:"stages"`
	}
	if err := decodeStrictJSON(data, &document); err != nil {
		return Stages[C]{}, fmt.Errorf("%w: %v", ErrTreeInvalid, err)
	}
	if document.Schema != TreeSchema {
		return Stages[C]{}, fmt.Errorf("%w: %q", ErrTreeSchema, document.Schema)
	}
	if len(document.Stages) == 0 {
		return Stages[C]{}, fmt.Errorf("%w: $.stages must not be empty", ErrTreeInvalid)
	}
	if _, ok := document.Stages[document.Start]; !ok {
		return Stages[C]{}, fmt.Errorf("%w: $.start: unknown stage %q", ErrTreeInvalid, document.Start)
	}
	parser := &treeParser[C]{registry: registry}
	out := Stages[C]{Roots: make(map[string]Node[C], len(document.Stages)), Start: document.Start}
	names := make([]string, 0, len(document.Stages))
	for name := range document.Stages {
		names = append(names, name)
	}
	slices.Sort(names) // 诊断确定：同一份坏文档总报同一个错
	for _, name := range names {
		root, err := parser.node(document.Stages[name], fmt.Sprintf("$.stages[%q]", name), 0)
		if err != nil {
			return Stages[C]{}, err
		}
		out.Roots[name] = root
	}
	for _, ref := range parser.stageRefs {
		if _, ok := document.Stages[ref.stage]; !ok {
			return Stages[C]{}, fmt.Errorf("%w: %s: set_stage to unknown stage %q", ErrTreeInvalid, ref.path, ref.stage)
		}
	}
	return out, nil
}

// treeParser 是一次解析的状态：注册表与收集到的 set_stage 引用（解析完再核对阶段名）。
type treeParser[C any] struct {
	registry  *Registry[C]
	stageRefs []stageRef
}

type stageRef struct{ path, stage string }

// node 解析一个节点；注册表开了轨迹时用 Traced 包住它。
func (p *treeParser[C]) node(data []byte, path string, depth int) (Node[C], error) {
	node, err := p.parse(data, path, depth)
	if err != nil || !p.registry.traceNodes {
		return node, err
	}
	return &Traced[C]{Path: path, Child: node}, nil
}

func (p *treeParser[C]) parse(data []byte, path string, depth int) (Node[C], error) {
	registry := p.registry
	if depth > maxTreeDepth {
		return nil, fmt.Errorf("%w: %s exceeds depth %d", ErrTreeInvalid, path, maxTreeDepth)
	}
	var header struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	switch header.Node {
	case "sequence", "selector":
		children, err := p.children(data, path, depth)
		if err != nil {
			return nil, err
		}
		if header.Node == "sequence" {
			return &Sequence[C]{Children: children}, nil
		}
		return &Selector[C]{Children: children}, nil
	case "parallel":
		var raw struct {
			Node     string            `json:"node"`
			Policy   string            `json:"policy"`
			Children []json.RawMessage `json:"children"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		var policy ParallelPolicy
		switch raw.Policy {
		case "", "all":
			policy = ParallelRequireAll
		case "one":
			policy = ParallelRequireOne
		default:
			return nil, fmt.Errorf("%w: %s.policy: unknown policy %q", ErrTreeInvalid, path, raw.Policy)
		}
		children, err := p.childList(raw.Children, path, depth)
		if err != nil {
			return nil, err
		}
		return &Parallel[C]{Children: children, Policy: policy}, nil
	case "inverter", "succeeder", "until_success":
		child, err := p.singleChild(data, path, depth)
		if err != nil {
			return nil, err
		}
		switch header.Node {
		case "inverter":
			return &Inverter[C]{Child: child}, nil
		case "succeeder":
			return &Succeeder[C]{Child: child}, nil
		default:
			return &UntilSuccess[C]{Child: child}, nil
		}
	case "repeat":
		var raw struct {
			Node  string          `json:"node"`
			Count int             `json:"count"`
			Child json.RawMessage `json:"child"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		child, err := p.requireChild(raw.Child, path, depth)
		if err != nil {
			return nil, err
		}
		return &Repeat[C]{Child: child, Count: raw.Count}, nil
	case "cooldown", "time_limit":
		var raw struct {
			Node  string          `json:"node"`
			Ticks int64           `json:"ticks"`
			Child json.RawMessage `json:"child"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		if raw.Ticks <= 0 {
			return nil, fmt.Errorf("%w: %s.ticks must be positive", ErrTreeInvalid, path)
		}
		if registry.now == nil {
			return nil, fmt.Errorf("%w: %s: registry has no tick clock for %q", ErrTreeInvalid, path, header.Node)
		}
		child, err := p.requireChild(raw.Child, path, depth)
		if err != nil {
			return nil, err
		}
		if header.Node == "cooldown" {
			return &Cooldown[C]{Child: child, Ticks: raw.Ticks, Now: registry.now}, nil
		}
		return &TimeLimit[C]{Child: child, Ticks: raw.Ticks, Now: registry.now}, nil
	case "guard":
		var raw struct {
			Node      string          `json:"node"`
			Condition json.RawMessage `json:"condition"`
			Child     json.RawMessage `json:"child"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		if len(raw.Condition) == 0 {
			return nil, fmt.Errorf("%w: %s.condition is required", ErrTreeInvalid, path)
		}
		// guard 谓词每拍重新求值，结果压成一个布尔值，所以只允许无状态的谓词形状：条件叶子以及在它们之上的纯逻辑。带副作用
		// 或运行状态的节点（action、cooldown、repeat 等）会在每次检查时触发且永远不被重置——在组装期拒绝。
		if err := validatePredicate(raw.Condition, path+".condition", depth+1); err != nil {
			return nil, err
		}
		predicate, err := p.node(raw.Condition, path+".condition", depth+1)
		if err != nil {
			return nil, err
		}
		child, err := p.requireChild(raw.Child, path, depth)
		if err != nil {
			return nil, err
		}
		// guard 谓词必须无状态；条件叶子的契约满足这一点，所以 guard 只需问它是否 Success。
		return &Guard[C]{Check: func(ctx *C) bool { return predicate.Tick(ctx) == StatusSuccess }, Child: child}, nil
	case "event":
		var raw struct {
			Node string `json:"node"`
			Kind uint16 `json:"kind"`
			Take bool   `json:"take"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		if _, ok := any((*C)(nil)).(eventSource); !ok {
			return nil, fmt.Errorf("%w: %s: the tree context has no events (use BehaviorContext)", ErrTreeInvalid, path)
		}
		return &EventReceived[C]{Kind: raw.Kind, Take: raw.Take}, nil
	case "set_stage":
		var raw struct {
			Node  string `json:"node"`
			Stage string `json:"stage"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		if _, ok := any((*C)(nil)).(stageSwitcher); !ok {
			return nil, fmt.Errorf("%w: %s: the tree context cannot switch stages (use BehaviorContext)", ErrTreeInvalid, path)
		}
		p.stageRefs = append(p.stageRefs, stageRef{path: path, stage: raw.Stage})
		return &SetStage[C]{Stage: raw.Stage}, nil
	case "condition":
		var raw struct {
			Node string          `json:"node"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		build, known := registry.conditions[raw.Name]
		if !known {
			return nil, fmt.Errorf("%w: %s: unknown condition %q", ErrTreeInvalid, path, raw.Name)
		}
		node, err := build(raw.Args)
		if err != nil {
			return nil, fmt.Errorf("%w: %s (%q): %v", ErrTreeInvalid, path, raw.Name, err)
		}
		return node, nil
	case "action":
		var raw struct {
			Node string          `json:"node"`
			Kind string          `json:"kind"`
			Args json.RawMessage `json:"args"`
		}
		if err := decodeStrictJSON(data, &raw); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		build, known := registry.actions[raw.Kind]
		if !known {
			return nil, fmt.Errorf("%w: %s: unknown action %q", ErrTreeInvalid, path, raw.Kind)
		}
		node, err := build(raw.Args)
		if err != nil {
			return nil, fmt.Errorf("%w: %s (%q): %v", ErrTreeInvalid, path, raw.Kind, err)
		}
		return node, nil
	case "":
		return nil, fmt.Errorf("%w: %s: missing \"node\"", ErrTreeInvalid, path)
	default:
		return nil, fmt.Errorf("%w: %s: unknown node %q", ErrTreeInvalid, path, header.Node)
	}
}

func (p *treeParser[C]) children(data []byte, path string, depth int) ([]Node[C], error) {
	var raw struct {
		Node     string            `json:"node"`
		Children []json.RawMessage `json:"children"`
	}
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	return p.childList(raw.Children, path, depth)
}

func (p *treeParser[C]) childList(children []json.RawMessage, path string, depth int) ([]Node[C], error) {
	if len(children) == 0 {
		return nil, fmt.Errorf("%w: %s.children must not be empty", ErrTreeInvalid, path)
	}
	result := make([]Node[C], 0, len(children))
	for index, child := range children {
		node, err := p.node(child, fmt.Sprintf("%s.children[%d]", path, index), depth+1)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, nil
}

func (p *treeParser[C]) singleChild(data []byte, path string, depth int) (Node[C], error) {
	var raw struct {
		Node  string          `json:"node"`
		Child json.RawMessage `json:"child"`
	}
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	return p.requireChild(raw.Child, path, depth)
}

func (p *treeParser[C]) requireChild(child json.RawMessage, path string, depth int) (Node[C], error) {
	if len(child) == 0 {
		return nil, fmt.Errorf("%w: %s.child is required", ErrTreeInvalid, path)
	}
	return p.node(child, path+".child", depth+1)
}

// validatePredicate 把 guard 谓词子树限制为无状态形状：经 sequence（与）、selector（或）与 inverter（非）组合的
// 条件叶子。
func validatePredicate(data []byte, path string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: %s exceeds depth %d", ErrTreeInvalid, path, maxTreeDepth)
	}
	var header struct {
		Node     string            `json:"node"`
		Children []json.RawMessage `json:"children"`
		Child    json.RawMessage   `json:"child"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	switch header.Node {
	case "condition":
		return nil
	case "event":
		var raw struct {
			Take bool `json:"take"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
		}
		if raw.Take {
			return fmt.Errorf("%w: %s: an event leaf that takes the event is not a valid guard predicate", ErrTreeInvalid, path)
		}
		return nil
	case "sequence", "selector":
		for index, child := range header.Children {
			if err := validatePredicate(child, fmt.Sprintf("%s.children[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
		return nil
	case "inverter":
		if len(header.Child) == 0 {
			return fmt.Errorf("%w: %s.child is required", ErrTreeInvalid, path)
		}
		return validatePredicate(header.Child, path+".child", depth+1)
	default:
		return fmt.Errorf("%w: %s: node %q is not a valid guard predicate (allowed: condition, event without take, sequence, selector, inverter)", ErrTreeInvalid, path, header.Node)
	}
}

// decodeStrictJSON 解码一个 JSON 值，拒绝未知字段与尾随数据。
func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after document")
	}
	return nil
}
