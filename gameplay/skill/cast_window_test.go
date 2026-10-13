package skill

import (
	"errors"
	"fmt"
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

func castWindowExpressionJSON(id, windowJSON string) string {
	return fmt.Sprintf(`{"schema":"roost.skill/v2","id":%q,"name":"Dynamic","description":"Dynamic window.",`+
		`"activation":{"type":"active","policy":{"mode":"tap"},"cast_window":%s},`+
		`"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",`+
		`"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`,
		id, windowJSON)
}

func TestCastWindowExpressionsAreClampedToDeclaredBounds(t *testing.T) {
	// The windup expression yields 9, far above its [2,5] bound; the recovery
	// expression yields 1, below its [3,6] bound. The declared bounds must
	// win in both directions, keeping the compile-time worst case authoritative.
	window := `{"windup_ticks_expression":{"op":"add","args":[4,5]},"windup_ticks_min":2,"windup_ticks_max":5,"commit_tick":2,` +
		`"recovery_ticks_expression":1,"recovery_ticks_min":3,"recovery_ticks_max":6}`
	program, environment := compileRuntimeJSON(t, castWindowExpressionJSON("skill.test.dynamic-window", window))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(4); err != nil || host.HealthForTest(2) != 100 {
		t.Fatalf("windup clamped wrong: executed before tick 5 (health=%d err=%v)", host.HealthForTest(2), err)
	}
	if err := runtime.Advance(5); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 90 {
		t.Fatal("windup expression did not execute at the clamped maximum")
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowRecovering)
	if err := runtime.Advance(7); err != nil {
		t.Fatal(err)
	}
	assertCastWindowState(t, runtime, castID, CastSuspended, CastWindowRecovering)
	if err := runtime.Advance(8); err != nil {
		t.Fatal(err)
	}
	// Recovery ran for the clamped minimum of 3 ticks (5 -> 8).
	assertCastWindowState(t, runtime, castID, CastFinished, CastWindowComplete)
}

func TestCastWindowExpressionShapeRules(t *testing.T) {
	cases := map[string]string{
		"bounds without expression":  `{"windup_ticks":3,"windup_ticks_min":1,"windup_ticks_max":4,"commit_tick":0,"recovery_ticks":0}`,
		"commit beyond windup min":   `{"windup_ticks_expression":5,"windup_ticks_min":2,"windup_ticks_max":8,"commit_tick":3,"recovery_ticks":0}`,
		"literal with expression":    `{"windup_ticks":3,"windup_ticks_expression":5,"windup_ticks_min":2,"windup_ticks_max":8,"commit_tick":0,"recovery_ticks":0}`,
		"inverted recovery bounds":   `{"windup_ticks":0,"commit_tick":0,"recovery_ticks_expression":5,"recovery_ticks_min":6,"recovery_ticks_max":2}`,
		"recovery bounds without":    `{"windup_ticks":0,"commit_tick":0,"recovery_ticks":2,"recovery_ticks_min":1,"recovery_ticks_max":4}`,
		"literal recovery with expr": `{"windup_ticks":0,"commit_tick":0,"recovery_ticks":2,"recovery_ticks_expression":5,"recovery_ticks_min":1,"recovery_ticks_max":6}`,
	}
	for name, window := range cases {
		t.Run(name, func(t *testing.T) {
			definition := mustParseJSON(t, castWindowExpressionJSON("skill.test.dynamic-window-invalid", window))
			_, diagnostics := Compile(definition, DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticShapeInvalid)
		})
	}
}

// 外部 Host 只用导出标识（ValueKindInt / QuantityBasisPoints）声明急速属性并按它缩放前摇：这条链路必须可达。
func TestExternallyAuthoredHasteAttributeDrivesWindup(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Attributes.Entries = append(environment.Gameplay.Attributes.Entries, AttributeCatalogEntry{
		Handle: 40, Key: "attack_haste_bp", ValueType: ValueKindInt, Quantity: QuantityBasisPoints,
		Readable: true, Snapshots: []string{"current"}, ModifierOperations: []string{"add"},
		Minimum: 0, Maximum: 20000, Rounding: "toward_zero",
	})
	window := `{"windup_ticks_expression":{"op":"scale_bp","args":[10,{"read_attribute":{"entity":"$caster","attribute":"attack_haste_bp","snapshot":"current"}}]},` +
		`"windup_ticks_min":2,"windup_ticks_max":10,"commit_tick":0,"recovery_ticks":0}`
	environment.Digest = AuthorityDigest(environment)
	program, diagnostics := Compile(mustParseJSON(t, castWindowExpressionJSON("skill.test.haste-window", window)), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Attributes: map[AttributeHandle]int64{40: 6000}})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	// 10 * 6000bp = 6 ticks of windup: no damage at tick 5, damage at tick 6.
	if err := runtime.Advance(5); err != nil || host.HealthForTest(2) != 100 {
		t.Fatalf("windup resolved early: health=%d err=%v", host.HealthForTest(2), err)
	}
	if err := runtime.Advance(6); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 90 {
		t.Fatalf("haste-scaled windup did not execute at tick 6 (health=%d)", host.HealthForTest(2))
	}
}

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
