package level

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// LevelComponent 仅持有装配接线；状态全部归 DAO，外部动作不得在 Guard 内执行。
type LevelComponent struct {
	entity.ComponentBase
	dao     *LevelDAO
	program *Program
}

func NewLevelComponent(dao *LevelDAO, program *Program) (*LevelComponent, error) {
	if dao == nil || program == nil {
		return nil, ErrState
	}
	c := &LevelComponent{dao: dao, program: program}
	if err := c.OnInitFinish(nil, false); err != nil {
		return nil, err
	}
	return c, nil
}
func (*LevelComponent) Name() string { return "level" }
func (c *LevelComponent) OnInitFinish(*entity.EntityCreateParam, bool) error {
	s := c.dao.state
	if err := s.validate(); err != nil {
		return err
	}
	return s.validateProgram(c.program)
}
func (c *LevelComponent) Snapshot() State { return cloneState(c.dao.state) }
func (c *LevelComponent) mutate(now int64, fn func(*runtime) error) error {
	if nest.CurrentRollbackTx() == nil {
		return nest.ErrTransactionClosed
	}
	if c.dao.state.Instance != "" && now < c.dao.state.Now || now < 0 {
		return ErrState
	}
	draft := cloneState(c.dao.state)
	draft.Now = now
	r := &runtime{program: c.program, state: &draft}
	if err := fn(r); err != nil {
		return err
	}
	if err := r.drain(); err != nil {
		return err
	}
	if draft.Revision == math.MaxUint64 {
		return ErrBudget
	}
	draft.Revision++
	if err := draft.validate(); err != nil {
		return err
	}
	return c.dao.replace(draft)
}
func (c *LevelComponent) Initialize(instance string, epoch uint64, now int64) error {
	return c.mutate(now, func(r *runtime) error {
		if r.state.Instance != "" || !identifier(instance) || epoch == 0 {
			return ErrState
		}
		*r.state = newState(instance, epoch, c.program.hash, now)
		return nil
	})
}
func (c *LevelComponent) Start(now int64) error {
	return c.mutate(now, func(r *runtime) error {
		if r.state.Phase != Waiting {
			return ErrState
		}
		due, err := deadline(now, r.program.definition.PrepareDelay)
		if err != nil {
			return err
		}
		r.state.Phase = Preparing
		r.state.Deadline = due
		return r.advance()
	})
}
func (c *LevelComponent) Advance(now int64) error {
	if nest.CurrentRollbackTx() == nil {
		return nest.ErrTransactionClosed
	}
	if c.dao.state.Instance == "" || now < c.dao.state.Now || now < 0 {
		return ErrState
	}
	if due, ok := c.NextDeadline(); !ok || now < due {
		return nil
	}
	return c.mutate(now, func(r *runtime) error {
		if r.state.Instance == "" {
			return ErrState
		}
		return r.advance()
	})
}

// Handle 只接服务器校验后的事实；同 source 要按序投递，旧序列拒绝，同序列异内容冲突。
func (c *LevelComponent) Handle(event Event, now int64) error {
	s := c.dao.state
	if nest.CurrentRollbackTx() == nil {
		return nest.ErrTransactionClosed
	}
	if event.Instance != s.Instance || event.Epoch != s.Epoch {
		return ErrStale
	}
	if !identifier(event.Source) || event.Sequence == 0 || isInternal(event.Kind) {
		return ErrState
	}
	schema, ok := c.program.registry.Events[event.Kind]
	if !ok || !matches(schema, event.Fields) {
		return ErrDefinition
	}
	raw, _ := json.Marshal(event)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	cursor := s.Sources[event.Source]
	if event.Sequence < cursor.Sequence {
		return ErrStale
	}
	if event.Sequence == cursor.Sequence {
		if cursor.Digest == digest {
			return nil
		}
		return ErrConflict
	}
	return c.mutate(now, func(r *runtime) error {
		if r.state.Phase != Running {
			return ErrState
		}
		if event.Stage != "" {
			stage, ok := r.state.Active[event.Stage]
			if !ok || stage.Generation != event.Generation {
				return ErrStale
			}
		} else if event.Generation != 0 {
			return ErrState
		}
		if _, ok := r.state.Sources[event.Source]; !ok && len(r.state.Sources) >= r.program.definition.Limits.Sources {
			return ErrBudget
		}
		r.state.Sources[event.Source] = SourceCursor{event.Sequence, digest}
		r.queue = append(r.queue, work{event: event})
		return nil
	})
}

