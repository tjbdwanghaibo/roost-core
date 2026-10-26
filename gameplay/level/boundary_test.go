package level

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"reflect"
	"testing"
	"time"
)

func TestDueTimerCancellationAndRescheduling(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			d := baseDefinition()
			cancel := Action{Kind: "cancel_timer", Name: "b"}
			if replace {
				cancel = Action{Kind: "timer", Name: "b", Delay: 5}
			}
			d.Stages[0].Triggers = []TriggerDefinition{
				{ID: "start", Event: EventStageStart, Actions: []Action{{Kind: "timer", Name: "a", Delay: 10}, {Kind: "timer", Name: "b", Delay: 10}}},
				{ID: "cancel", Event: EventTimer, Conditions: []Condition{{Field("name"), "eq", Literal(String("a"))}}, Actions: []Action{cancel}},
				{ID: "count", Event: EventTimer, Conditions: []Condition{{Field("name"), "eq", Literal(String("b"))}}, Actions: []Action{{Kind: "add", Scope: Global, Name: "count", Value: Literal(Int(1))}}},
			}
			h := newHarness(t, compiled(t, d, baseRegistry()), nest.RollbackUndo, false, nil)
			must(t, h.run(initStart))
			must(t, h.run(func(c *LevelComponent) error { return c.AdvanceTick(10) }))
			if h.c.Project().Variables["global/count"].Int != 0 {
				t.Fatal("cancelled/replaced timer fired")
			}
			if replace {
				must(t, h.run(func(c *LevelComponent) error { return c.AdvanceTick(15) }))
				if h.c.Project().Variables["global/count"].Int != 1 {
					t.Fatal("replacement lost")
				}
			}
		})
	}
}
func TestHeartbeatOneShotAndTimeDomain(t *testing.T) {
	d := baseDefinition()
	d.TimeDomain = "millisecond"
	d.PrepareDelay = 5
	d.Stages[0].Triggers = []TriggerDefinition{
		{ID: "schedule", Event: EventStageStart, Actions: []Action{{Kind: "timer", Name: "repeat", Delay: 10}}},
		{ID: "repeat", Event: EventTimer, Actions: []Action{{Kind: "add", Scope: Global, Name: "count", Value: Literal(Int(1))}, {Kind: "timer", Name: "repeat", Delay: 10}}},
	}
	h := newHarness(t, compiled(t, d, baseRegistry()), nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	if at, ok := h.c.NextWakeup(); !ok || at.UnixMilli() != 5 {
		t.Fatal(at, ok)
	}
	must(t, h.run(func(c *LevelComponent) error { return c.OnBeat(heartbeat.Beat{Now: time.UnixMilli(100)}) }))
	if at, ok := h.c.NextWakeup(); !ok || at.UnixMilli() != 110 {
		t.Fatal(at, ok)
	}
	must(t, h.run(func(c *LevelComponent) error { return c.OnBeat(heartbeat.Beat{Now: time.UnixMilli(10000)}) }))
	if h.c.Project().Variables["global/count"].Int != 1 {
		t.Fatal("offline replayed timer")
	}
	before := h.c.Snapshot()
	if err := h.run(func(c *LevelComponent) error { return c.AdvanceTick(10001) }); !errors.Is(err, ErrDefinition) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, h.c.Snapshot()) {
		t.Fatal("wrong clock mutated")
	}
}
func TestRestoreRejectsStateNotInFixedProgram(t *testing.T) {
	p := compiled(t, baseDefinition(), baseRegistry())
	h := newHarness(t, p, nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	before := h.c.Snapshot()
	for name, change := range map[string]func(*State){
		"unknown_variable":   func(s *State) { s.Variables["global/missing"] = Int(1) },
		"wrong_type":         func(s *State) { s.Variables["global/count"] = String("bad") },
		"unknown_trigger":    func(s *State) { s.Fires["a/missing"] = FireState{Count: 1} },
		"unknown_generation": func(s *State) { s.Generations["missing"] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			d := NewTransientLevelDAO(1)
			d.state = cloneState(before)
			change(&d.state)
			if _, err := NewLevelComponent(d, p); err == nil {
				t.Fatal("invalid restore accepted")
			}
		})
	}
	if err := (State{Now: 1}).validate(); err == nil {
		t.Fatal("garbage uninitialized state accepted")
	}
}
func TestCompileRejectsIgnoredFieldsAndVariableExpansion(t *testing.T) {
	d := baseDefinition()
	d.Stages[0].Triggers[0].Actions[0].Delay = 10
	if _, err := Compile(d, baseRegistry()); err == nil {
		t.Fatal("unused fields ignored")
	}
	d = baseDefinition()
	d.Variables = nil
	d.Stages = nil
	d.Initial = []string{"s0"}
	for i := 0; i < 128; i++ {
		d.Variables = append(d.Variables, Variable{Scope: Trigger, Name: fmt.Sprintf("v%d", i), Default: Int(0), Public: true})
	}
	for i := 0; i < 32; i++ {
		stage := StageDefinition{ID: fmt.Sprintf("s%d", i)}
		for j := 0; j < 64; j++ {
			stage.Triggers = append(stage.Triggers, TriggerDefinition{ID: fmt.Sprintf("t%d", j), Event: "hit", Actions: []Action{{Kind: "set", Scope: Trigger, Name: "v0", Value: Literal(Int(1))}}})
		}
		d.Stages = append(d.Stages, stage)
	}
	if _, err := Compile(d, baseRegistry()); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
}
