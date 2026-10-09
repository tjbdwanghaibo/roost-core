package skill

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestOwnedEntitySummonRegistersAuthoritativeIdentityAndTypedResult(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":2,"duration_ticks":10},"result":{"as":"summoned","success":{"flow":"if","condition":{"op":"eq","args":["$local.summoned.first_entity","$local.summoned.first_entity"]},"then":{"flow":"finish"},"else":{"flow":"finish"}},"failure":{"flow":"finish"}}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "identity", flow)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	records := host.OwnedEntities(1)
	if len(records) != 2 {
		t.Fatalf("records=%#v", records)
	}
	for index, record := range records {
		if record.Owner != 1 || record.GameplayDigest != program.identity.gameplayDigest || record.SourceSkillID != program.id || record.SourceCastID != castID || record.SourceEffectIndex != 0 || record.Template != 1 || record.SummonSequence != uint64(index+1) || record.SummonTick != 0 || record.LifetimeTicks != 10 {
			t.Fatalf("record[%d]=%#v", index, record)
		}
	}
	records[0].GameplayTags[0] = 999
	if host.OwnedEntities(1)[0].GameplayTags[0] == 999 {
		t.Fatal("owned entity metadata aliased inspector result")
	}
}

func TestOwnedEntitySummonBindingsAreTypedClampedAndCapabilityChecked(t *testing.T) {
	environment := DefaultCompileEnvironment()
	template := &environment.Gameplay.UnitTemplates.Entries[0]
	template.AllowedAttributeOverrides = []UnitTemplateAttributeOverridePolicy{{Attribute: 2, Minimum: 10, Maximum: 100}}
	template.Parameters = []UnitTemplateParameterPolicy{
		{Name: "start_position", ValueType: valueKindPosition},
		{Name: "end_position", ValueType: valueKindPosition},
		{Name: "length", ValueType: valueKindInt, Quantity: quantityDimensionless, Minimum: 1, Maximum: 50},
	}
	template.DynamicCollider = true
	environment.Digest = authorityDigest(environment)
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10,"attribute_overrides":{"ability_power":200},"parameter_bindings":{"start_position":"$caster.position","end_position":"$caster.position","length":999}}},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.owned.bindings","name":"Bindings","description":"Typed summon bindings.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	record := host.OwnedEntities(1)[0]
	if host.AttributeForTest(record.Entity, 2) != 100 {
		t.Fatalf("attribute override=%d", host.AttributeForTest(record.Entity, 2))
	}
	length, ok := record.ParameterBindings["length"].Int()
	if !ok || length != 50 {
		t.Fatalf("parameter bindings=%#v", record.ParameterBindings)
	}
	record.ParameterBindings["length"] = IntRuntimeValue(1, quantityUnknown)
	if stored, _ := host.OwnedEntities(1)[0].ParameterBindings["length"].Int(); stored != 50 {
		t.Fatal("parameter binding inspector aliases host metadata")
	}

	t.Run("dynamic collider capability", func(t *testing.T) {
		invalid := environment
		invalid.Gameplay.UnitTemplates.Entries = append([]UnitTemplateCatalogEntry(nil), environment.Gameplay.UnitTemplates.Entries...)
		invalid.Gameplay.UnitTemplates.Entries[0].DynamicCollider = false
		invalid.Digest = authorityDigest(invalid)
		_, got := Compile(mustParseJSON(t, json), invalid)
		if !diagnosticsHaveErrors(got) {
			t.Fatal("expected DynamicCollider capability diagnostic")
		}
	})
	t.Run("unknown override", func(t *testing.T) {
		unknownJSON := strings.Replace(json, `"ability_power":200`, `"move_speed":200`, 1)
		_, got := Compile(mustParseJSON(t, unknownJSON), environment)
		if !diagnosticsHaveErrors(got) {
			t.Fatal("expected override policy diagnostic")
		}
	})
	t.Run("invalid override bounds", func(t *testing.T) {
		invalid := environment
		invalid.Gameplay.UnitTemplates.Entries = append([]UnitTemplateCatalogEntry(nil), environment.Gameplay.UnitTemplates.Entries...)
		invalid.Gameplay.UnitTemplates.Entries[0].AllowedAttributeOverrides = []UnitTemplateAttributeOverridePolicy{{Attribute: 2, Minimum: 101, Maximum: 100}}
		invalid.Digest = authorityDigest(invalid)
		_, got := Compile(mustParseJSON(t, json), invalid)
		if !diagnosticsHaveErrors(got) {
			t.Fatal("expected invalid override bounds diagnostic")
		}
	})
	t.Run("forged parameter quantity", func(t *testing.T) {
		host := runtimeTestHost(environment)
		command := ownedSummonCommand(Position{})
		command.GameplayDigest = environment.Digest
		command.ParameterBindings = []SummonParameterBinding{{Name: "length", Value: IntRuntimeValue(5, quantityWorldDistance)}}
		before := host.CurrentRevision()
		if _, err := host.Apply(EffectCommand{Payload: command}); err != ErrHostContractViolation {
			t.Fatalf("err=%v", err)
		}
		if host.CurrentRevision() != before || len(host.OwnedEntities(1)) != 0 {
			t.Fatal("invalid quantity mutated host state")
		}
	})
}

