package skill

import (
	"strings"
	"testing"
)

func TestPersistentStateSchemaLowersStablePrivateLayout(t *testing.T) {
	json := stateSchemaSkillJSON(`{
		"stance":{"type":"enum","scope":"owner","default":"ready","values":["ready","spent"],"lifetime":{"duration_ticks":10,"maximum_duration_ticks":20,"on_write":"refresh","clear_on":["owner_death","skill_removed"]}},
		"marks":{"type":"int","scope":"owner_target","default":0,"minimum":0,"maximum":3,"lifetime":{"duration_ticks":10,"maximum_duration_ticks":20,"on_write":"extend","clear_on":["target_death"]}}
	}`)
	program, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	view := InspectStateLayouts(program).PersistentState
	if len(view) != 2 || view[0].Name != "marks" || view[0].Slot != 0 || view[1].Name != "stance" || strings.Join(view[1].EnumValues, ",") != "ready,spent" {
		t.Fatalf("layout=%#v", view)
	}
	if view[0].Scope != StateScopeOwnerTarget || view[0].Maximum != 3 || view[0].OnWrite != "extend" {
		t.Fatalf("marks=%#v", view[0])
	}
}

func TestPersistentStateRejectsSharedDeclarationDuplicateEnumAndInvalidSnapshotScope(t *testing.T) {
	if _, err := Parse([]byte(stateSchemaSkillJSON(`{"shared.combo":{"type":"int","scope":"owner","default":0,"minimum":0,"maximum":3,"lifetime":{"duration_ticks":1,"maximum_duration_ticks":1,"on_write":"refresh","clear_on":[]}}}`))); err == nil {
		t.Fatal("shared declaration accepted")
	}
	for name, schema := range map[string]string{
		"duplicate-enum": `{"mode":{"type":"enum","scope":"owner","default":"a","values":["a","a"],"lifetime":{"duration_ticks":1,"maximum_duration_ticks":1,"on_write":"refresh","clear_on":[]}}}`,
		"snapshot-scope": `{"token":{"type":"snapshot_token","scope":"match","default":null,"lifetime":{"duration_ticks":1,"maximum_duration_ticks":1,"on_write":"refresh","clear_on":[]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := Compile(mustParseJSON(t, stateSchemaSkillJSON(schema)), DefaultCompileEnvironment())
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("diagnostics=%#v", diagnostics)
			}
		})
	}
}

func TestSharedStateUnknownHandleIsRejected(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"shared.unknown","owner":"$caster","operation":"add","value":1,"duration_ticks":10,"expiry_policy":"refresh"}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(`{}`), `{"flow":"finish"}`, flow, 1)
	_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	if !diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestPersistentStateTypedReadAndMutationExecuteAcrossCasts(t *testing.T) {
	state := `{"marks":{"type":"int","scope":"owner_target","default":0,"minimum":0,"maximum":3,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":["target_death"]}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_state","state":"marks","owner":"$caster","subject":"$input.target","operation":"add","value":1,"duration_ticks":20,"expiry_policy":"refresh"}},{"flow":"if","condition":{"op":"eq","args":[{"read_state":{"state":"marks","owner":"$caster","subject":"$input.target","snapshot":"current"}},1]},"then":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}}},{"flow":"finish"}]}`
	json := strings.Replace(stateSchemaSkillJSON(state), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)
	json = strings.Replace(json, `{"flow":"finish"}`, flow, 1)
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 98 {
		t.Fatalf("first cast health=%d", host.HealthForTest(2))
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 98 {
		t.Fatalf("second cast should observe marks=2, health=%d", host.HealthForTest(2))
	}
	events := host.Events(0)
	stateEvents := 0
	for _, event := range events {
		if event.Kind == "state_changed" {
			stateEvents++
			if event.State == nil || event.State.Handle.GameplayDigest != program.identity.gameplayDigest {
				t.Fatalf("state event=%#v", event)
			}
		}
	}
	if stateEvents != 2 {
		t.Fatalf("state events=%d", stateEvents)
	}
}

func stateSchemaSkillJSON(state string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.state-schema","name":"State","description":"Persistent state.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"persistent_state":` + state + `,"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]}`
}

func TestPersistentStatePrivateIdentityAndOwnerTargetIsolation(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "r", Digest: "d"})
	handleA := StateHandle{GameplayDigest: "program-a", Slot: 1}
	handleB := StateHandle{GameplayDigest: "program-b", Slot: 1}
	bindingA := StateScopeBinding{Owner: 1, Subject: 2}
	bindingB := StateScopeBinding{Owner: 3, Subject: 2}
	for _, item := range []struct {
		handle  StateHandle
		binding StateScopeBinding
		value   int64
	}{{handleA, bindingA, 1}, {handleB, bindingA, 2}, {handleA, bindingB, 3}} {
		command := stateIntCommand(host, item.handle, item.binding, StateScopeOwnerTarget, "set", item.value, 10, "refresh")
		if _, err := host.ModifyState(command); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		handle  StateHandle
		binding StateScopeBinding
		want    int64
	}{{handleA, bindingA, 1}, {handleB, bindingA, 2}, {handleA, bindingB, 3}} {
		result, err := host.ReadState(StateReadRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, Handle: item.handle, Binding: item.binding, Default: IntRuntimeValue(0, quantityDimensionless)})
		got, _ := result.Value.Int()
		if err != nil || got != item.want {
			t.Fatalf("state %#v %#v = %d, %v", item.handle, item.binding, got, err)
		}
	}
}

