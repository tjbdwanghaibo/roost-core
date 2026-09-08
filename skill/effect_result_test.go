package skill

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEffectResultDamageSuccessExecutesTypedBranch(t *testing.T) {
	flow := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"as":"damage","success":{"flow":"if","condition":{"op":"eq","args":["$local.damage.health_damage",10]},"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}},{"flow":"finish"}]},"else":{"flow":"finish"}},"failure":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}},{"flow":"finish"}]}}}`
	program, environment := compileEffectResultSkill(t, flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 1 {
		t.Fatalf("branch marker=%d", marker)
	}
}

func TestEffectResultStateChangeCarriesDynamicBeforeAndAfter(t *testing.T) {
	state := `{"marks":{"type":"int","scope":"owner_target","default":0,"minimum":0,"maximum":3,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"marks","owner":"$caster","subject":"$input.target","operation":"add","value":1,"duration_ticks":20,"expiry_policy":"refresh"},"result":{"as":"change","success":{"flow":"if","condition":{"op":"and","args":["$local.change.applied",{"op":"eq","args":["$local.change.after",1]}]},"then":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}},"else":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}}}}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(state), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)
	json = strings.Replace(json, `"memory":{}`, `"memory":{"branch":{"type":"int","default":0}}`, 1)
	json = strings.Replace(json, `{"flow":"finish"}`, flow, 1)
	program, environment := compileRuntimeJSON(t, json)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 1 {
		t.Fatalf("state result marker=%d", marker)
	}
}

func TestEffectResultBlockedCombatStillUsesSuccessOutcome(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"success":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}},"failure":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}}}},{"flow":"finish"}]}`
	program, environment := compileEffectResultSkill(t, flow)
	host := &effectResultHost{MemoryHost: runtimeTestHost(environment), damage: EffectResult{Payload: DamageEffectResult{ResultOutcome: successfulResultOutcome(), Result: DamageResult{Attempted: 10, Blocked: true}}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 1 {
		t.Fatalf("blocked damage branch marker=%d", marker)
	}
}

func TestEffectExpectedFailureFromMemoryHostUsesFailureBranch(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"failure":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}}}},{"flow":"finish"}]}`
	program, environment := compileEffectResultSkill(t, flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 999})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 2 {
		t.Fatalf("memory host failure branch marker=%d", marker)
	}
	assertSingleExpectedFailureEvent(t, runtime.RuntimeEvents(), ExpectedFailureInvalidTarget)
}

func TestEffectResultScopeRejectsInvalidFieldOutcomeAndEscape(t *testing.T) {
	tests := map[string]string{
		"wrong field":               `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"as":"r","success":{"flow":"if","condition":"$local.r.token","then":{"flow":"finish"},"else":{"flow":"finish"}}}}`,
		"success field in failure":  `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"as":"r","failure":{"flow":"if","condition":{"op":"eq","args":["$local.r.health_damage",0]},"then":{"flow":"finish"},"else":{"flow":"finish"}}}}`,
		"result escapes branch":     `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"as":"r","success":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}}}},{"flow":"if","condition":"$local.r.succeeded","then":{"flow":"finish"},"else":{"flow":"finish"}}]}`,
		"branch suspends":           `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"success":{"flow":"wait","ticks":1,"then":{"flow":"finish"}}}}`,
		"failure reason closed set": `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"as":"r","failure":{"flow":"if","condition":{"op":"eq","args":["$local.r.failure_reason","capacity_reached"]},"then":{"flow":"finish"},"else":{"flow":"finish"}}}}`,
		"process result":            `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"success":{"flow":"finish"}},"on":{"tick":{"flow":"finish"}}}`,
	}
	for name, flow := range tests {
		t.Run(name, func(t *testing.T) {
			json := effectResultSkillJSON(flow)
			if _, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment()); !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("diagnostics=%#v", diagnostics)
			}
		})
	}
}

func TestEffectExpectedFailureBranchesAndEmitsExactlyOnce(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"as":"damage","failure":{"flow":"if","condition":{"op":"eq","args":["$local.damage.failure_reason","invalid_target"]},"then":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}},"else":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":3}}}}},{"flow":"finish"}]}`
	program, environment := compileEffectResultSkill(t, flow)
	host := &effectResultHost{MemoryHost: runtimeTestHost(environment), damage: EffectResult{Payload: DamageEffectResult{ResultOutcome: ResultOutcome{FailureReason: ExpectedFailureInvalidTarget}}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 2 {
		t.Fatalf("failure branch marker=%d", marker)
	}
	assertSingleExpectedFailureEvent(t, runtime.RuntimeEvents(), ExpectedFailureInvalidTarget)
}

func TestEffectExpectedFailureWithoutMatchingBranchReturnsToParentFlow(t *testing.T) {
	tests := map[string]string{
		"no result":    `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"}},{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}},{"flow":"finish"}]}`,
		"only success": `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"success":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":3}}}},{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}},{"flow":"finish"}]}`,
	}
	for name, flow := range tests {
		t.Run(name, func(t *testing.T) {
			program, environment := compileEffectResultSkill(t, flow)
			host := &effectResultHost{MemoryHost: runtimeTestHost(environment), damage: EffectResult{Payload: DamageEffectResult{ResultOutcome: ResultOutcome{FailureReason: ExpectedFailureInvalidTarget}}}}
			runtime := NewRuntime(host, RuntimeOptions{})
			castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			marker, _ := runtime.casts[castID].memory[0].Int()
			if marker != 1 {
				t.Fatalf("parent marker=%d", marker)
			}
			assertSingleExpectedFailureEvent(t, runtime.RuntimeEvents(), ExpectedFailureInvalidTarget)
		})
	}
}

func TestEffectExpectedFailureRejectsHostContractViolation(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"failure":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}}}},{"flow":"finish"}]}`
	program, environment := compileEffectResultSkill(t, flow)
	host := &effectResultHost{MemoryHost: runtimeTestHost(environment), damage: EffectResult{Payload: DamageEffectResult{ResultOutcome: ResultOutcome{FailureReason: ExpectedFailureCapacityReached}}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("error=%v", err)
	}
	snapshot, ok := runtime.InspectCast(castID)
	if !ok || snapshot.Status != CastFailed {
		t.Fatalf("snapshot=%#v ok=%v", snapshot, ok)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 0 || len(runtime.RuntimeEvents()) != 0 {
		t.Fatalf("failure branch or event escaped contract error: marker=%d events=%#v", marker, runtime.RuntimeEvents())
	}
}

func TestEffectResultRejectsInconsistentOutcomeAndPayloadFields(t *testing.T) {
	layout := resultLayoutByType(resultTypeDamage, valueType{})
	if _, _, err := runtimeEffectResultFromHost(layout, DamageEffectResult{ResultOutcome: ResultOutcome{Succeeded: false, FailureReason: ExpectedFailureNone}}); !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("inconsistent outcome error=%v", err)
	}
	dynamicLayout := resultLayoutByType(resultTypeStateChange, valueType{Base: valueKindInt, Quantity: quantityCount})
	payload := StateChangeEffectResult{ResultOutcome: successfulResultOutcome(), Before: StringRuntimeValue("wrong"), After: IntRuntimeValue(1, quantityCount), Applied: true}
	if _, _, err := runtimeEffectResultFromHost(dynamicLayout, payload); !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("dynamic field contract error=%v", err)
	}
}

func TestEffectResultSnapshotTokenCanBeMintedByExternalHost(t *testing.T) {
	if _, err := NewSnapshotToken(0); !errors.Is(err, ErrCastInputInvalid) {
		t.Fatalf("zero token error=%v", err)
	}
	token, err := NewSnapshotToken(42)
	if err != nil || token.OpaqueID() != 42 {
		t.Fatalf("token=%#v error=%v", token, err)
	}
	layout := resultLayoutByType(resultTypeSnapshotCapture, valueType{})
	if _, _, err := runtimeEffectResultFromHost(layout, SnapshotCaptureEffectResult{ResultOutcome: successfulResultOutcome(), Token: token}); err != nil {
		t.Fatalf("external host token rejected: %v", err)
	}
}

func TestEffectResultRejectsStateFailureCarryingSuccessFields(t *testing.T) {
	state := `{"marks":{"type":"int","scope":"owner","default":0,"minimum":0,"maximum":3,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"marks","owner":"$caster","operation":"add","value":1,"duration_ticks":20,"expiry_policy":"refresh"},"result":{"failure":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}}}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(state), `"memory":{}`, `"memory":{"branch":{"type":"int","default":0}}`, 1)
	json = strings.Replace(json, `{"flow":"finish"}`, flow, 1)
	program, environment := compileRuntimeJSON(t, json)
	host := &stateResultContractHost{MemoryHost: runtimeTestHost(environment), result: StateMutationResult{ResultOutcome: failedResultOutcome(ExpectedFailureReferenceExpired), Before: IntRuntimeValue(0, quantityDimensionless), After: IntRuntimeValue(1, quantityDimensionless)}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("state contract error=%v", err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 0 {
		t.Fatalf("state failure branch executed marker=%d", marker)
	}
}

func TestEffectResultOptionalStateFieldRequiresGuardAndPreservesMissing(t *testing.T) {
	state := `{"tracked":{"type":"entity","scope":"owner","default":null,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}}`
	unguarded := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"tracked","owner":"$caster","operation":"clear"},"result":{"as":"change","success":{"flow":"if","condition":{"op":"eq","args":["$local.change.after","$caster"]},"then":{"flow":"finish"},"else":{"flow":"finish"}}}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(state), `{"flow":"finish"}`, unguarded, 1)
	if _, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment()); !diagnosticsHaveErrors(diagnostics) {
		t.Fatal("unguarded optional result field compiled")
	}

	guarded := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"tracked","owner":"$caster","operation":"clear"},"result":{"as":"change","success":{"flow":"if","condition":{"op":"exists","args":["$local.change.after"]},"then":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":2}},"else":{"flow":"effect","effect":{"type":"set_memory","name":"branch","value":1}}}}},{"flow":"finish"}]}`
	json = strings.Replace(stateSchemaSkillJSON(state), `"memory":{}`, `"memory":{"branch":{"type":"int","default":0}}`, 1)
	json = strings.Replace(json, `{"flow":"finish"}`, guarded, 1)
	program, environment := compileRuntimeJSON(t, json)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := runtime.casts[castID].memory[0].Int()
	if marker != 1 {
		t.Fatalf("optional result marker=%d", marker)
	}
}

func TestEffectResultStatusStackFieldsReflectMutation(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
	command := EffectCommand{Payload: StatusCommand{SourceOwner: 1, Target: 1, Status: 1, DurationTicks: 10, Stacks: 1}}
	first, err := host.Apply(command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Apply(command)
	if err != nil {
		t.Fatal(err)
	}
	remove, err := host.Apply(EffectCommand{Payload: RemoveStatusCommand{SourceOwner: 1, Target: 1, Status: 1}})
	if err != nil {
		t.Fatal(err)
	}
	firstResult := first.Payload.(StatusEffectResult).Result
	secondResult := second.Payload.(StatusEffectResult).Result
	removeResult := remove.Payload.(StatusEffectResult).Result
	if firstResult.PreviousStacks != 0 || firstResult.CurrentStacks != 1 || secondResult.PreviousStacks != 1 || secondResult.CurrentStacks != 1 || removeResult.PreviousStacks != 1 || removeResult.CurrentStacks != 0 || removeResult.RemovedStacks != 1 {
		t.Fatalf("stack results first=%#v second=%#v remove=%#v", firstResult, secondResult, removeResult)
	}
}

func TestEffectResultInspectorAndRuntimeValueAreImmutableProjections(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},"result":{"success":{"flow":"finish"}}},{"flow":"finish"}]}`
	program, environment := compileEffectResultSkill(t, flow)
	views := InspectEffectResults(program)
	if len(views) != 1 || len(views[0].FieldHandles) == 0 {
		t.Fatalf("views=%#v", views)
	}
	views[0].FieldHandles[0] = "corrupt"
	viewsAgain := InspectEffectResults(program)
	if viewsAgain[0].FieldHandles[0] == "corrupt" {
		t.Fatal("inspector mutation changed program")
	}
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if containsMapType(reflect.TypeOf(runtimeEffectResultValue{}), map[reflect.Type]bool{}) {
		t.Fatal("runtime result contains a map")
	}
}

type effectResultHost struct {
	*MemoryHost
	damage EffectResult
	err    error
}

type stateResultContractHost struct {
	*MemoryHost
	result StateMutationResult
}

func (host *stateResultContractHost) ModifyState(StateMutationCommand) (StateMutationResult, error) {
	return host.result, nil
}

func (host *effectResultHost) Apply(command EffectCommand) (EffectResult, error) {
	if _, ok := command.Payload.(DamageCommand); ok {
		return host.damage, host.err
	}
	return host.MemoryHost.Apply(command)
}

func assertSingleExpectedFailureEvent(t *testing.T, events []RuntimeEvent, reason ExpectedFailureReason) {
	t.Helper()
	count := 0
	for _, event := range events {
		if event.Kind != "effect_expected_failure" {
			continue
		}
		count++
		if event.Result == nil || event.Result.FailureReason != reason || event.Result.ResultType != string(resultTypeDamage) {
			t.Fatalf("event=%#v", event)
		}
	}
	if count != 1 {
		t.Fatalf("expected failure events=%d, all=%#v", count, events)
	}
}

func containsMapType(typ reflect.Type, seen map[reflect.Type]bool) bool {
	if typ == nil || seen[typ] {
		return false
	}
	seen[typ] = true
	if typ.Kind() == reflect.Map {
		return true
	}
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return containsMapType(typ.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < typ.NumField(); index++ {
			if containsMapType(typ.Field(index).Type, seen) {
				return true
			}
		}
	}
	return false
}

func compileEffectResultSkill(t *testing.T, flow string) (*Program, CompileEnvironment) {
	t.Helper()
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, effectResultSkillJSON(flow)), environment)
	requireNoErrors(t, diagnostics)
	return program, environment
}

func effectResultSkillJSON(flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.effect-result","name":"Result","description":"Typed effect result.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{"branch":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}
