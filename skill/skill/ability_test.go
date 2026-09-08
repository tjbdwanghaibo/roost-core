package skill

import (
	"errors"
	"testing"
)

func TestAbilityStateCooldownMutationClampsAndPreservesCast(t *testing.T) {
	environment := abilityTestEnvironment()
	flow := `{"flow":"wait","ticks":20,"then":{"flow":"finish"}}`
	program := compileAbilityTestSkill(t, environment, "cooldown", `{"mode":"tap"}`, 30, flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	operations := []struct {
		name      string
		operation string
		operand   int64
		want      int64
	}{
		{"add", "add", -20, 10},
		{"set", "set", 20, 20},
		{"mul_bp", "mul_bp", 5000, 10},
		{"min", "min", 8, 8},
		{"max", "max", 12, 12},
		{"maximum clamp", "set", 100, 60},
		{"minimum clamp", "add", -100, 0},
	}
	for _, test := range operations {
		t.Run(test.name, func(t *testing.T) {
			result, mutationErr := runtime.ModifyAbilityState(1, handle, "cooldown_remaining_ticks", test.operation, IntRuntimeValue(test.operand, abilityMutationQuantity(test.operation)), 0, EventContext{})
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			got, _ := result.After.Int()
			if got != test.want {
				t.Fatalf("after=%d want=%d", got, test.want)
			}
		})
	}
	active, err := runtime.ReadAbilityState(1, handle, "cast_active")
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := active.Bool(); !value {
		t.Fatal("cooldown mutation cancelled the active cast")
	}
	if snapshot, ok := runtime.InspectCast(castID); !ok || snapshot.Status != CastSuspended {
		t.Fatalf("cast=%#v present=%v", snapshot, ok)
	}
	if got := countAbilityEvents(runtime.RuntimeEvents(), "cooldown_remaining_ticks"); got != len(operations) {
		t.Fatalf("cooldown events=%d", got)
	}
}

func TestAbilityStateReadAndMutationExecuteFromDSL(t *testing.T) {
	environment := abilityTestEnvironment()
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_ability_state","owner":"$caster","ability":"$ability.self","property":"cooldown_remaining_ticks","operation":"add","value":-20}},{"flow":"if","condition":{"op":"eq","args":[{"read_ability_state":{"owner":"$caster","ability":"$ability.self","property":"cooldown_remaining_ticks","snapshot":"current"}},10]},"then":{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}}},{"flow":"finish"}]}`
	program := compileAbilityTestSkill(t, environment, "dsl", `{"mode":"tap"}`, 30, flow)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(1) != 99 || countAbilityEvents(runtime.RuntimeEvents(), "cooldown_remaining_ticks") != 1 {
		t.Fatalf("health=%d events=%#v", host.HealthForTest(1), runtime.RuntimeEvents())
	}
}

func TestAbilityStateRejectsReadOnlyMutation(t *testing.T) {
	environment := abilityTestEnvironment()
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_ability_state","owner":"$caster","ability":"$ability.self","property":"cast_active","operation":"set","value":false}},{"flow":"finish"}]}`
	json := abilityTestSkillJSON("readonly", `{"mode":"tap"}`, 0, flow)
	if _, diagnostics := Compile(mustParseJSON(t, json), environment); !diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestAbilityDisableOverlaysExpireIndependentlyAndDoNotCancelCast(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "disable", `{"mode":"tap"}`, 0, `{"flow":"wait","ticks":10,"then":{"flow":"finish"}}`)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	for _, duration := range []Tick{2, 4} {
		if _, err := runtime.ModifyAbilityState(1, handle, "enabled", "set", BoolRuntimeValue(false), duration, EventContext{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("disabled activation=%v", err)
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	assertAbilityEnabled(t, runtime, handle, false)
	if err := runtime.Advance(4); err != nil {
		t.Fatal(err)
	}
	assertAbilityEnabled(t, runtime, handle, true)
	active, _ := runtime.ReadAbilityState(1, handle, "cast_active")
	if value, _ := active.Bool(); !value {
		t.Fatal("disable overlay cancelled active cast")
	}
	if snapshot, ok := runtime.InspectCast(castID); !ok || snapshot.Status != CastSuspended {
		t.Fatalf("cast=%#v", snapshot)
	}
	if _, err := runtime.ModifyAbilityState(1, handle, "enabled", "set", BoolRuntimeValue(false), 6, EventContext{}); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("overlong overlay=%v", err)
	}
}

func TestAbilityAmmoMutationPreservesRechargeTimeline(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "ammo", `{"mode":"ammo","max_stock":3,"recharge_ticks":3,"initial_stock":3}`, 0, `{"flow":"finish"}`)
	newRuntime := func() (*Runtime, AbilityHandle) {
		runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		return runtime, runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	}

	runtime, handle := newRuntime()
	if _, err := runtime.ModifyAbilityState(1, handle, "ammo_stock", "add", IntRuntimeValue(99, quantityCount), 0, EventContext{}); err != nil {
		t.Fatal(err)
	}
	if runtime.scheduler.Len() != 1 { // cancelled recharge remains as one harmless tombstone
		t.Fatalf("scheduled tasks=%d", runtime.scheduler.Len())
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	assertAbilityAmmo(t, runtime, handle, 3)

	runtime, handle = newRuntime()
	state := runtime.skillStates[skillStateKey{Caster: 1, Skill: program.id}]
	due := state.rechargeDue
	if _, err := runtime.ModifyAbilityState(1, handle, "ammo_stock", "set", IntRuntimeValue(1, quantityCount), 0, EventContext{}); err != nil {
		t.Fatal(err)
	}
	if state.rechargeDue != due || runtime.scheduler.Len() != 1 {
		t.Fatalf("due=%d want=%d tasks=%d", state.rechargeDue, due, runtime.scheduler.Len())
	}
	if err := runtime.Advance(due); err != nil {
		t.Fatal(err)
	}
	assertAbilityAmmo(t, runtime, handle, 2)
}

func TestAbilityAmmoCancelledRechargeCannotAdvanceNewGeneration(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "ammo-generation", `{"mode":"ammo","max_stock":2,"recharge_ticks":3,"initial_stock":2}`, 0, `{"flow":"finish"}`)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	if _, err := runtime.ModifyAbilityState(1, handle, "ammo_stock", "add", IntRuntimeValue(1, quantityCount), 0, EventContext{}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	assertAbilityAmmo(t, runtime, handle, 1)
	if err := runtime.Advance(4); err != nil {
		t.Fatal(err)
	}
	assertAbilityAmmo(t, runtime, handle, 2)
}

func TestAbilityCastActiveClearsOnImmediateAndScheduledFailure(t *testing.T) {
	environment := abilityTestEnvironment()
	for _, test := range []struct {
		name string
		flow string
	}{
		{"immediate", `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}`},
		{"scheduled", `{"flow":"wait","ticks":1,"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			json := abilityTestSkillJSON("failure-"+test.name, `{"mode":"tap"}`, 0, test.flow)
			json = stringsReplaceOnce(t, json, `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`)
			program, diagnostics := Compile(mustParseJSON(t, json), environment)
			requireNoErrors(t, diagnostics)
			runtime := NewRuntime(&effectResultHost{MemoryHost: runtimeTestHost(environment), err: ErrHostContractViolation}, RuntimeOptions{})
			_, activationErr := runtime.Activate(program, CastInput{Caster: 1, Target: 999})
			if test.name == "scheduled" {
				if activationErr != nil {
					t.Fatal(activationErr)
				}
				activationErr = runtime.Advance(1)
			}
			if activationErr == nil {
				t.Fatal("expected cast failure")
			}
			handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
			active, err := runtime.ReadAbilityState(1, handle, "cast_active")
			if err != nil {
				t.Fatal(err)
			}
			if value, _ := active.Bool(); value {
				t.Fatal("failed cast remained active")
			}
		})
	}
}

func TestAbilityCastActiveClearsOnPulseFailure(t *testing.T) {
	program, environment := compilePolicySkill(t, "ability-pulse-failure", `{"mode":"toggle","pulse_interval_ticks":1,"max_duration_ticks":5,"sustain_costs":[]}`, 0)
	runtime := NewRuntime(&effectResultHost{MemoryHost: runtimeTestHost(environment), err: ErrHostContractViolation}, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 999}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(1); err == nil {
		t.Fatal("expected pulse failure")
	}
	handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	active, err := runtime.ReadAbilityState(1, handle, "cast_active")
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := active.Bool(); value {
		t.Fatal("pulse failure left cast active")
	}
}

func TestAbilitySelectFiltersStableOrderLimitAndAuthority(t *testing.T) {
	environment := abilityTestEnvironment()
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"ability","shape":{"type":"ability_set"},"filters":[{"type":"ability_tag","tag":"spell"},{"type":"ability_enabled"}],"order":{"by":"ability_slot","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"ability","do":{"flow":"effect","effect":{"type":"modify_ability_state","owner":"$caster","ability":"$local.ability","property":"cooldown_remaining_ticks","operation":"set","value":5}}}},{"flow":"finish"}]}`
	program := compileAbilityTestSkill(t, environment, "select", `{"mode":"tap"}`, 0, flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	tag := environment.Gameplay.Tags.Entries[0].Handle
	first, second := *program, *program
	first.id, second.id = "skill.test.ability.first", "skill.test.ability.second"
	first.cast.mode, second.cast.mode = castModeAmmo, castModeAmmo
	first.cast.maxStock, second.cast.maxStock = 3, 3
	first.cast.initialStock, second.cast.initialStock = 1, 0
	first.cast.rechargeTicks, second.cast.rechargeTicks = 3, 3
	for _, registration := range []AbilityRegistration{
		{Owner: 1, Handle: 30, Slot: 0, Tags: []GameplayTagHandle{tag}, Program: program},
		{Owner: 1, Handle: 10, Slot: 1, Tags: []GameplayTagHandle{tag}, Program: &first, AmmoStock: 1, AmmoMax: 3},
		{Owner: 1, Handle: 20, Slot: 1, Tags: []GameplayTagHandle{tag}, Program: &second, AmmoStock: 0, AmmoMax: 3},
	} {
		if err := runtime.RegisterAbility(registration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	events := runtime.RuntimeEvents()
	if len(events) != 2 || events[0].Ability.Ability != 30 || events[1].Ability.Ability != 10 {
		t.Fatalf("ability events=%#v", events)
	}
	runtime.abilities[abilityKey{owner: 1, handle: 20}].overlays[1] = 10

	base := selectorProgram{element: selectionAbility, shapePlan: shapeProgram{kind: "ability_set"}, from: referenceProgramValue{kind: referenceBuiltin, builtin: "$caster", typ: valueType{Base: valueKindEntity}}}
	for _, test := range []struct {
		name   string
		filter filterProgram
		want   []AbilityHandle
	}{
		{"tag", filterProgram{kind: "ability_tag", tag: tag}, []AbilityHandle{30, 10, 20}},
		{"slot", filterProgram{kind: "ability_slot", slot: 1}, []AbilityHandle{10, 20}},
		{"self", filterProgram{kind: "self_ability"}, []AbilityHandle{30}},
		{"not self", filterProgram{kind: "not_self_ability"}, []AbilityHandle{10, 20}},
		{"enabled", filterProgram{kind: "ability_enabled"}, []AbilityHandle{30, 10}},
		{"cooldown", filterProgram{kind: "ability_on_cooldown"}, []AbilityHandle{30, 10}},
		{"ammo", filterProgram{kind: "ability_has_ammo"}, []AbilityHandle{10}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := base
			selector.filters = []filterProgram{test.filter}
			selected, selectErr := runtime.selectAbilities(runtime.casts[1], selector)
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			got := make([]AbilityHandle, len(selected))
			for index := range selected {
				got[index] = selected[index].ability.Handle
			}
			if !abilityHandlesEqual(got, test.want) {
				t.Fatalf("selected=%v want=%v", got, test.want)
			}
		})
	}
	stable := base
	stable.order = selectOrderProgram{by: "stable_id", direction: "asc"}
	selected, err := runtime.selectAbilities(runtime.casts[1], stable)
	if err != nil {
		t.Fatal(err)
	}
	stableHandles := make([]AbilityHandle, len(selected))
	for index := range selected {
		stableHandles[index] = selected[index].ability.Handle
	}
	if !abilityHandlesEqual(stableHandles, []AbilityHandle{10, 20, 30}) {
		t.Fatalf("stable_id order=%v", stableHandles)
	}

	cast := &castInstance{caster: 1, ability: 30, program: program, inputs: []RuntimeValue{EntityRuntimeValue(2)}}
	selector := selectorProgram{element: selectionAbility, shapePlan: shapeProgram{kind: "ability_set"}, from: referenceProgramValue{kind: referenceInput, index: 0, typ: valueType{Base: valueKindEntity}}}
	if _, err := runtime.selectAbilities(cast, selector); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("enemy ability selection=%v", err)
	}
}

func TestAbilityDefaultCatalogAndSelectableTagPolicy(t *testing.T) {
	environment := DefaultCompileEnvironment()
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_ability_state","owner":"$caster","ability":"$ability.self","property":"enabled","operation":"set","value":false,"duration_ticks":1}},{"flow":"if","condition":{"read_ability_state":{"owner":"$caster","ability":"$ability.self","property":"cast_active","snapshot":"current"}},"then":{"flow":"finish"},"else":{"flow":"finish"}}]}`
	program := compileAbilityTestSkill(t, environment, "default-catalog", `{"mode":"tap"}`, 0, flow)
	if len(program.abilityProperties) != 8 {
		t.Fatalf("default ability properties=%d", len(program.abilityProperties))
	}

	invalidSelect := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"ability","shape":{"type":"ability_set"},"filters":[{"type":"ability_tag","tag":"projectile"}],"order":{"by":"ability_slot","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"ability","then":{"flow":"finish"}}},{"flow":"finish"}]}`
	if _, diagnostics := Compile(mustParseJSON(t, abilityTestSkillJSON("unselectable-tag", `{"mode":"tap"}`, 0, invalidSelect)), environment); !diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestAbilityCatalogAllowsAuthorizedAllyOwner(t *testing.T) {
	environment := abilityTestEnvironment()
	environment.Gameplay.Abilities.OwnerRelations = []string{"self", "ally"}
	environment.Digest = authorityDigest(environment)
	target := compileAbilityTestSkill(t, environment, "ally-target", `{"mode":"tap"}`, 0, `{"flow":"finish"}`)
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$input.target","kind":"ability","shape":{"type":"ability_set"},"filters":[],"order":{"by":"ability_slot","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"ability","then":{"flow":"effect","effect":{"type":"modify_ability_state","owner":"$input.target","ability":"$local.ability","property":"cooldown_remaining_ticks","operation":"set","value":5}}}},{"flow":"finish"}]}`
	json := abilityTestSkillJSON("ally-select", `{"mode":"tap"}`, 0, flow)
	json = stringsReplaceOnce(t, json, `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`)
	selector, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Relation: "ally"})
	runtime := NewRuntime(host, RuntimeOptions{})
	if err := runtime.RegisterAbility(AbilityRegistration{Owner: 2, Handle: 7, Slot: 0, Program: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate(selector, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	events := runtime.RuntimeEvents()
	if len(events) != 1 || events[0].Ability == nil || events[0].Ability.Owner != 2 || events[0].Ability.Ability != 7 {
		t.Fatalf("events=%#v", events)
	}
}

func TestAbilityRegistrationValidatesStoreInvariants(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "registration", `{"mode":"tap"}`, 0, `{"flow":"finish"}`)
	for name, registration := range map[string]AbilityRegistration{
		"negative slot":  {Owner: 1, Handle: 1, Slot: -1, Program: program},
		"non-ammo stock": {Owner: 1, Handle: 1, Slot: 0, Program: program, AmmoStock: 1, AmmoMax: 1},
		"unknown tag":    {Owner: 1, Handle: 1, Slot: 0, Program: program, Tags: []GameplayTagHandle{2}},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
			if err := runtime.RegisterAbility(registration); !errors.Is(err, ErrCastInputInvalid) {
				t.Fatalf("registration error=%v", err)
			}
		})
	}
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if err := runtime.RegisterAbility(AbilityRegistration{Owner: 1, Handle: 1, Slot: 0, Program: program}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RegisterAbility(AbilityRegistration{Owner: 1, Handle: 2, Slot: 1, Program: program}); !errors.Is(err, ErrCastInputInvalid) {
		t.Fatalf("duplicate program registration=%v", err)
	}
}

func abilityHandlesEqual(left, right []AbilityHandle) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func abilityTestEnvironment() CompileEnvironment {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Abilities = AbilityControlCatalog{Revision: "ability-test-1", SelectableTags: []GameplayTagHandle{1}, OwnerRelations: []string{"self"}, Properties: []AbilityPropertyPolicy{
		{Property: "cooldown_remaining_ticks", ValueType: valueKindInt, Mutable: true, Minimum: 0, Maximum: 60, MaximumMutation: 100},
		{Property: "cooldown_total_ticks", ValueType: valueKindInt},
		{Property: "ammo_stock", ValueType: valueKindInt, Mutable: true, Minimum: 0, Maximum: 10, MaximumMutation: 100},
		{Property: "ammo_max_stock", ValueType: valueKindInt},
		{Property: "enabled", ValueType: valueKindBool, Mutable: true, MaximumMutation: 1, MaximumDurationTicks: 5},
		{Property: "cast_active", ValueType: valueKindBool},
		{Property: "last_commit_tick", ValueType: valueKindInt},
		{Property: "last_finish_tick", ValueType: valueKindInt},
	}}
	environment.Digest = authorityDigest(environment)
	return environment
}

func compileAbilityTestSkill(t *testing.T, environment CompileEnvironment, id, policy string, cooldown Tick, flow string) *Program {
	t.Helper()
	program, diagnostics := Compile(mustParseJSON(t, abilityTestSkillJSON(id, policy, cooldown, flow)), environment)
	requireNoErrors(t, diagnostics)
	return program
}

func abilityTestSkillJSON(id, policy string, cooldown Tick, flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.ability.` + id + `","name":"Ability","description":"Ability state.","gameplay_tags":["spell"],"activation":{"type":"active","policy":` + policy + `},"input_schema":{"type":"none"},"cooldown_ticks":` + tickString(cooldown) + `,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}

func abilityMutationQuantity(operation string) quantityKind {
	if operation == "mul_bp" {
		return quantityBasisPoints
	}
	return quantityTicks
}

func countAbilityEvents(events []RuntimeEvent, property string) int {
	count := 0
	for _, event := range events {
		if event.Ability != nil && event.Ability.Property == property {
			count++
		}
	}
	return count
}

func assertAbilityEnabled(t *testing.T, runtime *Runtime, handle AbilityHandle, want bool) {
	t.Helper()
	value, err := runtime.ReadAbilityState(1, handle, "enabled")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := value.Bool()
	if got != want {
		t.Fatalf("enabled=%v want=%v", got, want)
	}
}

func assertAbilityAmmo(t *testing.T, runtime *Runtime, handle AbilityHandle, want int64) {
	t.Helper()
	value, err := runtime.ReadAbilityState(1, handle, "ammo_stock")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := value.Int()
	if got != want {
		t.Fatalf("ammo=%d want=%d", got, want)
	}
}
