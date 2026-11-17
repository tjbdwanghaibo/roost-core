package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is the optional declarative lane: a scenario described in YAML,
// interpreted at runtime (no compile step), so load-test scripts can be
// reloaded on the fly. The vocabulary is deliberately tiny — exactly the
// code combinators, with params passed through opaquely as map[string]any.
// Anything beyond orchestration belongs in a Go action.
//
//	scenarios:
//	  - name: full_play
//	    node:
//	      sequence:
//	        - action: connect
//	        - action: login
//	          param: {wait_ready: true}
//	        - wait: 1s
//	        - loop:
//	            times: 0
//	            node: {action: ping}
//	        - retry: {times: 3, node: {action: buy}}
//	        - timeout: {duration: 30s, node: {action: raid}}
//	        - best_effort: {action: chat}
//	        - selector: [{action: attack}, {action: retreat}]
//	        - random:
//	            - {weight: 7, node: {action: farm}}
//	            - {weight: 3, node: {action: pvp}}
type Spec struct {
	Scenarios []ScenarioSpec `yaml:"scenarios"`
}

type ScenarioSpec struct {
	Name string    `yaml:"name"`
	Node *NodeSpec `yaml:"node"`
}

// NodeSpec is one node in the declarative tree; exactly one field may be
// set.
type NodeSpec struct {
	Action     string          `yaml:"action"`
	Param      map[string]any  `yaml:"param"`
	Wait       string          `yaml:"wait"`
	Sequence   []*NodeSpec     `yaml:"sequence"`
	Selector   []*NodeSpec     `yaml:"selector"`
	Parallel   []*NodeSpec     `yaml:"parallel"`
	BestEffort *NodeSpec       `yaml:"best_effort"`
	Loop       *LoopSpec       `yaml:"loop"`
	Retry      *RetrySpec      `yaml:"retry"`
	Timeout    *TimeoutSpec    `yaml:"timeout"`
	Random     []*WeightedSpec `yaml:"random"`
}

type LoopSpec struct {
	Times int       `yaml:"times"`
	Node  *NodeSpec `yaml:"node"`
}

type RetrySpec struct {
	Times int       `yaml:"times"`
	Node  *NodeSpec `yaml:"node"`
}

type TimeoutSpec struct {
	Duration string    `yaml:"duration"`
	Node     *NodeSpec `yaml:"node"`
}

type WeightedSpec struct {
	Weight int       `yaml:"weight"`
	Node   *NodeSpec `yaml:"node"`
}

// ParseSpec interprets a YAML document into scenarios. Unknown YAML keys
// are rejected (typos fail loudly), and every scenario in the document must
// build for any to be returned.
func ParseSpec(raw []byte) ([]Scenario, error) {
	var spec Spec
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("robot scenario: parse spec: %w", err)
	}
	if len(spec.Scenarios) == 0 {
		return nil, errors.New("robot scenario: spec has no scenarios")
	}
	scenarios := make([]Scenario, 0, len(spec.Scenarios))
	seen := make(map[string]bool, len(spec.Scenarios))
	for _, entry := range spec.Scenarios {
		name := normalizeName(entry.Name)
		if name == "" {
			return nil, errors.New("robot scenario: spec scenario without a name")
		}
		if seen[name] {
			return nil, fmt.Errorf("robot scenario: duplicate spec scenario %q", name)
		}
		seen[name] = true
		node, err := buildTimedRoot(entry.Node, "$."+name)
		if err != nil {
			return nil, err
		}
		scenarios = append(scenarios, New(name, node))
	}
	return scenarios, nil
}