func TestOwnedReplacementPoliciesAreStableAndAtomic(t *testing.T) {
	tests := []struct {
		policy string
		want   []EntityID
	}{
		{"reject_new", []EntityID{2, 3}},
		{"replace_oldest", []EntityID{3, 4}},
		{"replace_newest", []EntityID{2, 4}},
		{"replace_nearest", []EntityID{3, 4}},
		{"replace_farthest", []EntityID{2, 4}},
	}
	for _, test := range tests {
		t.Run(test.policy, func(t *testing.T) {
			host := ownedPolicyHost(test.policy, 2)
			summonOwnedForTest(t, host, Position{X: 0})
			summonOwnedForTest(t, host, Position{X: 100})
			before := host.CurrentRevision()
			result, err := host.Apply(EffectCommand{Payload: ownedSummonCommand(Position{X: 10})})
			if err != nil {
				t.Fatal(err)
			}
			if test.policy == "reject_new" {
				outcome := result.Payload.(SummonEffectResult)
				if outcome.Succeeded || outcome.FailureReason != ExpectedFailureCapacityReached || host.CurrentRevision() != before {
					t.Fatalf("reject result=%#v revision=%d/%d", result, host.CurrentRevision(), before)
				}
			}
			got := ownedEntityIDs(host.OwnedEntities(1))
			if !equalEntityIDs(got, test.want) {
				t.Fatalf("ids=%v want=%v", got, test.want)
			}
		})
	}
}

func TestOwnedReplacementDistanceTieUsesSummonSequenceThenEntityID(t *testing.T) {
	records := []OwnedEntityMetadata{{Entity: 2, SummonSequence: 2}, {Entity: 9, SummonSequence: 1}}
	entities := map[EntityID]MemoryEntity{2: {ID: 2, Position: Position{X: 10}}, 9: {ID: 9, Position: Position{X: -10}}}
	sortOwnedReplacementCandidates(records, "replace_nearest", Position{}, entities)
	if records[0].Entity != 9 {
		t.Fatalf("tie order=%#v", records)
	}
}

func TestOwnedReplacementLimitsPerSourceSkillAndTeam(t *testing.T) {
	t.Run("source skill", func(t *testing.T) {
		catalog := defaultGameplayCatalog()
		catalog.UnitTemplates.Entries[0].ReplacementPolicy = "reject_new"
		catalog.UnitTemplates.Entries[0].MaximumPerOwner = 3
		catalog.UnitTemplates.Entries[0].MaximumPerSourceSkill = 1
		host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
		host.ConfigureGameplayCatalog(catalog)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
		summonOwnedForTest(t, host, Position{})
		result, err := host.Apply(EffectCommand{Payload: ownedSummonCommand(Position{X: 1})})
		if err != nil || result.Payload.(SummonEffectResult).FailureReason != ExpectedFailureCapacityReached || len(host.OwnedEntities(1)) != 1 {
			t.Fatalf("source limit result=%#v err=%v", result, err)
		}
	})
	t.Run("team", func(t *testing.T) {
		catalog := defaultGameplayCatalog()
		catalog.UnitTemplates.Entries[0].ReplacementPolicy = "reject_new"
		catalog.UnitTemplates.Entries[0].MaximumPerOwner = 2
		catalog.UnitTemplates.Entries[0].MaximumPerSourceSkill = 2
		catalog.UnitTemplates.Entries[0].MaximumPerTeam = 1
		host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
		host.ConfigureGameplayCatalog(catalog)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, TeamID: 7})
		host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, TeamID: 7})
		first := ownedSummonCommand(Position{})
		if result, err := host.Apply(EffectCommand{Payload: first}); err != nil || !result.Payload.(SummonEffectResult).Succeeded {
			t.Fatalf("first=%#v err=%v", result, err)
		}
		second := ownedSummonCommand(Position{X: 1})
		second.Owner = 2
		second.SourceSkillID = "other"
		result, err := host.Apply(EffectCommand{Payload: second})
		if err != nil || result.Payload.(SummonEffectResult).FailureReason != ExpectedFailureCapacityReached || len(host.OwnedEntities(0)) != 1 {
			t.Fatalf("team limit result=%#v err=%v", result, err)
		}
	})
}

