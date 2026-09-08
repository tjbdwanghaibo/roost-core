package skill

import (
	"errors"
	"fmt"
	"testing"
)

func compileExclusivitySkill(t *testing.T, id string, windup, recovery, gcd Tick, concurrent bool) (*Program, CompileEnvironment) {
	t.Helper()
	json := fmt.Sprintf(`{"schema":"roost.skill/v2","id":%q,"name":"Exclusive","description":"Cast exclusivity.",`+
		`"activation":{"type":"active","policy":{"mode":"tap"},"concurrent":%v,"cast_window":{"windup_ticks":%d,"commit_tick":0,"recovery_ticks":%d}},`+
		`"input_schema":{"type":"entity"},"cooldown_ticks":0,"global_cooldown_ticks":%d,"costs":[],"memory":{},"initial_phase":"cast",`+
		`"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`,
		id, concurrent, windup, recovery, gcd)
	return compileRuntimeJSON(t, json)
}

func TestCasterWindowExclusivityRejectsSecondRootCast(t *testing.T) {
	first, environment := compileExclusivitySkill(t, "skill.test.exclusive-a", 4, 3, 0, false)
	second, _ := compileExclusivitySkill(t, "skill.test.exclusive-b", 0, 0, 0, false)
	concurrent, _ := compileExclusivitySkill(t, "skill.test.exclusive-c", 0, 0, 0, true)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(first, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(second, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrCasterBusy) {
		t.Fatalf("windup did not lock the caster: %v", err)
	}
	// A different caster and a concurrent-flagged skill both bypass the lock.
	if _, err := runtime.Activate(second, CastInput{Caster: 3, Target: 2}); err != nil {
		t.Fatalf("exclusivity leaked across casters: %v", err)
	}
	if _, err := runtime.Activate(concurrent, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("concurrent skill blocked: %v", err)
	}
	if err := runtime.Advance(4); err != nil {
		t.Fatal(err)
	}
	// Windup done, recovery running: still busy.
	if _, err := runtime.Activate(second, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrCasterBusy) {
		t.Fatalf("recovery did not lock the caster: %v", err)
	}
	if err := runtime.Advance(7); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(second, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("caster still busy after recovery completed: %v", err)
	}
}

func TestGlobalCooldownGatesRootCastsAcrossSkills(t *testing.T) {
	trigger, environment := compileExclusivitySkill(t, "skill.test.gcd-a", 0, 0, 5, false)
	other, _ := compileExclusivitySkill(t, "skill.test.gcd-b", 0, 0, 0, false)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(trigger, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(other, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrGlobalCooldownActive) {
		t.Fatalf("global cooldown did not gate another skill: %v", err)
	}
	if _, err := runtime.Activate(other, CastInput{Caster: 3, Target: 2}); err != nil {
		t.Fatalf("global cooldown leaked across casters: %v", err)
	}
	// The global cooldown syncs as an ordinary cooldown entry under "$gcd".
	found := false
	for _, cooldown := range runtime.StateSnapshot().Cooldowns {
		if cooldown.Caster == 1 && cooldown.ProgramID == globalCooldownProgramID && cooldown.DueTick == 5 {
			found = true
		}
	}
	if !found {
		t.Fatal("global cooldown missing from the state snapshot")
	}
	if err := runtime.Advance(4); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(other, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrGlobalCooldownActive) {
		t.Fatalf("global cooldown expired early: %v", err)
	}
	if err := runtime.Advance(5); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(other, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("global cooldown never expired: %v", err)
	}
}