// RegisterSpec parses raw and registers every scenario it defines.
func RegisterSpec(r *Registry, raw []byte) error {
	scenarios, err := ParseSpec(raw)
	if err != nil {
		return err
	}
	for _, s := range scenarios {
		if err := r.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// buildTimedRoot 建出场景的根，并给每个顶层步骤套上 Step 计时：根是 sequence 时每个元素一步，
// 否则整棵树算一步。步骤名是两位序号加子树摘要（如 "08 retry(wait 250ms,mailbox_list)"），
// 不用在 YAML 里起名；序号让同样内容的两步分得开，也让报告按名字排序就是场景顺序。
func buildTimedRoot(spec *NodeSpec, path string) (Node, error) {
	root, err := buildNode(spec, path)
	if err != nil {
		return nil, err
	}
	if spec.Sequence == nil {
		return Step("01 "+describeNode(spec), root), nil
	}
	// 根已通过校验；这里按同样的规则再建一次各元素，好在外面各套一层 Step。
	steps := make([]Node, 0, len(spec.Sequence))
	for i, item := range spec.Sequence {
		node, err := buildNode(item, fmt.Sprintf("%s.sequence[%d]", path, i))
		if err != nil {
			return nil, err
		}
		steps = append(steps, Step(fmt.Sprintf("%02d %s", i+1, describeNode(item)), node))
	}
	return Sequence(steps...), nil
}

// describeNode 给子树一个简短摘要：动作名、"wait 时长"，组合节点用 kind(子节点…) 包起来。
func describeNode(spec *NodeSpec) string {
	if spec == nil {
		return ""
	}
	list := func(items []*NodeSpec, sep string) string {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, describeNode(item))
		}
		return strings.Join(parts, sep)
	}
	switch {
	case spec.Action != "":
		return spec.Action
	case spec.Wait != "":
		return "wait " + spec.Wait
	case spec.Sequence != nil:
		return list(spec.Sequence, ",")
	case spec.Selector != nil:
		return "selector(" + list(spec.Selector, "|") + ")"
	case spec.Parallel != nil:
		return "parallel(" + list(spec.Parallel, "&") + ")"
	case spec.BestEffort != nil:
		return "best_effort(" + describeNode(spec.BestEffort) + ")"
	case spec.Loop != nil:
		return fmt.Sprintf("loop %d(%s)", spec.Loop.Times, describeNode(spec.Loop.Node))
	case spec.Retry != nil:
		return "retry(" + describeNode(spec.Retry.Node) + ")"
	case spec.Timeout != nil:
		return "timeout " + spec.Timeout.Duration + "(" + describeNode(spec.Timeout.Node) + ")"
	case spec.Random != nil:
		parts := make([]string, 0, len(spec.Random))
		for _, item := range spec.Random {
			if item != nil {
				parts = append(parts, describeNode(item.Node))
			}
		}
		return "random(" + strings.Join(parts, "|") + ")"
	}
	return ""
}

func buildNode(spec *NodeSpec, path string) (Node, error) {
	if spec == nil {
		return nil, fmt.Errorf("robot scenario: %s: node is required", path)
	}
	set := 0
	var build func() (Node, error)
	claim := func(fn func() (Node, error)) {
		set++
		build = fn
	}
	if spec.Action != "" {
		claim(func() (Node, error) { return Action(spec.Action, anyParam(spec.Param)), nil })
	}
	if spec.Wait != "" {
		claim(func() (Node, error) {
			d, err := time.ParseDuration(spec.Wait)
			if err != nil {
				return nil, fmt.Errorf("robot scenario: %s: wait: %w", path, err)
			}
			return Wait(d), nil
		})
	}
	if spec.Sequence != nil {
		claim(func() (Node, error) { return buildList(spec.Sequence, path+".sequence", Sequence) })
	}
	if spec.Selector != nil {
		claim(func() (Node, error) { return buildList(spec.Selector, path+".selector", Selector) })
	}
	if spec.Parallel != nil {
		claim(func() (Node, error) { return buildList(spec.Parallel, path+".parallel", Parallel) })
	}
	if spec.BestEffort != nil {
		claim(func() (Node, error) {
			inner, err := buildNode(spec.BestEffort, path+".best_effort")
			if err != nil {
				return nil, err
			}
			return BestEffort(inner), nil
		})
	}
	if spec.Loop != nil {
		claim(func() (Node, error) {
			inner, err := buildNode(spec.Loop.Node, path+".loop")
			if err != nil {
				return nil, err
			}
			return Loop(spec.Loop.Times, inner), nil
		})
	}
	if spec.Retry != nil {
		claim(func() (Node, error) {
			inner, err := buildNode(spec.Retry.Node, path+".retry")
			if err != nil {
				return nil, err
			}
			return Retry(spec.Retry.Times, inner), nil
		})
	}
	if spec.Timeout != nil {
		claim(func() (Node, error) {
			d, err := time.ParseDuration(spec.Timeout.Duration)
			if err != nil {
				return nil, fmt.Errorf("robot scenario: %s: timeout: %w", path, err)
			}
			inner, err := buildNode(spec.Timeout.Node, path+".timeout")
			if err != nil {
				return nil, err
			}
			return Timeout(d, inner), nil
		})
	}
	if spec.Random != nil {
		claim(func() (Node, error) {
			weighted := make([]WeightedNode, 0, len(spec.Random))
			for i, item := range spec.Random {
				if item == nil {
					continue
				}
				inner, err := buildNode(item.Node, fmt.Sprintf("%s.random[%d]", path, i))
				if err != nil {
					return nil, err
				}
				weighted = append(weighted, Weighted(item.Weight, inner))
			}
			return Random(weighted...), nil
		})
	}
	if set == 0 {
		return nil, fmt.Errorf("robot scenario: %s: empty node", path)
	}
	if set > 1 {
		return nil, fmt.Errorf("robot scenario: %s: exactly one node kind per entry (got %d)", path, set)
	}
	return build()
}

func buildList(specs []*NodeSpec, path string, combine func(...Node) Node) (Node, error) {
	nodes := make([]Node, 0, len(specs))
	for i, item := range specs {
		node, err := buildNode(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return combine(nodes...), nil
}

// anyParam keeps a nil map nil so actions see the same shape as code-built
// trees.
func anyParam(param map[string]any) any {
	if param == nil {
		return nil
	}
	return param
}