func TestOwnedEntityOwnerDeathLifecyclePolicy(t *testing.T) {
	for _, test := range []struct {
		policy      string
		wantAtTick1 int
	}{
		{policy: "dismiss", wantAtTick1: 0},
		{policy: "persist_until_duration", wantAtTick1: 1},
	} {
		t.Run(test.policy, func(t *testing.T) {
			catalog := defaultGameplayCatalog()
			catalog.UnitTemplates.Entries[0].OwnerDeathPolicy = test.policy
			host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
			host.ConfigureGameplayCatalog(catalog)
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
			command := ownedSummonCommand(Position{})
			command.DurationTicks = 3
			if _, err := host.Apply(EffectCommand{Payload: command}); err != nil {
				t.Fatal(err)
			}
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: false})
			if _, err := host.Advance(1); err != nil {
				t.Fatal(err)
			}
			if got := len(host.OwnedEntities(1)); got != test.wantAtTick1 {
				t.Fatalf("tick 1 entities=%d", got)
			}
			if _, err := host.Advance(3); err != nil {
				t.Fatal(err)
			}
			if got := len(host.OwnedEntities(1)); got != 0 {
				t.Fatalf("tick 3 entities=%d", got)
			}
		})
	}
}

func TestOwnedEntitySelectFiltersAndStableOrder(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"owned_entities"},"filters":[{"type":"source_skill","skill":"skill.test.owned.select"},{"type":"source_cast","cast":1},{"type":"summoned_before","tick":1},{"type":"unit_template","template":"deployable.trap"},{"type":"entity_tag","tag":"spell"}],"order":{"by":"summon_sequence","direction":"desc"},"limit":2},"consume":{"mode":"each","as":"owned","do":{"flow":"effect","effect":{"type":"add_memory","name":"count","value":1}}},"on_empty":{"flow":"finish"}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "select", flow)
	host := runtimeTestHost(environment)
	for index := 0; index < 3; index++ {
		command := ownedSummonCommand(Position{X: int64(index)})
		command.SourceSkillID = program.id
		command.GameplayDigest = program.identity.gameplayDigest
		if _, err := host.Apply(EffectCommand{Payload: command}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	count, _ := runtime.casts[castID].memory[0].Int()
	if count != 2 {
		t.Fatalf("selected count=%d", count)
	}
}

func TestOwnedEntitySelectSupportsEveryStableOrder(t *testing.T) {
	host := ownedPolicyHost("reject_new", 8)
	positions := []Position{{X: 30}, {X: 10}, {X: 20}}
	durations := []Tick{20, 30, 40}
	for index := range positions {
		command := ownedSummonCommand(positions[index])
		command.DurationTicks = durations[index]
		if _, err := host.Apply(EffectCommand{Payload: command}); err != nil {
			t.Fatal(err)
		}
		if index < len(positions)-1 {
			if _, err := host.Advance(Tick(index + 1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		order SelectOrderBy
		dir   SelectDirection
		want  []EntityID
	}{
		{SelectOrderEntityID, SelectAscending, []EntityID{2, 3, 4}},
		{SelectOrderSummonTick, SelectAscending, []EntityID{2, 3, 4}},
		{SelectOrderSummonSequence, SelectDescending, []EntityID{4, 3, 2}},
		{SelectOrderDistanceToOwner, SelectAscending, []EntityID{3, 4, 2}},
		{SelectOrderRemainingLifetime, SelectAscending, []EntityID{2, 3, 4}},
	}
	for _, test := range tests {
		t.Run(string(test.order), func(t *testing.T) {
			result, err := host.Select(SelectRequest{Caster: 1, ElementKind: "entity", Shape: OwnedEntitiesSelectShape{Owner: 1}, Order: SelectOrder{By: test.order, Direction: test.dir}, Limit: 8})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]EntityID, len(result.Selection.elements))
			for index, element := range result.Selection.elements {
				got[index] = element.entity
			}
			if !equalEntityIDs(got, test.want) {
				t.Fatalf("got=%v want=%v", got, test.want)
			}
		})
	}
}

func TestEntityCommandEnforcesOwnershipAndControlProfile(t *testing.T) {
	host := ownedPolicyHost("reject_new", 2)
	entity := summonOwnedForTest(t, host, Position{})
	success, err := host.Apply(EffectCommand{Payload: OwnedEntityCommand{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "hold_position"}})
	if err != nil || !success.Payload.(EntityCommandEffectResult).Succeeded {
		t.Fatalf("success=%#v err=%v", success, err)
	}
	for name, command := range map[string]OwnedEntityCommand{
		"permission": {Owner: 2, GameplayDigest: "digest", Target: entity, Command: "hold_position"},
		"command":    {Owner: 1, GameplayDigest: "digest", Target: entity, Command: "attack_target", TargetEntity: 2},
		"behavior":   {Owner: 1, GameplayDigest: "digest", Target: entity, Command: "invoke_behavior", Behavior: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			result, applyErr := host.Apply(EffectCommand{Payload: command})
			if applyErr != nil || result.Payload.(EntityCommandEffectResult).Succeeded {
				t.Fatalf("result=%#v err=%v", result, applyErr)
			}
		})
	}
}

func TestEntityCommandClosedSet(t *testing.T) {
	catalog := defaultGameplayCatalog()
	catalog.UnitTemplates.Entries[0].Commands = []string{"move_to", "follow", "attack_target", "hold_position", "return_to_owner", "stop", "invoke_behavior", "dismiss"}
	catalog.UnitTemplates.Entries[0].Behaviors = []string{"armed"}
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 5}})
	host.UpsertEntity(MemoryEntity{ID: 99, Alive: true})
	entity := summonOwnedForTest(t, host, Position{})
	commands := []OwnedEntityCommand{
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "move_to", Position: Position{X: 10}},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "follow", TargetEntity: 99},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "attack_target", TargetEntity: 99},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "hold_position"},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "return_to_owner"},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "stop"},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "invoke_behavior", Behavior: "armed"},
		{Owner: 1, GameplayDigest: "digest", Target: entity, Command: "dismiss"},
	}
	for _, command := range commands {
		result, err := host.Apply(EffectCommand{Payload: command})
		if err != nil || !result.Payload.(EntityCommandEffectResult).Succeeded {
			t.Fatalf("command %s result=%#v err=%v", command.Command, result, err)
		}
	}
}

