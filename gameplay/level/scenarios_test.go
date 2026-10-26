package level

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func exampleRegistry() Registry {
	return Registry{Events: map[string]map[string]ValueKind{"region.enter": {"actor": Integer, "region": Text, "checkpoint_ready": Boolean}, "enemy.dead": {}}, Actions: map[string]ActionSpec{"checkpoint": {Version: 1, Args: map[string]ValueKind{"actor": Integer}}, "spawn_wave": {Version: 1, Args: map[string]ValueKind{"wave": Integer}}, "settle": {Version: 1, Args: map[string]ValueKind{"winner": Integer}}}}
}
func example(t *testing.T, name string) *harness {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	must(t, err)
	d, err := DecodeDefinition(raw)
	must(t, err)
	h := newHarness(t, compiled(t, d, exampleRegistry()), nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	return h
}
func completeAll(t *testing.T, h *harness, now int64) {
	t.Helper()
	for _, e := range h.c.PendingActions() {
		receipt := Receipt{Instance: e.Instance, Epoch: e.Epoch, ID: e.ID, Success: true}
		must(t, h.run(func(c *LevelComponent) error { return c.Complete(receipt, now) }))
		before := h.c.Snapshot()
		must(t, h.run(func(c *LevelComponent) error { return c.Complete(receipt, now) }))
		if !reflect.DeepEqual(before, h.c.Snapshot()) {
			t.Fatal("duplicate result advanced state")
		}
	}
}
func TestTwoConfiguredConsumersUseTheSameComponent(t *testing.T) {
	t.Run("obstacle", func(t *testing.T) {
		h := example(t, "obstacle")
		send := func(seq uint64, region string, ready bool, now int64) {
			event := hit(seq)
			event.Kind = "region.enter"
			event.Fields = map[string]Value{"actor": Int(42), "region": String(region), "checkpoint_ready": Bool(ready)}
			must(t, h.run(func(c *LevelComponent) error { return c.Handle(event, now) }))
		}
		send(1, "finish", false, 1)
		if len(h.c.PendingActions()) != 0 {
			t.Fatal("finish bypassed checkpoint")
		}
		send(2, "checkpoint", false, 2)
		pending := h.c.PendingActions()
		if len(pending) != 1 || pending[0].Name != "checkpoint" || pending[0].Args["actor"].Int != 42 {
			t.Fatal(pending)
		}
		pending[0].Args["actor"] = Int(999)
		if h.c.PendingActions()[0].Args["actor"].Int != 42 {
			t.Fatal("action args alias")
		}
		completeAll(t, h, 3)
		if _, ok := h.c.Snapshot().Active["finish"]; !ok {
			t.Fatal("receipt failed to advance stage")
		}
		send(3, "finish", false, 4)
		if h.c.Snapshot().Phase != Running {
			t.Fatal("unqualified finish accepted")
		}
		send(4, "finish", true, 5)
		if h.c.Snapshot().Phase != Finishing || h.c.Snapshot().Result != "completed" {
			t.Fatal(h.c.Snapshot())
		}
		if err := h.run(func(c *LevelComponent) error { return c.Close(6) }); !errors.Is(err, ErrState) {
			t.Fatal("closed with unresolved settlement", err)
		}
		completeAll(t, h, 6)
		must(t, h.run(func(c *LevelComponent) error { return c.Close(6) }))
		must(t, h.run(func(c *LevelComponent) error { return c.AdvanceTick(9) }))
		if h.c.Snapshot().Phase != Closed {
			t.Fatal("close deadline")
		}
	})
	t.Run("waves", func(t *testing.T) {
		h := example(t, "waves")
		first := h.c.PendingActions()[0]
		completeAll(t, h, 1)
		event := hit(1)
		event.Kind = "enemy.dead"
		event.Stage = "wave"
		event.Generation = 1
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(event, 2) }))
		event.Sequence = 2
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(event, 3) }))
		state := h.c.Snapshot()
		if state.Active["wave"].Generation != 2 || h.c.Project().Variables["stage/wave/kills"].Int != 0 {
			t.Fatal("stage state not reset", state)
		}
		event.Sequence = 3
		if err := h.run(func(c *LevelComponent) error { return c.Handle(event, 4) }); !errors.Is(err, ErrStale) {
			t.Fatal("old stage event accepted", err)
		}
		if h.c.PendingActions()[0].ID == first.ID {
			t.Fatal("action identity reused")
		}
		completeAll(t, h, 4)
		event.Generation = 2
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(event, 5) }))
		event.Sequence = 4
		must(t, h.run(func(c *LevelComponent) error { return c.Handle(event, 6) }))
		if h.c.Snapshot().Result != "victory" || len(h.c.Snapshot().Timers) != 0 {
			t.Fatal("wave result/timer cleanup")
		}
		completeAll(t, h, 7)
	})
}
func TestTimerExpiryCancelsStageWorkAndRejectsLateResults(t *testing.T) {
	h := example(t, "waves")
	effect := h.c.PendingActions()[0]
	due, ok := h.c.NextDeadline()
	if !ok || due != 20 {
		t.Fatal("deadline", due)
	}
	must(t, h.run(func(c *LevelComponent) error { return c.AdvanceTick(200) }))
	if h.c.Snapshot().Result != "timeout" || len(h.c.PendingActions()) != 0 {
		t.Fatal("timeout cleanup")
	}
	err := h.run(func(c *LevelComponent) error {
		return c.Complete(Receipt{Instance: effect.Instance, Epoch: effect.Epoch, ID: effect.ID, Success: true}, 201)
	})
	if !errors.Is(err, ErrStale) {
		t.Fatal("cancelled result accepted", err)
	}
}
func TestStageLoopIsBoundedAndCompilerRejectsUnavailableActions(t *testing.T) {
	d := baseDefinition()
	d.Stages[0].Triggers = []TriggerDefinition{{ID: "loop", Event: EventStageStart, Actions: []Action{{Kind: "end", Name: "a"}, {Kind: "start", Name: "a"}}}}
	d.Limits = DefaultLimits()
	d.Limits.Steps = 20
	h := newHarness(t, compiled(t, d, baseRegistry()), nest.RollbackUndo, false, nil)
	must(t, h.run(func(c *LevelComponent) error { return c.Initialize("instance", 1, 0) }))
	before := h.c.Snapshot()
	if err := h.run(func(c *LevelComponent) error { return c.Start(0) }); !errors.Is(err, ErrBudget) {
		t.Fatal("loop not bounded", err)
	}
	if !reflect.DeepEqual(before, h.c.Snapshot()) {
		t.Fatal("loop advanced state")
	}
	d = baseDefinition()
	d.Stages[0].Triggers[0].Actions = []Action{{Kind: "request", Name: "respawn"}}
	if _, err := Compile(d, baseRegistry()); !errors.Is(err, ErrDefinition) {
		t.Fatal("missing physical capability accepted", err)
	}
	d = baseDefinition()
	d.Stages[0].Triggers[0].Actions[0].Value = Literal(String("bad"))
	if _, err := Compile(d, baseRegistry()); !errors.Is(err, ErrDefinition) {
		t.Fatal("wrong type accepted", err)
	}
	if _, err := DecodeDefinition([]byte(`{"unknown":1}`)); err == nil {
		t.Fatal("unknown configuration accepted")
	}
}
