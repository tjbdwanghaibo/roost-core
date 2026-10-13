package skill

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestBudgetRejectsRepeatAboveLimit(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"clear_memory","name":"x"}}},{"flow":"finish"}]}`, 1)
	// clear_memory 的名字必须声明（RR-20261005-NC-210），否则类型检查先于预算 pass 拒绝。
	input = strings.Replace(input, `"memory":{}`, `"memory":{"x":{"type":"int","default":0}}`, 1)
	environment := DefaultCompileEnvironment()
	environment.Limits.MaxRepeat = 2
	environment.Digest = authorityDigest(environment)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), environment)
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticBudgetExceeded)
}

func TestCompileRejectsInvalidPhaseGraphs(t *testing.T) {
	tests := []struct {
		name, json string
		code       DiagnosticCode
	}{
		{"duplicate id", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}},{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseDuplicate},
		{"missing target", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"missing"}}}]`), DiagnosticPhaseTargetMissing},
		{"self cycle", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"a"}}}]`), DiagnosticPhaseCycle},
		{"multi phase cycle", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"b"}}},{"id":"b","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"a"}}}]`), DiagnosticPhaseCycle},
		{"unreachable", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}},{"id":"b","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseUnreachable},
		{"missing initial", phaseSkillJSON("missing", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseInitialMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, tt.json), DefaultCompileEnvironment())
			if artifacts != nil {
				t.Fatal("expected nil artifacts")
			}
			requireDiagnostic(t, diagnostics, tt.code)
		})
	}
}

func phaseSkillJSON(initial, phases string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.graph","name":"Graph","description":"Tests graph.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"` + initial + `","phases":` + phases + `}`
}

func TestCompileRejectsFallthroughEnterWithoutTimeout(t *testing.T) {
	data := strings.Replace(strings.ReplaceAll(string(mustReadFixture(t, "simple_damage.json")), "\r\n", "\n"), `,
            {
              "flow": "finish",
              "reason": "done"
            }`, "", 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticLifecycleFallthrough)
}

func TestCompileRejectsParallelFinish(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"parallel","branches":[{"flow":"finish"},{"flow":"effect","effect":{"type":"clear_memory","name":"x"}}]}`, 1)
	// clear_memory 的名字必须声明（RR-20261005-NC-210），否则类型检查先于生命期 pass 拒绝。
	input = strings.Replace(input, `"memory":{}`, `"memory":{"x":{"type":"int","default":0}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticLifecycleControlConflict)
}

const nullableMemorySkillPrefix = `{"schema":"roost.skill/v2","id":"skill.test.memory","name":"Memory","description":"Tests memory.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"anchor":{"type":"entity","default":null}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":`

func TestCompileRejectsMaybeInitializedMemoryRead(t *testing.T) {
	input := nullableMemorySkillPrefix + `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticMemoryMaybeUninitialized)
}

func TestCompileAcceptsGuardedMemoryRead(t *testing.T) {
	input := nullableMemorySkillPrefix + `{"flow":"if","condition":{"op":"exists","args":["$memory.anchor"]},"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]},"else":{"flow":"finish"}}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected guarded compile, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}

func TestCompileAcceptsInitializedMemoryRead(t *testing.T) {
	input := `{"schema":"roost.skill/v2","id":"skill.test.memory.initialized","name":"Memory","description":"Tests initialized memory.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"anchor":{"type":"entity","default":"$caster"}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected initialized memory compile, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}

func TestProcRejectsRequiredExcludedTagConflict(t *testing.T) {
	input := passiveSkillJSON(1, `["spell"]`, `["spell"]`)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticEventFilterConflict)
}

func TestProcRejectsDepthAboveEnvironmentLimit(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Limits.MaxProcDepth = 1
	environment.Digest = authorityDigest(environment)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, passiveSkillJSON(2, `[]`, `[]`)), environment)
	requireDiagnostic(t, diagnostics, DiagnosticProcLimitExceeded)
}

func TestProcResolvesElementAndDamageTypeHandles(t *testing.T) {
	input := passiveSkillJSON(1, `[]`, `[]`)
	input = strings.Replace(input, `"elements":[]`, `"elements":["fire"]`, 1)
	input = strings.Replace(input, `"damage_types":[]`, `"damage_types":["magic"]`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if artifacts.proc.plan == nil || len(artifacts.proc.plan.Filter.Elements) != 1 || artifacts.proc.plan.Filter.Elements[0] != ElementHandle(2) {
		t.Fatalf("element plan = %#v", artifacts.proc.plan)
	}
	if len(artifacts.proc.plan.Filter.DamageTypes) != 1 || artifacts.proc.plan.Filter.DamageTypes[0] != DamageTypeHandle(2) {
		t.Fatalf("damage type plan = %#v", artifacts.proc.plan)
	}
}

func passiveSkillJSON(maxDepth int, required, excluded string) string {
	return `{
  "schema":"roost.skill/v2","id":"skill.test.passive","name":"Passive","description":"Runs on damage.",
  "activation":{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":` + required + `,"excluded_tags":` + excluded + `,"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":` + intString(maxDepth) + `,"allow_self_trigger":false,"once_per_root_event":true}},
  "input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"finish","reason":"done"}}}]
}`
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(rune('0'+value%10)) + result
		value /= 10
	}
	return result
}