func TestOwnedSpawnHandoffAndPreHandoffCancellation(t *testing.T) {
	spawnCallbacks := `"on":{"tick":{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}}`
	finishedFlow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},` + spawnCallbacks + `},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "handoff", finishedFlow)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	owned := runtime.OwnedSpawns(1)
	if len(owned) != 1 || !owned[0].HandedOff || owned[0].LifecycleEntity == 0 {
		t.Fatalf("owned spawns=%#v", owned)
	}

	cancelFlow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},` + spawnCallbacks + `},{"flow":"wait","ticks":5,"then":{"flow":"finish"}}]}`
	cancelProgram, _ := compileOwnedSkill(t, "cancel", cancelFlow)
	cancelHost := runtimeTestHost(environment)
	cancelRuntime := NewRuntime(cancelHost, RuntimeOptions{})
	castID, err := cancelRuntime.Activate(cancelProgram, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelRuntime.Cancel(castID); err != nil {
		t.Fatal(err)
	}
	if len(cancelRuntime.OwnedSpawns(1)) != 0 {
		t.Fatal("cancelled cast handed off its entity spawn")
	}
}

func TestOwnedSpawnCapacityFailureDoesNotMutateHost(t *testing.T) {
	spawnCallbacks := `"on":{"tick":{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},` + spawnCallbacks + `},{"flow":"finish"}]}`
	first, environment := compileOwnedSkill(t, "capacity-first", flow)
	second, _ := compileOwnedSkill(t, "capacity-second", flow)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MaxOwnedSpawns: 1})
	if _, err := runtime.Activate(first, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	beforeRevision := host.CurrentRevision()
	beforeEntities := ownedEntityIDs(host.OwnedEntities(1))
	if _, err := runtime.Activate(second, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if got := ownedEntityIDs(host.OwnedEntities(1)); !equalEntityIDs(got, beforeEntities) {
		t.Fatalf("entities=%v want=%v", got, beforeEntities)
	}
	if host.CurrentRevision() != beforeRevision || len(runtime.OwnedSpawns(1)) != 1 {
		t.Fatalf("capacity failure mutated state: revision=%d/%d spawns=%#v", host.CurrentRevision(), beforeRevision, runtime.OwnedSpawns(1))
	}
}

func TestOwnedSpawnReplacementReleasesCapacityAtomically(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	spawnCallbacks := `"on":{"tick":` + callback + `,"cancel":` + callback + `}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},` + spawnCallbacks + `},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "capacity-replacement", flow)
	host := runtimeTestHost(environment)
	catalog := environment.Gameplay
	catalog.UnitTemplates.Entries = append([]UnitTemplateCatalogEntry(nil), environment.Gameplay.UnitTemplates.Entries...)
	catalog.UnitTemplates.Entries[0].MaximumPerOwner = 1
	catalog.UnitTemplates.Entries[0].MaximumPerSourceSkill = 1
	host.ConfigureGameplayCatalog(catalog)
	runtime := NewRuntime(host, RuntimeOptions{MaxOwnedSpawns: 1})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	first := runtime.OwnedSpawns(1)[0]
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	spawns := runtime.OwnedSpawns(1)
	entities := host.OwnedEntities(1)
	if len(spawns) != 1 || len(entities) != 1 || spawns[0].ID == first.ID || spawns[0].LifecycleEntity != entities[0].Entity {
		t.Fatalf("spawns=%#v entities=%#v", spawns, entities)
	}
	if countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_cancel") != 1 {
		t.Fatalf("replacement callbacks=%#v", runtime.RuntimeEvents())
	}
}

