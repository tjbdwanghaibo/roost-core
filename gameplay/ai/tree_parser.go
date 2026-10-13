package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	now        func(*C) int64
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
		return nil, fmt.Errorf("%w: $.root is required", ErrTreeInvalid)
	}
	return parseNode(document.Root, registry, "$.root", 0)
}

func parseNode[C any](data []byte, registry *Registry[C], path string, depth int) (Node[C], error) {
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
		children, err := parseChildren(data, registry, path, depth)
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
		children, err := parseChildList(raw.Children, registry, path, depth)
		if err != nil {
			return nil, err
		}
		return &Parallel[C]{Children: children, Policy: policy}, nil
	case "inverter", "succeeder", "until_success":
		child, err := parseSingleChild(data, registry, path, depth)
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
		child, err := requireChild(raw.Child, registry, path, depth)
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
		child, err := requireChild(raw.Child, registry, path, depth)
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
		predicate, err := parseNode(raw.Condition, registry, path+".condition", depth+1)
		if err != nil {
			return nil, err
		}
		child, err := requireChild(raw.Child, registry, path, depth)
		if err != nil {
			return nil, err
		}
		// guard 谓词必须无状态；条件叶子的契约满足这一点，所以 guard 只需问它是否 Success。
		return &Guard[C]{Check: func(ctx *C) bool { return predicate.Tick(ctx) == StatusSuccess }, Child: child}, nil
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

func parseChildren[C any](data []byte, registry *Registry[C], path string, depth int) ([]Node[C], error) {
	var raw struct {
		Node     string            `json:"node"`
		Children []json.RawMessage `json:"children"`
	}
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	return parseChildList(raw.Children, registry, path, depth)
}

func parseChildList[C any](children []json.RawMessage, registry *Registry[C], path string, depth int) ([]Node[C], error) {
	if len(children) == 0 {
		return nil, fmt.Errorf("%w: %s.children must not be empty", ErrTreeInvalid, path)
	}
	result := make([]Node[C], 0, len(children))
	for index, child := range children {
		node, err := parseNode(child, registry, fmt.Sprintf("%s.children[%d]", path, index), depth+1)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, nil
}

func parseSingleChild[C any](data []byte, registry *Registry[C], path string, depth int) (Node[C], error) {
	var raw struct {
		Node  string          `json:"node"`
		Child json.RawMessage `json:"child"`
	}
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTreeInvalid, path, err)
	}
	return requireChild(raw.Child, registry, path, depth)
}

func requireChild[C any](child json.RawMessage, registry *Registry[C], path string, depth int) (Node[C], error) {
	if len(child) == 0 {
		return nil, fmt.Errorf("%w: %s.child is required", ErrTreeInvalid, path)
	}
	return parseNode(child, registry, path+".child", depth+1)
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
		return fmt.Errorf("%w: %s: node %q is not a valid guard predicate (allowed: condition, sequence, selector, inverter)", ErrTreeInvalid, path, header.Node)
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