func TestCompilePassOrder(t *testing.T) {
	artifacts, diagnostics := compileToArtifacts(mustParseFixture(t, "simple_damage.json"), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	want := []string{"normalize", "shape", "authority_capability", "gameplay_tags", "input_state", "temporal", "type_snapshot", "effect_result_scope", "graph", "memory", "lifetime_ownership", "motion", "event_proc", "identity_random", "budget", "visual", "lower"}
	if !reflect.DeepEqual(artifacts.passOrder, want) {
		t.Fatalf("pass order = %v, want %v", artifacts.passOrder, want)
	}
	if !artifacts.lowerReady {
		t.Fatal("successful pipeline did not reach lower")
	}
}

func TestCompilePassOrderStopsAfterUpstreamError(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Revision = ""
	artifacts, diagnostics := compileToArtifactsInternal(mustParseFixture(t, "simple_damage.json"), environment)
	requireDiagnostic(t, diagnostics, DiagnosticEnvironmentInvalid)
	want := []string{"normalize", "shape", "authority_capability"}
	if !reflect.DeepEqual(artifacts.passOrder, want) {
		t.Fatalf("pass order = %v, want %v", artifacts.passOrder, want)
	}
	if artifacts.lowerReady {
		t.Fatal("failed pipeline returned lower-ready artifacts")
	}
}

func TestShapeRejectsSelectOneWithMultipleResults(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{
    "flow":"select",
    "select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":5},"filters":[],"limit":2},
    "consume":{"mode":"one","as":"target","then":{"flow":"finish","reason":"done"}}
  }`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticShapeInvalid)
}

func TestShapeAcceptsVisualAfterVisualPassIsDeployed(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"description":"Immediately finishes."`, `"description":"Immediately finishes.","presentation":{"icon_keywords":["flame","bolt","spark"],"cast":{"category":"cast","theme":"default","elements":["default"]}}`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
}

func TestAttributeSnapshotResolvesHandleAndPoint(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"cast_start"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	plan := artifacts.snapshots.reads["$.phases[0].on.enter.steps[0].effect.amount"]
	if plan.Attribute != AttributeHandle(2) || plan.Snapshot != snapshotCastStart {
		t.Fatalf("snapshot plan = %#v", plan)
	}
}

func TestAttributeSnapshotRejectsUnsupportedPoint(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"health","snapshot":"each_tick"}}`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticAttributeSnapshotInvalid)
}

func TestGameplayTagsResolveDeclarableHandles(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"activation"`, `"gameplay_tags":["spell","spell"],"activation"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if len(artifacts.gameplay.skillTags) != 1 || artifacts.gameplay.skillTags[0] != GameplayTagHandle(1) {
		t.Fatalf("resolved tags = %v, want [1]", artifacts.gameplay.skillTags)
	}
}

func TestGameplayTagsRejectCompilerAndRuntimeOnlyDeclarations(t *testing.T) {
	for _, tag := range []string{"projectile", "critical"} {
		t.Run(tag, func(t *testing.T) {
			input := strings.Replace(minimalSkillJSON, `"activation"`, `"gameplay_tags":["`+tag+`"],"activation"`, 1)
			_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticGameplayTagPermission)
		})
	}
}

func TestGameplayTagsDamageDefaultsToNeutralElement(t *testing.T) {
	artifacts, diagnostics := compileToArtifacts(mustParseFixture(t, "simple_damage.json"), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	damage := artifacts.gameplay.damage["$.phases[0].on.enter.steps[0].effect"]
	if damage.DamageType != DamageTypeHandle(1) || damage.Element != ElementHandle(1) {
		t.Fatalf("resolved damage = %#v, want physical/neutral handles", damage)
	}
}

func TestGameplayTagsRejectTrueDamageWithElement(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"damage_type": "physical"`, `"damage_type": "true", "element": "fire"`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticGameplayElementInvalid)
}

func TestCompileRejectsDamageToPosition(t *testing.T) {
	data := string(mustReadFixture(t, "simple_damage.json"))
	data = strings.Replace(data, `"type": "entity"`, `"type": "position"`, 1)
	data = strings.Replace(data, `"target": "$input.target"`, `"target": "$input.position"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)
}

func TestCompileRejectsUnknownLocal(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"$input.target"`, `"$local.missing"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticReferenceUnknown)
}

func TestCompileRejectsDamageAmountWithWrongType(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount": true`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)
}

func TestCompileRejectsInputReferenceOutsideSchema(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"type": "entity"`, `"type": "none"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticInputUnavailable)
}

func TestCompileRejectsExistsOnNonOptionalValue(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"if","condition":{"op":"exists","args":["$caster"]},"then":{"flow":"finish"},"else":{"flow":"finish"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticOptionalInvalid)
}

func TestCompileRejectsQuantityMismatchInDamageAmount(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"move_speed","snapshot":"current"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticQuantityMismatch)
}

func TestCompileAcceptsCombatAmountAttributeForDamage(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"current"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected artifacts, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}