func TestStateLifetimeRefreshKeepExtendAndMaximumClamp(t *testing.T) {
	tests := []struct {
		name       string
		policy     string
		secondTick Tick
		wantDue    Tick
	}{
		{"refresh", "refresh", 2, 7},
		{"keep", "keep", 2, 5},
		{"extend-clamped", "extend", 2, 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := NewMemoryHost(AuthorityIdentity{Revision: "r", Digest: "d"})
			handle := StateHandle{GameplayDigest: test.name, Slot: 1}
			binding := StateScopeBinding{Owner: 1}
			first := stateIntCommand(host, handle, binding, StateScopeOwner, "set", 1, 5, "refresh")
			first.MaximumDurationTicks = 8
			if _, err := host.ModifyState(first); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Advance(test.secondTick); err != nil {
				t.Fatal(err)
			}
			second := stateIntCommand(host, handle, binding, StateScopeOwner, "add", 1, 5, test.policy)
			second.MaximumDurationTicks = 8
			if _, err := host.ModifyState(second); err != nil {
				t.Fatal(err)
			}
			if _, err := host.Advance(test.wantDue - 1); err != nil {
				t.Fatal(err)
			}
			if result, _ := host.ReadState(StateReadRequest{Handle: handle, Binding: binding, Default: IntRuntimeValue(0, quantityDimensionless)}); !result.Present {
				t.Fatal("state expired early")
			}
			if _, err := host.Advance(test.wantDue); err != nil {
				t.Fatal(err)
			}
			if result, _ := host.ReadState(StateReadRequest{Handle: handle, Binding: binding, Default: IntRuntimeValue(0, quantityDimensionless)}); result.Present {
				t.Fatal("state did not expire")
			}
		})
	}
}

func TestStateScopeBindingAndLifecycleClear(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "r", Digest: "d"})
	invalid := stateIntCommand(host, StateHandle{GameplayDigest: "x", Slot: 1}, StateScopeBinding{Owner: 1, Subject: 2}, StateScopeOwner, "set", 1, 5, "refresh")
	if _, err := host.ModifyState(invalid); err == nil {
		t.Fatal("owner scope accepted subject binding")
	}
	command := stateIntCommand(host, StateHandle{GameplayDigest: "x", Slot: 2}, StateScopeBinding{Owner: 1, Subject: 2}, StateScopeOwnerTarget, "set", 1, 20, "refresh")
	command.ClearOn = []string{"target_death"}
	if _, err := host.ModifyState(command); err != nil {
		t.Fatal(err)
	}
	if cleared := host.ClearStateLifecycle("target_death", 0, 2); cleared != 1 {
		t.Fatalf("cleared=%d", cleared)
	}
}

func TestPersistentStateMutationIsAtomicOrderedAndEmitsStructuredEvent(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "r", Digest: "d"})
	handle := StateHandle{Shared: 1}
	binding := StateScopeBinding{Owner: 1}
	root := EventContext{EventID: 8, RootEventID: 7, ParentEventID: 6, Owner: 1}
	for _, value := range []int64{2, 3} {
		command := stateIntCommand(host, handle, binding, StateScopeOwner, "add", value, 10, "refresh")
		command.Event = root
		if _, err := host.ModifyState(command); err != nil {
			t.Fatal(err)
		}
	}
	result, err := host.ReadState(StateReadRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, Handle: handle, Binding: binding, Default: IntRuntimeValue(0, quantityDimensionless)})
	got, _ := result.Value.Int()
	if err != nil || got != 5 {
		t.Fatalf("state=%d err=%v", got, err)
	}
	events := host.Events(0)
	if len(events) != 2 || events[0].State == nil || events[1].State == nil {
		t.Fatalf("events=%#v", events)
	}
	before, _ := events[1].State.Before.Int()
	after, _ := events[1].State.After.Int()
	if before != 2 || after != 5 || events[1].State.Handle != handle || events[1].State.Binding != binding || events[1].State.Reason != "add" || events[1].Context.RootEventID != 7 || events[1].Context.ParentEventID != 6 {
		t.Fatalf("event=%#v", events[1])
	}
}

func TestStateEntityValueInvalidationClearsOnRead(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "r", Digest: "d"})
	host.UpsertEntity(MemoryEntity{ID: 9, Alive: true})
	handle := StateHandle{GameplayDigest: "entity", Slot: 1}
	binding := StateScopeBinding{Owner: 1}
	command := StateMutationCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Handle: handle, Binding: binding, Scope: StateScopeOwner, Operation: "set", Value: EntityRuntimeValue(9), Default: MissingRuntimeValue(valueType{Base: valueKindEntity}), DurationTicks: 10, MaximumDurationTicks: 10, ExpiryPolicy: "refresh"}
	if _, err := host.ModifyState(command); err != nil {
		t.Fatal(err)
	}
	host.UpsertEntity(MemoryEntity{ID: 9, Alive: false})
	result, err := host.ReadState(StateReadRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, Handle: handle, Binding: binding, Default: MissingRuntimeValue(valueType{Base: valueKindEntity})})
	if err != nil || result.Present || result.Value.Present() {
		t.Fatalf("invalid entity state=%#v err=%v", result, err)
	}
	events := host.Events(0)
	if events[len(events)-1].Kind != "state_cleared" || events[len(events)-1].State.Reason != "entity_invalid" {
		t.Fatalf("events=%#v", events)
	}
}

func stateIntCommand(host *MemoryHost, handle StateHandle, binding StateScopeBinding, scope StateScope, operation string, value int64, duration Tick, policy string) StateMutationCommand {
	return StateMutationCommand{
		Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Handle: handle, Binding: binding, Scope: scope,
		Operation: operation, Value: IntRuntimeValue(value, quantityDimensionless), Default: IntRuntimeValue(0, quantityDimensionless),
		Minimum: 0, Maximum: 100, DurationTicks: duration, MaximumDurationTicks: 20, ExpiryPolicy: policy,
	}
}