func TestOwnedSpawnStartFailureRollsBackReplacement(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "rollback-replacement", flow)
	base := runtimeTestHost(environment)
	catalog := environment.Gameplay
	catalog.UnitTemplates.Entries = append([]UnitTemplateCatalogEntry(nil), environment.Gameplay.UnitTemplates.Entries...)
	catalog.UnitTemplates.Entries[0].MaximumPerOwner = 1
	catalog.UnitTemplates.Entries[0].MaximumPerSourceSkill = 1
	catalog.UnitTemplates.Entries[0].Commands = []string{"hold_position"}
	base.ConfigureGameplayCatalog(catalog)
	host := &ownedSpawnTestHost{MemoryHost: base}
	runtime := NewRuntime(host, RuntimeOptions{MaxOwnedSpawns: 1})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	beforeEntity := host.OwnedEntities(1)[0].Entity
	beforeSpawn := runtime.OwnedSpawns(1)[0].ID
	host.failNextStep = true
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err == nil {
		t.Fatal("expected spawn start failure")
	}
	entities := host.OwnedEntities(1)
	spawns := runtime.OwnedSpawns(1)
	if len(entities) != 1 || entities[0].Entity != beforeEntity || len(spawns) != 1 || spawns[0].ID != beforeSpawn {
		t.Fatalf("entities=%#v spawns=%#v", entities, spawns)
	}
	if len(host.ownedTransactions) != 0 {
		t.Fatalf("pending transactions=%#v", host.ownedTransactions)
	}
}

func TestOwnedSpawnSignalsUseCanonicalOrderAndTargetContext(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"hit":` + callback + `,"collision":` + callback + `,"enter":` + callback + `,"tick":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "signal-order", flow)
	host := &ownedSpawnTestHost{MemoryHost: runtimeTestHost(environment), startSignals: []SpawnSignal{{Kind: SpawnSignalHit, Target: 99}}, tickSignals: []SpawnSignal{{Kind: SpawnSignalCollision, Target: 98}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"owned_spawn_callback_hit", "owned_spawn_callback_enter", "owned_spawn_callback_collision", "owned_spawn_callback_tick"}
	wantTargets := []EntityID{99, runtime.OwnedSpawns(1)[0].LifecycleEntity, 98, runtime.OwnedSpawns(1)[0].LifecycleEntity}
	events := runtime.RuntimeEvents()
	callbacks := make([]RuntimeEvent, 0, len(wantKinds))
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "owned_spawn_callback_") {
			callbacks = append(callbacks, event)
		}
	}
	if len(callbacks) != len(wantKinds) {
		t.Fatalf("callbacks=%#v", callbacks)
	}
	for index := range wantKinds {
		if callbacks[index].Kind != wantKinds[index] || callbacks[index].Context.Target != wantTargets[index] {
			t.Fatalf("callback[%d]=%#v want kind=%s target=%d", index, callbacks[index], wantKinds[index], wantTargets[index])
		}
	}
}

