package skill

import "testing"

func TestStatusSelectFiltersAndStableOrders(t *testing.T) {
	wireFlow := `{"flow":"select","select":{"from":"$input.target","kind":"status_instance","shape":{"type":"status_set"},"filters":[{"type":"status_id","status":"slow"},{"type":"status_category","category":"control"},{"type":"status_tag","tag":"spell"},{"type":"status_polarity","polarity":"negative"},{"type":"status_dispellable"},{"type":"status_transferable"},{"type":"status_copyable"},{"type":"status_source","source":"$caster"},{"type":"status_owner","owner":"$input.target"},{"type":"status_source_skill","skill":"skill.test"},{"type":"status_stack_compare","op":"gte","value":1},{"type":"status_duration_compare","op":"gt","value":1}],"order":{"by":"status_dispel_priority","direction":"desc"},"limit":1},"consume":{"mode":"one","as":"status","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`
	compileStatusSkill(t, "select-wire", wireFlow)
	host := statusInstanceHost()
	first := applyStatusForTest(t, host, 10, 1, 1, "skill.a", 1, 20)
	if _, err := host.Advance(1); err != nil {
		t.Fatal(err)
	}
	second := applyStatusForTest(t, host, 10, 3, 2, "skill.b", 3, 10)
	filters := []struct {
		name   string
		filter SelectFilter
		want   StatusInstanceID
	}{
		{"id", StatusIDSelectFilter{Status: 1}, first.ID},
		{"category", StatusTextSelectFilter{Kind: "status_category", Value: "buff"}, second.ID},
		{"tag", StatusTextSelectFilter{Kind: "status_tag", Tag: 1}, first.ID},
		{"polarity", StatusTextSelectFilter{Kind: "status_polarity", Value: "positive"}, second.ID},
		{"dispellable", StatusFlagSelectFilter{Kind: "status_dispellable"}, first.ID},
		{"transferable", StatusFlagSelectFilter{Kind: "status_transferable"}, first.ID},
		{"copyable", StatusFlagSelectFilter{Kind: "status_copyable"}, first.ID},
		{"source", StatusEntitySelectFilter{Kind: "status_source", Entity: 2}, second.ID},
		{"owner", StatusEntitySelectFilter{Kind: "status_owner", Entity: 10}, first.ID},
		{"source skill", StatusSourceSkillSelectFilter{SkillID: "skill.b"}, second.ID},
		{"stacks", StatusCompareSelectFilter{Kind: "status_stack_compare", Operation: "gte", Value: 2}, second.ID},
		{"duration", StatusCompareSelectFilter{Kind: "status_duration_compare", Operation: "gt", Value: 15}, first.ID},
	}
	for _, test := range filters {
		t.Run(test.name, func(t *testing.T) {
			result, err := host.Select(SelectRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, ElementKind: "status_instance", Shape: StatusSetSelectShape{Target: 10}, Filters: []SelectFilter{test.filter}, Order: SelectOrder{By: SelectOrderStatusInstanceID, Direction: SelectAscending}, Limit: 8})
			if err != nil {
				t.Fatal(err)
			}
			refs := result.Selection.StatusInstances()
			if test.name == "owner" {
				if len(refs) != 2 || refs[0].ID != first.ID || refs[1].ID != second.ID {
					t.Fatalf("refs=%#v", refs)
				}
				return
			}
			if len(refs) != 1 || refs[0].ID != test.want {
				t.Fatalf("refs=%#v", refs)
			}
		})
	}
	orders := []struct {
		by        SelectOrderBy
		direction SelectDirection
		want      []StatusInstanceID
	}{
		{SelectOrderStatusDispelPriority, SelectAscending, []StatusInstanceID{first.ID, second.ID}},
		{SelectOrderRemainingDuration, SelectAscending, []StatusInstanceID{second.ID, first.ID}},
		{SelectOrderStackCount, SelectDescending, []StatusInstanceID{second.ID, first.ID}},
		{SelectOrderAppliedTick, SelectAscending, []StatusInstanceID{first.ID, second.ID}},
		{SelectOrderStatusInstanceID, SelectDescending, []StatusInstanceID{second.ID, first.ID}},
	}
	for _, test := range orders {
		result, err := host.Select(SelectRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, ElementKind: "status_instance", Shape: StatusSetSelectShape{Target: 10}, Order: SelectOrder{By: test.by, Direction: test.direction}, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		refs := result.Selection.StatusInstances()
		for index := range test.want {
			if refs[index].ID != test.want[index] {
				t.Fatalf("order %s refs=%#v", test.by, refs)
			}
		}
	}
}

