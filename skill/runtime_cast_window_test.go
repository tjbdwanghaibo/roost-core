package skill

import (
	"errors"
	"testing"
)

func TestCastWindowUsesExactPrepareCommitExecuteRecoveryTimeline(t *testing.T) {
	program, environment := compileCastWindowSkill(t, true)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowPreparing)
	if host.ResourceForTest(1, "mana") != 100 || host.HealthForTest(2) != 100 {
		t.Fatal("prepare paid refundable cost or executed phase enter")
	}
	if err := runtime.Advance(2); err != nil || host.ResourceForTest(1, "mana") != 100 {
		t.Fatalf("commit happened early: %v", err)
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowCommitted)
	if host.ResourceForTest(1, "mana") != 95 || runtime.CooldownUntil(program, 1) != 13 || host.HealthForTest(2) != 100 {
		t.Fatalf("commit boundary mismatch: mana=%d cooldown=%d health=%d", host.ResourceForTest(1, "mana"), runtime.CooldownUntil(program, 1), host.HealthForTest(2))
	}
	if err := runtime.Advance(5); err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowRecovering)
	if host.HealthForTest(2) != 90 {
		t.Fatal("phase enter did not execute at windup boundary")
	}
	if err := runtime.Cancel(castID); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("recovery accepted gameplay input: %v", err)
	}
	if err := runtime.Advance(8); err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowRecovering)
	if err := runtime.Advance(9); err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastFinished, CastWindowComplete)
}

func TestCastWindowCancelBeforeCommitUsesRefundPolicy(t *testing.T) {
	for _, refund := range []bool{true, false} {
		t.Run(map[bool]string{true: "refundable", false: "paid-at-prepare"}[refund], func(t *testing.T) {
			program, environment := compileCastWindowSkill(t, refund)
			host := runtimeTestHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{})
			castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Cancel(castID); err != nil {
				t.Fatal(err)
			}
			wantMana := int64(100)
			if !refund {
				wantMana = 95
			}
			if host.ResourceForTest(1, "mana") != wantMana || runtime.CooldownUntil(program, 1) != 0 {
				t.Fatalf("refund=%v mana=%d cooldown=%d", refund, host.ResourceForTest(1, "mana"), runtime.CooldownUntil(program, 1))
			}
			if err := runtime.Advance(5); err != nil || host.HealthForTest(2) != 100 {
				t.Fatalf("cancelled cast executed: %v", err)
			}
		})
	}
}

func compileCastWindowSkill(t *testing.T, refund bool) (*Program, CompileEnvironment) {
	t.Helper()
	refundJSON := "false"
	if refund {
		refundJSON = "true"
	}
	json := `{"schema":"roost.skill/v2","id":"skill.test.cast-window","name":"Window","description":"Cast window.","activation":{"type":"active","policy":{"mode":"tap"},"cast_window":{"windup_ticks":5,"commit_tick":3,"recovery_ticks":4,"movement":"locked","turning":"allowed","interrupt_tags":[],"refund_before_commit":` + refundJSON + `}},"input_schema":{"type":"entity"},"cooldown_ticks":10,"costs":[{"resource":"mana","amount":5}],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	return compileRuntimeJSON(t, json)
}

func assertCastWindowState(t *testing.T, runtime *Runtime, castID CastID, status CastStatus, stage CastWindowStage) {
	t.Helper()
	snapshot, found := runtime.InspectCast(castID)
	if !found || snapshot.Status != status || snapshot.WindowStage != stage {
		t.Fatalf("cast = %#v found=%v", snapshot, found)
	}
}