// 没有召唤物契约（不实现 OwnedEntityRuntimeHost）的 Host 上，summon 必须 fail closed，召唤物不能到达 Host。
//
// B3 ③ 收尾改写：原用例的 Host 不声明能力表，Runtime 的准入核对因此被跳过，Program 一路执行到
// summon 才由施法中途的类型断言拒绝（ErrHostContractViolation，已扣费）。能力表并入 Host 接口后
// 每个 Host 都要声明，这条承诺分成两层：
//   - 如实声明（表里没有 summon）的 Host：在 Activate 准入处就被拒绝（ErrHostCapabilityMissing，
//     同时 errors.Is ErrHostContractViolation），什么都没执行；
//   - 谎报 summon 的 Host：准入放行，施法中途的类型断言仍然兜底拒绝（原用例的断言原样保留）。
//     这种 Host 由 CheckHostCapabilities 在业务测试里点名（TestCheckHostCapabilitiesCatchesMisdeclaredHosts）。
func TestOwnedEntityRuntimeFailsClosedWithoutOwnedHostContract(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "host-contract", flow)

	honest := runtimeTestHost(environment)
	_, err := NewRuntime(&hostWithoutOwnedContract{inner: honest}, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("honest host without summons: err=%v, want ErrHostCapabilityMissing at admission", err)
	}
	if len(honest.OwnedEntities(1)) != 0 {
		t.Fatal("owned summon reached a host without the required contract")
	}

	lying := runtimeTestHost(environment)
	if _, err := NewRuntime(&declaredSummonWithoutContract{hostWithoutOwnedContract{inner: lying}}, RuntimeOptions{}).Activate(program, CastInput{Caster: 1}); err != ErrHostContractViolation {
		t.Fatalf("host declaring summon without the contract: err=%v, want ErrHostContractViolation from the in-cast check", err)
	}
	if len(lying.OwnedEntities(1)) != 0 {
		t.Fatal("owned summon reached a host without the required contract")
	}
}

// RemoveProgram 停不下移交后的衍生物：记录留成 stop_pending（停止入口统一，维护者 2026-10-07；之前留成 running、
// 列在 OwnedSpawns 里等调用方重试），不再被推进；调用方再请求一次（同一个 RemoveProgram）会立即停掉它，
// 不请求则由 Runtime 在 tick 上重试（TestEveryStopEntryDefersARefusedStopTheSameWay）。
func TestOwnedSpawnStopFailureRemainsTrackedForRetry(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"cancel":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "stop-retry", flow)
	host := &ownedSpawnTestHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	spawnID := runtime.OwnedSpawns(1)[0].ID
	host.failNextStop = true
	if err := runtime.RemoveProgram(program.id); err == nil {
		t.Fatal("expected stop failure")
	}
	if spawns := runtime.OwnedSpawns(1); len(spawns) != 0 || runtime.spawns.get(spawnID).Status != SpawnStopPending || !host.spawns[spawnID].active {
		t.Fatalf("spawns=%#v record=%q host=%#v, want the record kept as stop_pending and the host spawn still running", spawns, runtime.spawns.get(spawnID).Status, host.spawns[spawnID])
	}
	if err := runtime.RemoveProgram(program.id); err != nil {
		t.Fatal(err)
	}
	if runtime.spawns.get(spawnID).liveOnHost() || host.spawns[spawnID].active {
		t.Fatal("retry did not stop tracked spawn")
	}
	if callbacks := countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_cancel"); callbacks != 1 {
		t.Fatalf("cancel callbacks = %d, want one: the second request only re-issues the host stop", callbacks)
	}
}