func TestStatusOperationClosedSetClampCopyTransferAndExpiry(t *testing.T) {
	operations := map[string]string{
		"remove": "", "add_stacks": `,"value":1`, "set_stacks": `,"value":1`,
		"add_duration": `,"value":1`, "set_duration": `,"value":1`, "mul_duration_bp": `,"value":10000`, "refresh": "",
		"copy_to": `,"target":"$caster","ownership_policy":"original_source"`, "transfer_to": `,"target":"$caster","ownership_policy":"new_source"`,
	}
	for operation, arguments := range operations {
		body := `{"flow":"effect","effect":{"type":"modify_status_instance","status":"$local.status","operation":"` + operation + `"` + arguments + `}}`
		flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$input.target","kind":"status_instance","shape":{"type":"status_set"},"filters":[],"order":{"by":"status_instance_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"status","then":` + body + `},"on_empty":{"flow":"finish"}},{"flow":"finish"}]}`
		compileStatusSkill(t, "operation-"+operation, flow)
	}
	host := statusInstanceHost()
	ref := applyStatusForTest(t, host, 10, 1, 1, "skill.a", 2, 10)
	apply := func(operation string, value int64, target EntityID, policy string) StatusEffectResult {
		t.Helper()
		command := ModifyStatusInstanceCommand{Owner: 1, SourceSkillID: "skill.a", Status: ref, Operation: operation, Value: value, Target: target, OwnershipPolicy: policy}
		result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: command})
		if err != nil {
			t.Fatal(err)
		}
		return result.Payload.(StatusEffectResult)
	}
	if got := apply("add_stacks", 99, 0, ""); !got.Succeeded || got.Result.CurrentStacks != 5 {
		t.Fatalf("add=%#v", got)
	}
	if got := apply("set_stacks", 3, 0, ""); got.Result.CurrentStacks != 3 {
		t.Fatalf("set=%#v", got)
	}
	if got := apply("set_duration", 10, 0, ""); got.Result.DueTick != 10 {
		t.Fatalf("duration=%#v", got)
	}
	if got := apply("add_duration", 5, 0, ""); got.Result.DueTick != 15 {
		t.Fatalf("add duration=%#v", got)
	}
	if got := apply("mul_duration_bp", 20000, 0, ""); got.Result.DueTick != 30 {
		t.Fatalf("mul duration=%#v", got)
	}
	if got := apply("refresh", 0, 0, ""); got.Result.DueTick != 30 {
		t.Fatalf("refresh=%#v", got)
	}
	if got := apply("copy_to", 0, 99, "new_owner"); got.FailureReason != ExpectedFailureInvalidTarget {
		t.Fatalf("copy missing target=%#v", got)
	}
	copyResult := apply("copy_to", 0, 11, "new_owner")
	if !copyResult.Succeeded || copyResult.Result.Created.ID.OpaqueID() == 0 {
		t.Fatalf("copy=%#v", copyResult)
	}
	transferResult := apply("transfer_to", 0, 11, "new_source")
	if !transferResult.Succeeded || !transferResult.Result.Removed || transferResult.Result.Created.ID.OpaqueID() == 0 {
		t.Fatalf("transfer=%#v", transferResult)
	}
	if got := apply("add_stacks", 1, 0, ""); got.FailureReason != ExpectedFailureReferenceExpired {
		t.Fatalf("expired=%#v", got)
	}
	remove, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: ModifyStatusInstanceCommand{Owner: 1, Status: transferResult.Result.Created, Operation: "remove"}})
	if err != nil || !remove.Payload.(StatusEffectResult).Succeeded {
		t.Fatalf("remove=%#v err=%v", remove, err)
	}
}

func TestStatusDurationClampAppliesAtCreationAndCopy(t *testing.T) {
	host := statusInstanceHost()
	ref := applyStatusForTest(t, host, 10, 1, 1, "skill.a", 1, 600)
	if got := host.statuses[0].dueTick; got != 60 {
		t.Fatalf("initial due tick=%d want=60", got)
	}
	result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: ModifyStatusInstanceCommand{Owner: 1, SourceSkillID: "skill.a", Status: ref, Operation: "copy_to", Target: 11, OwnershipPolicy: "new_owner"}})
	if err != nil {
		t.Fatal(err)
	}
	created := result.Payload.(StatusEffectResult).Result.Created
	if created.ID.OpaqueID() == 0 || host.statuses[len(host.statuses)-1].dueTick != 60 {
		t.Fatalf("copy=%#v statuses=%#v", result, host.statuses)
	}
}

func TestStatusBatchSnapshotAndConsumerCannotSuspend(t *testing.T) {
	modify := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_status_instance","status":"$local.status","operation":"remove"}},{"flow":"effect","effect":{"type":"modify_status_instance","status":"$local.status","operation":"add_stacks","value":1}}]}`
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$input.target","kind":"status_instance","shape":{"type":"status_set"},"filters":[{"type":"status_category","category":"control"}],"order":{"by":"status_instance_id","direction":"asc"},"limit":4},"consume":{"mode":"each","as":"status","do":` + modify + `},"on_empty":{"flow":"finish"}},{"flow":"finish"}]}`
	program := compileStatusSkill(t, "batch", flow)
	host := runtimeTestHost(DefaultCompileEnvironment())
	applyStatusForTest(t, host, 2, 1, 1, program.id, 1, 20)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if len(host.statuses) != 0 {
		t.Fatalf("statuses=%#v", host.statuses)
	}
	failures := 0
	for _, event := range runtime.RuntimeEvents() {
		if event.Kind == "effect_expected_failure" && event.Result != nil && event.Result.FailureReason == ExpectedFailureReferenceExpired {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("failures=%d events=%#v", failures, runtime.RuntimeEvents())
	}

	waitFlow := `{"flow":"select","select":{"from":"$input.target","kind":"status_instance","shape":{"type":"status_set"},"filters":[],"order":{"by":"status_instance_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"status","then":{"flow":"wait","ticks":1,"then":{"flow":"finish"}}},"on_empty":{"flow":"finish"}}`
	json := statusSkillJSON("suspend", waitFlow)
	_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	if !diagnosticsHaveErrors(diagnostics) {
		t.Fatal("expected status consumer suspend diagnostic")
	}
}