func TestCompileRejectsPhaseEventsTheRuntimeNeverDispatches(t *testing.T) {
	finish := `{"flow":"finish","reason":"done"}`
	for _, event := range []string{"recast", "timeout"} {
		t.Run(event, func(t *testing.T) {
			input := strings.Replace(minimalSkillJSON, `"on":{"enter":`+finish+`}`, `"on":{"enter":`+finish+`,"`+event+`":`+finish+`}`, 1)
			if input == minimalSkillJSON {
				t.Fatal("fixture replacement failed")
			}
			program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			if program != nil {
				t.Fatalf("on.%s compiled into a Program the Runtime never runs it from; diagnostics=%#v", event, diagnostics)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticCapabilityUnknown, "$.phases[0].on."+event)
		})
	}
}

func TestPhaseTimeoutTicksDoNotExcuseEnterFallthrough(t *testing.T) {
	enter := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}}`
	timeout := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":5,"damage_type":"physical"}},{"flow":"finish","reason":"timeout"}]}`
	input := `{"schema":"roost.skill/v2","id":"skill.test.phase.timeout","name":"Timeout","description":"Phase timeout.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":3,"on":{"enter":` + enter + `,"timeout":` + timeout + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, input), environment)
	if program != nil {
		host := runtimeTestHost(environment)
		_, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Target: 2})
		t.Fatalf("fallthrough enter with timeout_ticks compiled; Activate = %v (invariant=%v)", err, errors.Is(err, ErrProgramInvariant))
	}
	requireDiagnosticAt(t, diagnostics, DiagnosticLifecycleFallthrough, "$.phases[0].on.enter")

	// 只去掉 timeout 分支、enter 仍不结束：fallthrough 本身也不能再靠 timeout_ticks 通过。
	withoutHandler := strings.Replace(input, `,"timeout":`+timeout, ``, 1)
	if _, diagnostics := Compile(mustParseJSON(t, withoutHandler), environment); !diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("fallthrough enter excused by timeout_ticks: %#v", diagnostics)
	}
}

// 不依赖 fallthrough 的非零 timeout_ticks 仍可编译，但给出 warning：Runtime
// 没有 phase 计时，这个值不起作用。
func TestNonzeroPhaseTimeoutTicksWarnsThatItIsNotEnforced(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"timeout_ticks":0`, `"timeout_ticks":5`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	if program == nil || diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("finishing phase with timeout_ticks rejected: %#v", diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticWarning && diagnostic.Path == "$.phases[0].timeout_ticks" {
			return
		}
	}
	t.Fatalf("missing timeout_ticks warning: %#v", diagnostics)
}

func requireDiagnosticAt(t *testing.T, diagnostics []Diagnostic, code DiagnosticCode, path string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Path == path && diagnostic.Severity == DiagnosticError {
			return
		}
	}
	t.Fatalf("missing error %s at %s in %#v", code, path, diagnostics)
}

func TestPhaseEventTableIsTheSingleSource(t *testing.T) {
	if fields := reflect.TypeOf(phaseEventsIR{}).NumField(); fields != len(phaseEventTable) {
		t.Fatalf("phaseEventsIR has %d events, the table lists %d", fields, len(phaseEventTable))
	}
	if fields := reflect.TypeOf(PhaseEventsDefinition{}).NumField(); fields != len(phaseEventTable) {
		t.Fatalf("PhaseEventsDefinition has %d events, the table lists %d", fields, len(phaseEventTable))
	}
	sentinel := func(name string) flowIR { return &finishFlowIR{reason: name} }
	events := phaseEventsIR{
		enter: sentinel(phaseEventEnter), recast: sentinel(phaseEventRecast), cancel: sentinel(phaseEventCancel),
		directionChanged: sentinel(phaseEventDirectionChanged), targetChanged: sentinel(phaseEventTargetChanged),
		timeout: sentinel(phaseEventTimeout), release: sentinel(phaseEventRelease), pulse: sentinel(phaseEventPulse),
	}
	seen := map[string]bool{}
	for _, event := range phaseEventTable {
		if seen[event.name] {
			t.Fatalf("event %q listed twice", event.name)
		}
		seen[event.name] = true
		if got := event.flow(events).(*finishFlowIR).reason; got != event.name {
			t.Fatalf("table entry %q reads the %q field", event.name, got)
		}
	}
	dispatched := map[string]bool{}
	for _, flow := range dispatchedPhaseEventFlows(events) {
		dispatched[flow.name] = true
	}
	for _, flow := range undispatchedPhaseEventFlows(events) {
		if dispatched[flow.name] {
			t.Fatalf("event %q is both dispatched and refused", flow.name)
		}
		dispatched[flow.name] = true
	}
	if len(dispatched) != len(phaseEventTable) {
		t.Fatalf("dispatched + refused cover %d events, want %d", len(dispatched), len(phaseEventTable))
	}
	for _, port := range []InputPort{InputPortDirectionChanged, InputPortTargetChanged} {
		if !seen[string(port)] {
			t.Fatalf("input port %q is not a phase event: UpdateInput would look up a root lower never exported", port)
		}
	}
}
