// Package level 提供实体拥有的通用关卡组件：多 Stage、事件/条件/动作和事务状态。
// 所有写操作在所属实体 Nest Guard 内执行，外部动作只形成请求，不在事务中执行 I/O。
package level

import "errors"

var (
	ErrDefinition = errors.New("level: invalid definition or unsupported capability")
	ErrState      = errors.New("level: invalid state or transition")
	ErrStale      = errors.New("level: stale instance, stage or source sequence")
	ErrConflict   = errors.New("level: conflicting retry")
	ErrBudget     = errors.New("level: execution or storage budget exceeded")
)

type ValueKind string

const (
	Integer ValueKind = "int"
	Text    ValueKind = "string"
	Boolean ValueKind = "bool"
)

type Value struct {
	Kind ValueKind
	Int  int64
	Text string
	Bool bool
}

func Int(v int64) Value     { return Value{Kind: Integer, Int: v} }
func String(v string) Value { return Value{Kind: Text, Text: v} }
func Bool(v bool) Value     { return Value{Kind: Boolean, Bool: v} }
func (v Value) valid() bool {
	switch v.Kind {
	case Integer:
		return v.Text == "" && !v.Bool
	case Text:
		return len(v.Text) <= 1024 && v.Int == 0 && !v.Bool
	case Boolean:
		return v.Int == 0 && v.Text == ""
	}
	return false
}

type Scope string

const (
	Global  Scope = "global"
	Stage   Scope = "stage"
	Trigger Scope = "trigger"
)

type Variable struct {
	Scope   Scope
	Name    string
	Default Value
	Public  bool
}
type Reference struct {
	Scope Scope
	Name  string
}

// Expr 只允许一个来源；事件字段的类型由 Registry.Events 声明。
type Expr struct {
	Literal    *Value
	Variable   *Reference
	EventField string
}

func Literal(v Value) Expr              { return Expr{Literal: &v} }
func Var(scope Scope, name string) Expr { return Expr{Variable: &Reference{scope, name}} }
func Field(name string) Expr            { return Expr{EventField: name} }

type Condition struct {
	Left  Expr
	Op    string
	Right Expr
}
type Action struct {
	Kind          string // set/add/start/end/timer/cancel_timer/request/finish
	Scope         Scope
	Name          string // 变量、目标 Stage、定时器或外部能力名
	Value         Expr
	Delay         int64
	Args          map[string]Expr
	LevelLifetime bool   // request=true 时阶段结束后仍保留；用于结算等跨阶段动作。
	Result        string // finish 的不可变终局结果标识
}
type TriggerDefinition struct {
	ID         string
	Event      string
	Conditions []Condition
	Actions    []Action
	MaxFires   uint32
	Cooldown   int64
}
type StageDefinition struct {
	ID       string
	Triggers []TriggerDefinition
}
type Limits struct{ Steps, Timers, Effects, Sources int }

func DefaultLimits() Limits { return Limits{Steps: 512, Timers: 128, Effects: 1024, Sources: 64} }

type Definition struct {
	Version                  int
	Name                     string
	Revision                 uint32
	TimeDomain               string // tick 或 millisecond；now 必须由可信宿主提供，不能混用墙钟和模拟时间。
	PrepareDelay, CloseDelay int64
	Initial                  []string
	Variables                []Variable
	Stages                   []StageDefinition
	Limits                   Limits
}
type ActionSpec struct {
	Version uint32
	Args    map[string]ValueKind
}

// Registry 只声明宿主确实能执行的动作/事件及其类型；不携带函数、不动态执行用户代码。
type Registry struct {
	Events  map[string]map[string]ValueKind
	Actions map[string]ActionSpec
}

const (
	EventLevelStart = "level.start"
	EventStageStart = "stage.start"
	EventStageEnd   = "stage.end"
	EventTimer      = "timer"
	EventActionDone = "action.done"
)
