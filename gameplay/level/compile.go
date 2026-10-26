package level

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Program 拥有定义与类型表的深副本；按事件类型索引，多个实体可共享只读 Program。
type Program struct {
	definition Definition
	registry   Registry
	hash       string
	stages     map[string]StageDefinition
	variables  map[string]Variable
	index      map[string]map[string][]TriggerDefinition
}

func (p *Program) Hash() string { return p.hash }
func DecodeDefinition(raw []byte) (Definition, error) {
	var d Definition
	if len(raw) > 1<<20 {
		return d, ErrBudget
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return d, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return d, ErrDefinition
	}
	return d, nil
}
func identifier(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func eventName(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !identifier(part) {
			return false
		}
	}
	return true
}
func variableKey(scope Scope, name string) string { return string(scope) + "/" + name }
func Compile(def Definition, registry Registry) (*Program, error) {
	raw, err := json.Marshal(struct {
		Definition Definition
		Registry   Registry
	}{def, registry})
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrDefinition
	}
	var owned struct {
		Definition Definition
		Registry   Registry
	}
	if err = json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	def, registry = owned.Definition, owned.Registry
	if def.Limits == (Limits{}) {
		def.Limits = DefaultLimits()
	}
	limits := def.Limits
	if def.Version != 1 || !identifier(def.Name) || def.Revision == 0 || (def.TimeDomain != "tick" && def.TimeDomain != "millisecond") || def.PrepareDelay < 0 || def.CloseDelay < 0 || len(def.Stages) == 0 || len(def.Stages) > 32 || len(def.Initial) == 0 || len(def.Initial) > 32 || len(def.Variables) > 128 || limits.Steps < 1 || limits.Steps > 4096 || limits.Timers < 1 || limits.Timers > 128 || limits.Effects < 1 || limits.Effects > 4096 || limits.Sources < 1 || limits.Sources > 64 {
		return nil, ErrDefinition
	}
	if registry.Events == nil {
		registry.Events = map[string]map[string]ValueKind{}
	}
	builtins := map[string]map[string]ValueKind{EventLevelStart: {}, EventStageStart: {}, EventStageEnd: {}, EventTimer: {"name": Text}, EventActionDone: {"id": Text, "name": Text, "success": Boolean}}
	for name, schema := range registry.Events {
		if !eventName(name) || isInternal(name) {
			return nil, ErrDefinition
		}
		if err = validateSchema(schema); err != nil {
			return nil, err
		}
	}
	for name, spec := range registry.Actions {
		if !identifier(name) || spec.Version == 0 {
			return nil, ErrDefinition
		}
		if err = validateSchema(spec.Args); err != nil {
			return nil, err
		}
	}
	for name, schema := range builtins {
		registry.Events[name] = schema
	}
	p := &Program{definition: def, registry: registry, stages: make(map[string]StageDefinition), variables: make(map[string]Variable), index: make(map[string]map[string][]TriggerDefinition)}
	for _, v := range def.Variables {
		key := variableKey(v.Scope, v.Name)
		if !identifier(v.Name) || !v.Default.valid() || (v.Scope != Global && v.Scope != Stage && v.Scope != Trigger) {
			return nil, ErrDefinition
		}
		if _, ok := p.variables[key]; ok {
			return nil, ErrDefinition
		}
		p.variables[key] = v
	}
	for _, stage := range def.Stages {
		if !identifier(stage.ID) || len(stage.Triggers) > 64 {
			return nil, ErrDefinition
		}
		if _, ok := p.stages[stage.ID]; ok {
			return nil, ErrDefinition
		}
		p.stages[stage.ID] = stage
	}
	// 默认值投影也占预算；避免少量声明展开成几十万个 Stage/Trigger 变量。
	variableSlots := 0
	for _, v := range def.Variables {
		switch v.Scope {
		case Global:
			variableSlots++
		case Stage:
			variableSlots += len(def.Stages)
		case Trigger:
			for _, stage := range def.Stages {
				variableSlots += len(stage.Triggers)
			}
		}
	}
	if variableSlots > 8192 {
		return nil, ErrBudget
	}
	initial := map[string]bool{}
	for _, id := range def.Initial {
		if _, ok := p.stages[id]; !ok || initial[id] {
			return nil, ErrDefinition
		}
		initial[id] = true
	}
	for _, stage := range def.Stages {
		p.index[stage.ID] = make(map[string][]TriggerDefinition)
		seen := map[string]bool{}
		for _, t := range stage.Triggers {
			schema, exists := registry.Events[t.Event]
			if !identifier(t.ID) || seen[t.ID] || !exists || len(t.Conditions) > 32 || len(t.Actions) == 0 || len(t.Actions) > 64 || t.Cooldown < 0 {
				return nil, ErrDefinition
			}
			seen[t.ID] = true
			for _, condition := range t.Conditions {
				a, e := p.exprType(condition.Left, schema)
				if e != nil {
					return nil, e
				}
				b, e := p.exprType(condition.Right, schema)
				if e != nil || a != b {
					return nil, ErrDefinition
				}
				switch condition.Op {
				case "eq", "ne":
				case "lt", "le", "gt", "ge":
					if a != Integer {
						return nil, ErrDefinition
					}
				default:
					return nil, ErrDefinition
				}
			}
			for _, action := range t.Actions {
				if err = p.validateAction(action, schema); err != nil {
					return nil, fmt.Errorf("%w: stage %s trigger %s action %s", err, stage.ID, t.ID, action.Kind)
				}
			}
			p.index[stage.ID][t.Event] = append(p.index[stage.ID][t.Event], t)
		}
	}
	canonical, _ := json.Marshal(struct {
		Definition Definition
		Registry   Registry
	}{def, registry})
	sum := sha256.Sum256(canonical)
	p.hash = hex.EncodeToString(sum[:])
	return p, nil
}
func isInternal(name string) bool {
	return name == EventLevelStart || name == EventStageStart || name == EventStageEnd || name == EventTimer || name == EventActionDone
}
func validateSchema(schema map[string]ValueKind) error {
	if len(schema) > 32 {
		return ErrDefinition
	}
	for name, kind := range schema {
		if !identifier(name) || (kind != Integer && kind != Text && kind != Boolean) {
			return ErrDefinition
		}
	}
	return nil
}
func (p *Program) exprType(e Expr, schema map[string]ValueKind) (ValueKind, error) {
	count := 0
	var kind ValueKind
	if e.Literal != nil {
		count++
		if !e.Literal.valid() {
			return "", ErrDefinition
		}
		kind = e.Literal.Kind
	}
	if e.Variable != nil {
		count++
		v, ok := p.variables[variableKey(e.Variable.Scope, e.Variable.Name)]
		if !ok {
			return "", ErrDefinition
		}
		kind = v.Default.Kind
	}
	if e.EventField != "" {
		count++
		v, ok := schema[e.EventField]
		if !ok {
			return "", ErrDefinition
		}
		kind = v
	}
	if count != 1 {
		return "", ErrDefinition
	}
	return kind, nil
}
func (p *Program) validateAction(a Action, schema map[string]ValueKind) error {
	// 各动作只接受自己的字段，配置拼错不能被静默忽略。
	if a.Kind != "set" && a.Kind != "add" && (a.Scope != "" || a.Value != (Expr{})) ||
		a.Kind != "timer" && a.Delay != 0 ||
		a.Kind != "request" && (len(a.Args) != 0 || a.LevelLifetime) ||
		a.Kind != "finish" && a.Result != "" || a.Kind == "finish" && a.Name != "" {
		return ErrDefinition
	}
	switch a.Kind {
	case "set", "add":
		v, ok := p.variables[variableKey(a.Scope, a.Name)]
		if !ok {
			return ErrDefinition
		}
		kind, err := p.exprType(a.Value, schema)
		if err != nil || kind != v.Default.Kind || a.Kind == "add" && kind != Integer {
			return ErrDefinition
		}
	case "start", "end":
		if _, ok := p.stages[a.Name]; !ok {
			return ErrDefinition
		}
	case "timer":
		if !identifier(a.Name) || a.Delay <= 0 {
			return ErrDefinition
		}
	case "cancel_timer":
		if !identifier(a.Name) {
			return ErrDefinition
		}
	case "request":
		spec, ok := p.registry.Actions[a.Name]
		if !ok || len(spec.Args) != len(a.Args) {
			return ErrDefinition
		}
		for name, e := range a.Args {
			kind, err := p.exprType(e, schema)
			if err != nil || kind != spec.Args[name] {
				return ErrDefinition
			}
		}
	case "finish":
		if !identifier(a.Result) {
			return ErrDefinition
		}
	default:
		return ErrDefinition
	}
	return nil
}
