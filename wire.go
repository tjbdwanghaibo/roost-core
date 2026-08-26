package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// TreeSchema is the wire schema identifier for JSON behavior trees.
const TreeSchema = "cube.ai/v1"

// maxTreeDepth bounds recursion so a hostile or broken document cannot blow
// the stack.
const maxTreeDepth = 64

// Wire errors.
var (
	ErrTreeSchema  = errors.New("ai: unsupported tree schema")
	ErrTreeInvalid = errors.New("ai: invalid tree document")
)

// Registry supplies the leaf vocabulary for JSON tree assembly. Composite
// and decorator nodes (sequence, selector, parallel, inverter, succeeder,
// until_success, repeat, cooldown, time_limit, guard) are built in; the
// business registers only its condition and action leaves. Factories receive
// the leaf's raw "args" and must parse them strictly themselves.
type Registry[C any] struct {
	// now feeds the built-in time nodes (cooldown/time_limit); required if a
	// document uses them.
	now        func(*C) int64
	conditions map[string]func(args json.RawMessage) (Node[C], error)
	actions    map[string]func(args json.RawMessage) (Node[C], error)
}

func NewRegistry[C any](now func(*C) int64) *Registry[C] {
	return &Registry[C]{
		now:        now,
		conditions: make(map[string]func(args json.RawMessage) (Node[C], error)),
		actions:    make(map[string]func(args json.RawMessage) (Node[C], error)),
	}
}

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

// ParseTree assembles a behavior tree from its JSON document. Parsing is
// strict in the skillv2 wire tradition: unknown fields, unknown node names,
// arity violations, and depth blowups are all compile-time rejections that
// name the offending JSON path — a broken document never becomes a silently
// wrong tree. The returned tree is freshly stateful and ready for
// NewBehaviorStrategy (a failed parse leaves any currently installed
// strategy untouched, which Controller.SetStrategy already guarantees).
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
		predicate, err := parseNode(raw.Condition, registry, path+".condition", depth+1)
		if err != nil {
			return nil, err
		}
		child, err := requireChild(raw.Child, registry, path, depth)
		if err != nil {
			return nil, err
		}
		// A guard predicate must be stateless; the condition leaf contract
		// satisfies that, so the guard simply asks it for Success.
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

// decodeStrictJSON decodes one JSON value rejecting unknown fields and
// trailing data.
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