func TestOwnedSpawnCleanupBoundaries(t *testing.T) {
	spawnFlow := func(duration Tick) string {
		callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
		return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":` + strconv.Itoa(int(duration)) + `},"on":{"tick":` + callback + `,"end":` + callback + `,"cancel":` + callback + `}},{"flow":"finish"}]}`
	}
	t.Run("lifecycle entity", func(t *testing.T) {
		program, environment := compileOwnedSkill(t, "cleanup-entity", spawnFlow(10))
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		spawn := runtime.OwnedSpawns(1)[0]
		if _, err := host.Apply(EffectCommand{Payload: OwnedEntityCommand{Owner: 1, GameplayDigest: program.identity.gameplayDigest, Target: spawn.LifecycleEntity, Command: "dismiss"}}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Advance(1); err != nil {
			t.Fatal(err)
		}
		if len(runtime.OwnedSpawns(1)) != 0 {
			t.Fatal("spawn survived lifecycle entity dismiss")
		}
		if countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_cancel") != 1 || countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_end") != 0 {
			t.Fatalf("lifecycle callbacks=%#v", runtime.RuntimeEvents())
		}
	})
	t.Run("duration", func(t *testing.T) {
		program, environment := compileOwnedSkill(t, "cleanup-duration", spawnFlow(3))
		runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Advance(3); err != nil {
			t.Fatal(err)
		}
		if len(runtime.OwnedSpawns(1)) != 0 {
			t.Fatal("spawn survived bounded duration")
		}
		if countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_tick") != 2 || countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_end") != 1 || countRuntimeEventKind(runtime.RuntimeEvents(), "owned_spawn_callback_cancel") != 0 {
			t.Fatalf("duration callbacks=%#v", runtime.RuntimeEvents())
		}
	})
	t.Run("program removal and shutdown", func(t *testing.T) {
		program, environment := compileOwnedSkill(t, "cleanup-runtime", spawnFlow(10))
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.RemoveProgram(program.id); err != nil || len(runtime.OwnedSpawns(1)) != 0 || len(host.OwnedEntities(1)) != 0 {
			t.Fatalf("remove program: err=%v spawns=%#v entities=%#v", err, runtime.OwnedSpawns(1), host.OwnedEntities(1))
		}
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Shutdown(); err != nil || len(runtime.OwnedSpawns(1)) != 0 || len(host.OwnedEntities(1)) != 0 {
			t.Fatalf("shutdown: err=%v spawns=%#v entities=%#v", err, runtime.OwnedSpawns(1), host.OwnedEntities(1))
		}
	})
}