// PendingActions 返回副本；宿主须通过 RequestSettled 取得已提交结果，再以 ID 幂等执行。
func (c *LevelComponent) PendingActions() []Effect {
	out := []Effect{}
	for _, e := range c.dao.state.Effects {
		if e.Status == "pending" {
			e.Args = maps.Clone(e.Args)
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Effect) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	return out
}
func (c *LevelComponent) Complete(receipt Receipt, now int64) error {
	if nest.CurrentRollbackTx() == nil {
		return nest.ErrTransactionClosed
	}
	s := c.dao.state
	if receipt.Instance != s.Instance || receipt.Epoch != s.Epoch {
		return ErrStale
	}
	e, ok := s.Effects[receipt.ID]
	if !ok {
		return ErrStale
	}
	if e.Status == "cancelled" {
		return ErrStale
	}
	if e.Status == "done" {
		if e.Success == receipt.Success {
			return nil
		}
		return ErrConflict
	}
	return c.mutate(now, func(r *runtime) error {
		e.Status = "done"
		e.Success = receipt.Success
		r.state.Effects[e.ID] = e
		if stage, ok := r.state.Active[e.Stage]; ok && stage.Generation == e.Generation {
			r.queue = append(r.queue, work{event: Event{Kind: EventActionDone, Stage: e.Stage, Generation: e.Generation, Fields: map[string]Value{"id": String(e.ID), "name": String(e.Name), "success": Bool(e.Success)}}})
		}
		return nil
	})
}
func (c *LevelComponent) Close(now int64) error {
	return c.mutate(now, func(r *runtime) error {
		if r.state.Phase != Finishing {
			return ErrState
		}
		for _, e := range r.state.Effects {
			if e.Status == "pending" {
				return ErrState
			}
		}
		due, err := deadline(now, r.program.definition.CloseDelay)
		if err != nil {
			return err
		}
		r.state.Phase = Closing
		r.state.Deadline = due
		return r.advance()
	})
}
func matches(schema map[string]ValueKind, values map[string]Value) bool {
	if len(schema) != len(values) {
		return false
	}
	for key, kind := range schema {
		v, ok := values[key]
		if !ok || v.Kind != kind || !v.valid() {
			return false
		}
	}
	return true
}
func deadline(now, delay int64) (int64, error) {
	if delay < 0 || now > math.MaxInt64-delay {
		return 0, ErrBudget
	}
	return now + delay, nil
}
func effectID(instance string, epoch, sequence uint64) string {
	return fmt.Sprintf("%s:%d:%d", instance, epoch, sequence)
}

type work struct {
	op, stage, result string
	generation        uint64
	event             Event
	timer             *Timer
}
type runtime struct {
	program *Program
	state   *State
	queue   []work
	steps   int
}

func (r *runtime) charge() error {
	r.steps++
	if r.steps > r.program.definition.Limits.Steps {
		return ErrBudget
	}
	return nil
}
func (r *runtime) advance() error {
	s := r.state
	if s.Phase == Preparing && s.Now >= s.Deadline {
		s.Phase = Running
		s.Deadline = 0
		for _, id := range r.program.definition.Initial {
			r.queue = append(r.queue, work{op: "start", stage: id})
		}
		r.queue = append(r.queue, work{event: Event{Kind: EventLevelStart}})
	}
	if s.Phase == Closing && s.Now >= s.Deadline {
		s.Phase = Closed
		s.Deadline = 0
	}
	if s.Phase == Running {
		keys := sortedKeys(s.Timers)
		slices.SortStableFunc(keys, func(a, b string) int {
			if s.Timers[a].Due < s.Timers[b].Due {
				return -1
			}
			if s.Timers[a].Due > s.Timers[b].Due {
				return 1
			}
			return 0
		})
		for _, key := range keys {
			timer := s.Timers[key]
			if timer.Due > s.Now {
				continue
			}
			r.queue = append(r.queue, work{timer: &timer, event: Event{Kind: EventTimer, Stage: timer.Stage, Generation: timer.Generation, Fields: map[string]Value{"name": String(timer.Name)}}})
		}
	}
	return nil
}
func (r *runtime) drain() error {
	for len(r.queue) > 0 {
		if err := r.charge(); err != nil {
			return err
		}
		item := r.queue[0]
		r.queue = r.queue[1:]
		switch item.op {
		case "start":
			if r.state.Phase != Running {
				continue
			}
			if _, ok := r.state.Active[item.stage]; ok {
				return ErrState
			}
			generation := r.state.Generations[item.stage] + 1
			if generation == 0 {
				return ErrBudget
			}
			r.state.Generations[item.stage] = generation
			r.state.Active[item.stage] = StageState{Generation: generation, StartedAt: r.state.Now}
			r.queue = append([]work{{event: Event{Kind: EventStageStart, Stage: item.stage, Generation: generation}}}, r.queue...)
		case "end":
			stage, ok := r.state.Active[item.stage]
			if !ok || stage.Exiting {
				continue
			}
			stage.Exiting = true
			r.state.Active[item.stage] = stage
			r.queue = append([]work{{event: Event{Kind: EventStageEnd, Stage: item.stage, Generation: stage.Generation}}, {op: "remove", stage: item.stage, generation: stage.Generation}}, r.queue...)
		case "remove":
			r.removeStage(item.stage, item.generation)
		case "finish":
			if r.state.Phase != Running {
				continue
			}
			r.state.Result = item.result
			r.state.Phase = Finishing
			for name, stage := range r.state.Active {
				r.removeStage(name, stage.Generation)
			}
		default:
			if item.timer != nil {
				key := item.timer.Stage + "/" + item.timer.Name
				current, ok := r.state.Timers[key]
				if !ok || current != *item.timer {
					continue
				}
				delete(r.state.Timers, key)
			}
			if r.state.Phase != Running {
				continue
			}
			if err := r.dispatch(item.event); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *runtime) removeStage(name string, generation uint64) {
	stage, ok := r.state.Active[name]
	if !ok || stage.Generation != generation {
		return
	}
	delete(r.state.Active, name)
	for key, timer := range r.state.Timers {
		if timer.Stage == name && timer.Generation == generation {
			delete(r.state.Timers, key)
		}
	}
	for key := range r.state.Variables {
		if strings.HasPrefix(key, "stage/"+name+"/") || strings.HasPrefix(key, "trigger/"+name+"/") {
			delete(r.state.Variables, key)
		}
	}
	for key := range r.state.Fires {
		if strings.HasPrefix(key, name+"/") {
			delete(r.state.Fires, key)
		}
	}
	for id, e := range r.state.Effects {
		if e.Stage == name && e.Generation == generation && !e.LevelLifetime && e.Status == "pending" {
			e.Status = "cancelled"
			r.state.Effects[id] = e
		}
	}
}
func (r *runtime) dispatch(event Event) error {
	stages := sortedKeys(r.state.Active)
	if event.Stage != "" {
		stages = []string{event.Stage}
	}
	for _, name := range stages {
		stage, ok := r.state.Active[name]
		if !ok || event.Stage != "" && stage.Generation != event.Generation || stage.Exiting && event.Kind != EventStageEnd {
			continue
		}
		for _, t := range r.program.index[name][event.Kind] {
			if err := r.charge(); err != nil {
				return err
			}
			key := name + "/" + t.ID
			fired := r.state.Fires[key]
			if t.MaxFires > 0 && fired.Count >= t.MaxFires || fired.Count > 0 && r.state.Now-fired.LastAt < t.Cooldown {
				continue
			}
			pass := true
			for _, condition := range t.Conditions {
				if err := r.charge(); err != nil {
					return err
				}
				a, err := r.eval(condition.Left, event, name, t.ID)
				if err != nil {
					return err
				}
				b, err := r.eval(condition.Right, event, name, t.ID)
				if err != nil {
					return err
				}
				if !compare(a, b, condition.Op) {
					pass = false
					break
				}
			}
			if !pass {
				continue
			}
			if fired.Count == math.MaxUint32 {
				return ErrBudget
			}
			r.state.Fires[key] = FireState{fired.Count + 1, r.state.Now}
			for _, action := range t.Actions {
				if err := r.charge(); err != nil {
					return err
				}
				if err := r.action(action, event, name, t.ID, stage.Generation); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func storedKey(scope Scope, name, stage, trigger string) string {
	switch scope {
	case Global:
		return "global/" + name
	case Stage:
		return "stage/" + stage + "/" + name
	default:
		return "trigger/" + stage + "/" + trigger + "/" + name
	}
}
func (r *runtime) eval(e Expr, event Event, stage, trigger string) (Value, error) {
	if e.Literal != nil {
		return *e.Literal, nil
	}
	if e.EventField != "" {
		v, ok := event.Fields[e.EventField]
		if !ok {
			return Value{}, ErrState
		}
		return v, nil
	}
	if e.Variable == nil {
		return Value{}, ErrDefinition
	}
	key := storedKey(e.Variable.Scope, e.Variable.Name, stage, trigger)
	if v, ok := r.state.Variables[key]; ok {
		return v, nil
	}
	return r.program.variables[variableKey(e.Variable.Scope, e.Variable.Name)].Default, nil
}
func compare(a, b Value, op string) bool {
	switch op {
	case "eq":
		return a == b
	case "ne":
		return a != b
	case "lt":
		return a.Int < b.Int
	case "le":
		return a.Int <= b.Int
	case "gt":
		return a.Int > b.Int
	case "ge":
		return a.Int >= b.Int
	}
	return false
}
func (r *runtime) action(a Action, event Event, stage, trigger string, generation uint64) error {
	s := r.state
	switch a.Kind {
	case "set", "add":
		value, err := r.eval(a.Value, event, stage, trigger)
		if err != nil {
			return err
		}
		key := storedKey(a.Scope, a.Name, stage, trigger)
		if a.Kind == "add" {
			old, _ := r.eval(Var(a.Scope, a.Name), event, stage, trigger)
			if value.Int > 0 && old.Int > math.MaxInt64-value.Int || value.Int < 0 && old.Int < math.MinInt64-value.Int {
				return ErrBudget
			}
			value.Int += old.Int
		}
		s.Variables[key] = value
	case "start", "end":
		r.queue = append(r.queue, work{op: a.Kind, stage: a.Name})
	case "timer":
		key := stage + "/" + a.Name
		if _, ok := s.Timers[key]; !ok && len(s.Timers) >= r.program.definition.Limits.Timers {
			return ErrBudget
		}
		due, err := deadline(s.Now, a.Delay)
		if err != nil {
			return err
		}
		s.Timers[key] = Timer{Name: a.Name, Stage: stage, Generation: generation, Due: due}
	case "cancel_timer":
		delete(s.Timers, stage+"/"+a.Name)
	case "request":
		if len(s.Effects) >= r.program.definition.Limits.Effects || s.NextEffect == math.MaxUint64 {
			return ErrBudget
		}
		s.NextEffect++
		e := Effect{Instance: s.Instance, Epoch: s.Epoch, Sequence: s.NextEffect, Stage: stage, Generation: generation, Name: a.Name, Args: map[string]Value{}, LevelLifetime: a.LevelLifetime, Status: "pending"}
		e.ID = effectID(s.Instance, s.Epoch, e.Sequence)
		for key, expr := range a.Args {
			v, err := r.eval(expr, event, stage, trigger)
			if err != nil {
				return err
			}
			e.Args[key] = v
		}
		s.Effects[e.ID] = e
	case "finish":
		r.queue = append(r.queue, work{op: "finish", result: a.Result})
	}
	return nil
}