func TestShieldStatusIsQueryableAndEmitsSingleAbsorbBreak(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"shield","target":"$input.target","amount":10,"duration_ticks":5}},{"flow":"finish"}]}`
	program := compileStatusSkill(t, "shield", flow)
	host := runtimeTestHost(DefaultCompileEnvironment())
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	selected, err := host.Select(SelectRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, ElementKind: "status_instance", Shape: StatusSetSelectShape{Target: 2}, Filters: []SelectFilter{StatusTextSelectFilter{Kind: "status_category", Value: "shield"}}, Order: SelectOrder{By: SelectOrderStatusInstanceID, Direction: SelectAscending}, Limit: 4})
	if err != nil || len(selected.Selection.StatusInstances()) != 1 {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	_, err = host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 15, DamageType: 3, Element: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if countRuntimeEventKind(host.Events(0), "shield_absorbed") != 1 || countRuntimeEventKind(host.Events(0), "shield_broken") != 1 {
		t.Fatalf("events=%#v", host.Events(0))
	}
	t.Run("custom hook wire is rejected", func(t *testing.T) {
		invalid := statusSkillJSON("custom-hook", `{"flow":"effect","effect":{"type":"shield","target":"$input.target","amount":10,"duration_ticks":5,"combat_hook":"custom"}}`)
		if _, err := Parse([]byte(invalid)); err == nil {
			t.Fatal("expected strict wire rejection")
		}
	})
	t.Run("catalog hook is closed", func(t *testing.T) {
		environment := DefaultCompileEnvironment()
		environment.Gameplay.Statuses.Entries = append([]StatusCatalogEntry(nil), environment.Gameplay.Statuses.Entries...)
		environment.Gameplay.Statuses.Entries[0].CombatHooks = []string{"custom"}
		environment.Digest = authorityDigest(environment)
		_, diagnostics := Compile(mustParseJSON(t, statusSkillJSON("catalog-hook", `{"flow":"finish"}`)), environment)
		if !diagnosticsHaveErrors(diagnostics) {
			t.Fatal("expected catalog hook diagnostic")
		}
	})
}

func TestShieldDurationClampAndDeathPreventionCombatHook(t *testing.T) {
	catalog := defaultGameplayCatalog()
	catalog.Statuses.Entries[1].MaximumDurationTicks = 7
	catalog.Statuses.Entries = append(catalog.Statuses.Entries, StatusCatalogEntry{Handle: 3, Key: "death_prevention", Category: "buff", RefreshPolicy: "replace", MaxStacks: 1, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "owner", RemovalPolicy: "consume", Polarity: "positive", CombatHooks: []string{"death_prevention"}, MaximumDurationTicks: 30})
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 20, MaxHealth: 20})
	shield, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: ShieldCommand{Source: 1, Target: 2, Amount: 1, DurationTicks: 100}})
	if err != nil || !shield.Payload.(ShieldEffectResult).Succeeded {
		t.Fatalf("shield=%#v err=%v", shield, err)
	}
	if got := host.statuses[len(host.statuses)-1].dueTick; got != 7 {
		t.Fatalf("shield due tick=%d want=7", got)
	}
	applyStatusForTest(t, host, 2, 3, 2, "skill.guard", 1, 30)
	damage, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 100, DamageType: 3, Element: 1}})
	if err != nil {
		t.Fatal(err)
	}
	result := damage.Payload.(DamageEffectResult).Result
	if result.Killed || result.HealthDamage != 19 || len(result.CombatHooks) != 1 || result.CombatHooks[0] != "death_prevention" {
		t.Fatalf("damage=%#v", result)
	}
	if target := host.entities[2]; !target.Alive || target.Health != 1 {
		t.Fatalf("target=%#v", target)
	}
	if countRuntimeEventKind(host.Events(0), "combat_hook_death_prevention") != 1 {
		t.Fatalf("events=%#v", host.Events(0))
	}
	if host.statusStacksLocked(2, 3, 0) != 0 {
		t.Fatalf("death prevention status was not consumed: %#v", host.statuses)
	}
}

func TestCombatHooksRespectSpellTagAndConsumePolicy(t *testing.T) {
	catalog := defaultGameplayCatalog()
	catalog.Statuses.Entries = append(catalog.Statuses.Entries,
		StatusCatalogEntry{Handle: 3, Key: "spell_shield", Category: "buff", RefreshPolicy: "replace", MaxStacks: 1, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "owner", RemovalPolicy: "consume", Polarity: "positive", CombatHooks: []string{"spell_shield"}, MaximumDurationTicks: 30},
		StatusCatalogEntry{Handle: 4, Key: "critical_override", Category: "buff", RefreshPolicy: "replace", MaxStacks: 1, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "owner", RemovalPolicy: "consume", Polarity: "positive", CombatHooks: []string{"critical_override"}, MaximumDurationTicks: 30},
	)
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
	applyStatusForTest(t, host, 2, 3, 2, "skill.guard", 1, 30)

	physical, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 1, Element: 1}})
	if err != nil || physical.Payload.(DamageEffectResult).Result.Immune || host.statusStacksLocked(2, 3, 0) != 1 {
		t.Fatalf("physical=%#v statuses=%#v err=%v", physical, host.statuses, err)
	}
	spell, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 2, Element: 1, Tags: []GameplayTagHandle{1}}})
	if err != nil || !spell.Payload.(DamageEffectResult).Result.Immune || host.statusStacksLocked(2, 3, 0) != 0 {
		t.Fatalf("spell=%#v statuses=%#v err=%v", spell, host.statuses, err)
	}

	applyStatusForTest(t, host, 1, 4, 1, "skill.crit", 1, 30)
	first, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 1, Element: 1, CanCritical: true}})
	if err != nil || !first.Payload.(DamageEffectResult).Result.Critical || host.statusStacksLocked(1, 4, 0) != 0 {
		t.Fatalf("first critical=%#v statuses=%#v err=%v", first, host.statuses, err)
	}
	second, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 1, Element: 1, CanCritical: true}})
	if err != nil || second.Payload.(DamageEffectResult).Result.Critical {
		t.Fatalf("second critical=%#v err=%v", second, err)
	}
}

func TestStatusTagFilterRequiresTargetQueryableTag(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Tags.Entries = append([]GameplayTagCatalogEntry(nil), environment.Gameplay.Tags.Entries...)
	environment.Gameplay.Tags.Entries[0].Classes = GameplayTagRuntimeOnly
	environment.Digest = authorityDigest(environment)
	flow := `{"flow":"select","select":{"from":"$input.target","kind":"status_instance","shape":{"type":"status_set"},"filters":[{"type":"status_tag","tag":"spell"}],"order":{"by":"status_instance_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"status","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`
	_, diagnostics := Compile(mustParseJSON(t, statusSkillJSON("runtime-only-tag", flow)), environment)
	if !diagnosticsHaveErrors(diagnostics) {
		t.Fatal("expected target-queryable status tag diagnostic")
	}
}

func statusInstanceHost() *MemoryHost {
	catalog := defaultGameplayCatalog()
	catalog.Statuses.Entries[0].MaxStacks = 5
	catalog.Statuses.Entries[0].GameplayTags = []GameplayTagHandle{1}
	catalog.Statuses.Entries[0].MaximumDurationTicks = 60
	catalog.Statuses.Entries = append(catalog.Statuses.Entries, StatusCatalogEntry{Handle: 3, Key: "haste", Category: "buff", RefreshPolicy: "stack", MaxStacks: 5, DispelCategory: "buff", TenacityPolicy: "none", SourceOwnership: "source", RemovalPolicy: "expire", Polarity: "positive", DispelPriority: 30, PeriodicPolicy: "none", Transferable: true, MaximumDurationTicks: 60})
	catalog.Statuses.Entries[2].Transferable = false
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	for _, entity := range []EntityID{1, 2, 10, 11} {
		host.UpsertEntity(MemoryEntity{ID: entity, Alive: true, Health: 100, MaxHealth: 100})
	}
	return host
}

func applyStatusForTest(t *testing.T, host *MemoryHost, target EntityID, status StatusHandle, source EntityID, skill string, stacks int, duration Tick) StatusInstanceRef {
	t.Helper()
	result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: StatusCommand{SourceOwner: source, SourceSkill: skill, SourceCast: 1, Target: target, Status: status, DurationTicks: duration, Stacks: stacks}})
	if err != nil || !result.Payload.(StatusEffectResult).Succeeded {
		t.Fatalf("apply=%#v err=%v", result, err)
	}
	instance := host.statuses[len(host.statuses)-1]
	return StatusInstanceRef{ID: StatusInstanceID{opaque: instance.sequence}, Target: instance.target}
}

func compileStatusSkill(t *testing.T, id, flow string) *Program {
	t.Helper()
	program, diagnostics := Compile(mustParseJSON(t, statusSkillJSON(id, flow)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

func statusSkillJSON(id, flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.status.` + id + `","name":"Status","description":"Status instances.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}