func countRuntimeEventKind(events []RuntimeEvent, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func TestOwnedSpawnDetachedScopeRejectsCastDependencies(t *testing.T) {
	tests := map[string]string{
		"cast memory":   `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"if","condition":{"op":"eq","args":["$memory.count",0]},"then":{"flow":"goto","phase":"cast"},"else":{"flow":"goto","phase":"cast"}}}}`,
		"input":         `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"effect","effect":{"type":"issue_entity_command","target":"$caster","command":"attack_target","target_entity":"$input.target"}}}}`,
		"goto":          `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"goto","phase":"cast"}}}`,
		"finish":        `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"finish"}}}`,
		"recursive":     `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"finish"}}}}}`,
		"cast builtin":  `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"if","condition":{"op":"eq","args":["$cast.elapsed_ticks",1]},"then":{"flow":"finish"},"else":{"flow":"finish"}}}}`,
		"cast snapshot": `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"on":{"tick":{"flow":"effect","effect":{"type":"damage","target":"$lifecycle_entity","amount":{"read_attribute":{"entity":"$owner","attribute":"ability_power","snapshot":"cast_start"}},"damage_type":"physical"}}}}`,
	}
	for name, flow := range tests {
		t.Run(name, func(t *testing.T) {
			json := `{"schema":"roost.skill/v2","id":"skill.test.owned.detached","name":"Detached","description":"Detached scope.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{"count":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
			_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatal("expected detached scope diagnostic")
			}
		})
	}
}

func compileOwnedSkill(t *testing.T, id, flow string) (*Program, CompileEnvironment) {
	t.Helper()
	json := `{"schema":"roost.skill/v2","id":"skill.test.owned.` + id + `","name":"Owned","description":"Owned entities.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"count":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	return program, environment
}

func ownedPolicyHost(policy string, maximum int) *MemoryHost {
	catalog := defaultGameplayCatalog()
	catalog.UnitTemplates.Entries[0].ReplacementPolicy = policy
	catalog.UnitTemplates.Entries[0].MaximumPerOwner = maximum
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
	return host
}

func ownedSummonCommand(position Position) SummonCommand {
	return SummonCommand{Owner: 1, GameplayDigest: "digest", SourceSkillID: "skill", SourceCastID: 1, SourceEffectIndex: 0, Template: 1, Position: position, Count: 1, DurationTicks: 10, GameplayTags: []GameplayTagHandle{1}}
}

func summonOwnedForTest(t *testing.T, host *MemoryHost, position Position) EntityID {
	t.Helper()
	result, err := host.Apply(EffectCommand{Payload: ownedSummonCommand(position)})
	if err != nil {
		t.Fatal(err)
	}
	payload := result.Payload.(SummonEffectResult)
	if !payload.Succeeded || len(payload.Entities) != 1 {
		t.Fatalf("summon=%#v", result)
	}
	return payload.FirstEntity
}

func ownedEntityIDs(records []OwnedEntityMetadata) []EntityID {
	result := make([]EntityID, len(records))
	for index, record := range records {
		result[index] = record.Entity
	}
	return result
}

func equalEntityIDs(left, right []EntityID) bool {
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

type ownedSpawnTestHost struct {
	*MemoryHost
	failNextStep bool
	failNextStop bool
	stepCount    int
	startSignals []SpawnSignal
	tickSignals  []SpawnSignal
}

func (host *ownedSpawnTestHost) StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error) {
	if host.failNextStop {
		host.failNextStop = false
		return CommitReceipt{}, ErrProgramInvariant
	}
	return host.MemoryHost.StopSpawn(command, state)
}

func (host *ownedSpawnTestHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if host.failNextStep {
		host.failNextStep = false
		return SpawnStepResult{}, ErrProgramInvariant
	}
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err != nil {
		return result, err
	}
	if _, complete := command.Motion.(StaticMotionStep); complete {
		host.stepCount++
		if host.stepCount == 1 {
			result.Signals = append(result.Signals, host.startSignals...)
		} else {
			result.Signals = append(result.Signals, host.tickSignals...)
		}
		result.Signals = normalizeSpawnSignals(result.Signals)
	}
	return result, nil
}

// hostWithoutOwnedContract 不实现 OwnedEntityRuntimeHost，能力表如实不含召唤物与 minion。
type hostWithoutOwnedContract struct{ inner *MemoryHost }

func (host *hostWithoutOwnedContract) HostCapabilities() HostCapabilityTable {
	return withoutHostCapabilities(host.inner.HostCapabilities(), HostCapability{Kind: HostCapabilitySummon}, HostCapability{HostCapabilitySpawnKind, "minion"})
}

func (host *hostWithoutOwnedContract) AuthorityIdentity() AuthorityIdentity {
	return host.inner.AuthorityIdentity()
}
func (host *hostWithoutOwnedContract) ReadState(request StateReadRequest) (StateReadResult, error) {
	return host.inner.ReadState(request)
}
func (host *hostWithoutOwnedContract) ModifyState(command StateMutationCommand) (StateMutationResult, error) {
	return host.inner.ModifyState(command)
}
func (host *hostWithoutOwnedContract) Advance(tick Tick) (WorldRevision, error) {
	return host.inner.Advance(tick)
}
func (host *hostWithoutOwnedContract) CurrentRevision() WorldRevision {
	return host.inner.CurrentRevision()
}
func (host *hostWithoutOwnedContract) Read(request ReadRequest) (ReadResult, error) {
	return host.inner.Read(request)
}
func (host *hostWithoutOwnedContract) Select(request SelectRequest) (SelectResult, error) {
	return host.inner.Select(request)
}
func (host *hostWithoutOwnedContract) PayCosts(payment CostPayment) (CommitReceipt, error) {
	return host.inner.PayCosts(payment)
}
func (host *hostWithoutOwnedContract) Apply(command EffectCommand) (EffectResult, error) {
	return host.inner.Apply(command)
}
func (host *hostWithoutOwnedContract) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	return host.inner.StepSpawn(command, state)
}
func (host *hostWithoutOwnedContract) StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error) {
	return host.inner.StopSpawn(command, state)
}
func (host *hostWithoutOwnedContract) Events(after EventCursor) []RuntimeEvent {
	return host.inner.Events(after)
}
