package skill

import (
	"errors"
	"strings"
	"testing"
)

func TestTargetValidity(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
	host.UpsertEntity(MemoryEntity{ID: 5, Alive: true, VisibleTo: map[EntityID]bool{1: true}, GameplayTags: map[GameplayTagHandle]bool{1: true}})
	host.UpsertEntity(MemoryEntity{ID: 3, Alive: true, VisibleTo: map[EntityID]bool{1: true}, DamageImmune: true})
	host.UpsertEntity(MemoryEntity{ID: 4, Alive: true, VisibleTo: map[EntityID]bool{1: true}, Untargetable: true})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, VisibleTo: map[EntityID]bool{9: true}})

	selectEntities := func(t *testing.T, caster EntityID, filters ...SelectFilter) []EntityID {
		t.Helper()
		result, err := host.Select(SelectRequest{
			Meta:        QueryMeta{RequiredRevision: host.CurrentRevision()},
			Caster:      caster,
			ElementKind: "entity",
			Shape:       CircleSelectShape{Center: Position{}, Radius: 100},
			Filters:     filters,
			Order:       SelectOrder{By: SelectOrderEntityID, Direction: SelectAscending},
			Limit:       16,
		})
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		entities := make([]EntityID, len(result.Selection.elements))
		for index, element := range result.Selection.elements {
			entities[index] = element.entity
		}
		return entities
	}

	t.Run("visible uses caster as observer", func(t *testing.T) {
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}), []EntityID{3, 4, 5})
		assertEntityIDs(t, selectEntities(t, 9, VisibleSelectFilter{}), []EntityID{2})
	})

	t.Run("targetable excludes untargetable but not combat immune", func(t *testing.T) {
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}, TargetableSelectFilter{}), []EntityID{3, 5})
	})

	t.Run("gameplay tags use typed handles", func(t *testing.T) {
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}, GameplayTagSelectFilter{Tag: 1, Has: true}), []EntityID{5})
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}, GameplayTagSelectFilter{Tag: 1, Has: false}), []EntityID{3, 4})
	})

	t.Run("line of sight uses declared collision layers", func(t *testing.T) {
		host.UpsertEntity(MemoryEntity{ID: 6, Alive: true, VisibleTo: map[EntityID]bool{1: true}, BlockedLineOfSightLayers: map[CollisionLayerHandle]bool{1: true}})
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}, LineOfSightSelectFilter{Layers: []CollisionLayerHandle{1}}), []EntityID{3, 4, 5})
		assertEntityIDs(t, selectEntities(t, 1, VisibleSelectFilter{}, LineOfSightSelectFilter{Layers: []CollisionLayerHandle{2}}), []EntityID{3, 4, 5, 6})
	})

	t.Run("compiler lowers only typed target query handles", func(t *testing.T) {
		flow := `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"visible"},{"type":"targetable"},{"type":"line_of_sight","collision":["terrain"]},{"type":"has_gameplay_tag","tag":"spell"}],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`
		input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1)
		program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		filters := program.selectors[0].filters
		if len(filters) != 4 || len(filters[2].collision) != 1 || filters[2].collision[0] != 1 || filters[3].tag != 1 {
			t.Fatalf("lowered target filters = %#v, want collision and gameplay tag handles", filters)
		}
	})

	t.Run("compiler rejects tags not marked target queryable", func(t *testing.T) {
		flow := `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"has_gameplay_tag","tag":"critical"}],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`
		input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1)
		_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireDiagnostic(t, diagnostics, DiagnosticGameplayTagPermission)
	})
}

func TestAreaMembership(t *testing.T) {
	process := &ProcessInstance{ID: 1, Status: ProcessRunning}
	type expectedSignal struct {
		kind                        ProcessSignalKind
		target                      EntityID
		membershipTicks, enterCount int64
	}
	frames := []struct {
		members []EntityID
		want    []expectedSignal
	}{
		{members: nil},
		{members: []EntityID{3, 2, 3}, want: []expectedSignal{{ProcessSignalEnter, 2, 1, 1}, {ProcessSignalEnter, 3, 1, 1}, {ProcessSignalTick, 2, 1, 1}, {ProcessSignalTick, 3, 1, 1}}},
		{members: []EntityID{4, 3}, want: []expectedSignal{{ProcessSignalLeave, 2, 1, 1}, {ProcessSignalEnter, 4, 1, 1}, {ProcessSignalTick, 3, 2, 1}, {ProcessSignalTick, 4, 1, 1}}},
		{members: []EntityID{4, 3}, want: []expectedSignal{{ProcessSignalTick, 3, 3, 1}, {ProcessSignalTick, 4, 2, 1}}},
		{members: nil, want: []expectedSignal{{ProcessSignalLeave, 3, 3, 1}, {ProcessSignalLeave, 4, 2, 1}}},
	}
	for frame, test := range frames {
		got := advanceAreaMembership(process, test.members)
		if len(got) != len(test.want) {
			t.Fatalf("frame %d signals = %#v, want %#v", frame, got, test.want)
		}
		for index, want := range test.want {
			if got[index].Kind != want.kind || got[index].Target != want.target || got[index].MembershipTicks != want.membershipTicks || got[index].EnterCount != want.enterCount {
				t.Fatalf("frame %d signal %d = %#v, want %#v", frame, index, got[index], want)
			}
		}
	}

	for _, emit := range []bool{false, true} {
		t.Run("stop cleanup emit="+map[bool]string{false: "false", true: "true"}[emit], func(t *testing.T) {
			process := &ProcessInstance{ID: 2, Status: ProcessRunning}
			advanceAreaMembership(process, []EntityID{4, 2})
			leaves := stopAreaMembership(process, emit)
			if len(process.AreaMembers) != 0 {
				t.Fatalf("area members leaked after stop: %#v", process.AreaMembers)
			}
			if !emit && len(leaves) != 0 {
				t.Fatalf("silent stop emitted leaves: %#v", leaves)
			}
			if emit {
				if len(leaves) != 2 || leaves[0].Target != 2 || leaves[1].Target != 4 || leaves[0].Kind != ProcessSignalLeave || leaves[1].Kind != ProcessSignalLeave {
					t.Fatalf("final leaves = %#v, want sorted [2,4]", leaves)
				}
			}
		})
	}

	t.Run("runtime queries and emits member callbacks", func(t *testing.T) {
		program, diagnostics := Compile(mustParseJSON(t, areaProcessSkillJSON(3)), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		host := &areaSnapshotHost{MemoryHost: runtimeTestHost(DefaultCompileEnvironment()), snapshots: [][]EntityID{{3, 2}, {4, 3}}}
		for _, id := range []EntityID{2, 3, 4} {
			host.UpsertEntity(MemoryEntity{ID: id, Alive: true, Health: 100, MaxHealth: 100})
		}
		runtime := NewRuntime(host, RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		assertAreaCallbackEvents(t, runtime.RuntimeEvents(), []ProcessSignal{
			{Kind: ProcessSignalEnter, Target: 2, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalEnter, Target: 3, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 2, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 3, MembershipTicks: 1, EnterCount: 1},
		})
		if err := runtime.Advance(2); err != nil {
			t.Fatal(err)
		}
		assertAreaCallbackEvents(t, runtime.RuntimeEvents(), []ProcessSignal{
			{Kind: ProcessSignalEnter, Target: 2, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalEnter, Target: 3, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 2, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 3, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalLeave, Target: 2, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalEnter, Target: 4, MembershipTicks: 1, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 3, MembershipTicks: 2, EnterCount: 1},
			{Kind: ProcessSignalTick, Target: 4, MembershipTicks: 1, EnterCount: 1},
		})
		if err := runtime.Advance(4); err != nil {
			t.Fatal(err)
		}
		callbacks := areaCallbackSignals(runtime.RuntimeEvents())
		last := callbacks[len(callbacks)-2:]
		if last[0].Kind != ProcessSignalLeave || last[0].Target != 3 || last[1].Kind != ProcessSignalLeave || last[1].Target != 4 {
			t.Fatalf("final leave callbacks = %#v, want sorted members 3,4", last)
		}
		if len(runtime.OwnedProcesses(1)) != 0 {
			t.Fatalf("ended area retained process state: %#v", runtime.OwnedProcesses(1))
		}
	})

	t.Run("host error clears membership state", func(t *testing.T) {
		program, diagnostics := Compile(mustParseJSON(t, areaProcessSkillJSON(2)), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		host := &areaSnapshotHost{MemoryHost: runtimeTestHost(DefaultCompileEnvironment()), snapshots: [][]EntityID{{2}}, failAt: 1}
		host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
		runtime := NewRuntime(host, RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Advance(2); err == nil {
			t.Fatal("expected area select host failure")
		}
		for _, process := range runtime.processes {
			if len(process.AreaMembers) != 0 {
				t.Fatalf("host error leaked area members: %#v", process.AreaMembers)
			}
		}
	})
}

func TestAreaMembershipRotatingMembersRemainBounded(t *testing.T) {
	process := &ProcessInstance{ID: 9, Status: ProcessRunning}
	for member := EntityID(1); member <= 64; member++ {
		signals := advanceAreaMembership(process, []EntityID{member})
		if len(process.AreaMembers) != 1 {
			t.Fatalf("frame %d retained %d member states: %#v", member, len(process.AreaMembers), process.AreaMembers)
		}
		if member == 1 {
			continue
		}
		if len(signals) != 3 || signals[0].Kind != ProcessSignalLeave || signals[0].Target != member-1 || signals[0].MembershipTicks != 1 {
			t.Fatalf("frame %d signals = %#v, want leave for %d before enter/tick", member, signals, member-1)
		}
	}
	last := advanceAreaMembership(process, nil)
	if len(process.AreaMembers) != 0 {
		t.Fatalf("empty frame retained member states: %#v", process.AreaMembers)
	}
	if len(last) != 1 || last[0].Kind != ProcessSignalLeave || last[0].Target != 64 || last[0].MembershipTicks != 1 {
		t.Fatalf("final signals = %#v, want leave for 64", last)
	}
}

func TestAreaCallbackFinishStopsRemainingSignals(t *testing.T) {
	finish := `{"flow":"finish","reason":"area_complete"}`
	command := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$event.target","command":"hold_position"}}`
	flow := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"enter":` + finish + `,"tick":` + command + `}}`
	input := strings.Replace(strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1), `"timeout_ticks":0`, `"timeout_ticks":10`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)

	host := &areaSnapshotHost{MemoryHost: runtimeTestHost(DefaultCompileEnvironment()), snapshots: [][]EntityID{{2, 3}}}
	for _, id := range []EntityID{2, 3} {
		host.UpsertEntity(MemoryEntity{ID: id, Alive: true, Health: 100, MaxHealth: 100})
	}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, ok := runtime.InspectCast(castID); !ok || snapshot.Status != CastFinished {
		t.Fatalf("cast = %#v, found=%v, want finished", snapshot, ok)
	}
	callbacks := areaCallbackSignals(runtime.RuntimeEvents())
	if len(callbacks) != 1 || callbacks[0].Kind != ProcessSignalEnter || callbacks[0].Target != 2 {
		t.Fatalf("callbacks = %#v, want only first enter", callbacks)
	}
	if host.stops != 1 {
		t.Fatalf("StopProcess calls = %d, want unified stop exactly once", host.stops)
	}
	for _, process := range runtime.processes {
		if process.CastID == castID && (process.Status != ProcessCancelled || len(process.AreaMembers) != 0) {
			t.Fatalf("area process after finish = status %q members %#v", process.Status, process.AreaMembers)
		}
	}
	if len(runtime.OwnedProcesses(1)) != 0 {
		t.Fatalf("finished callback handed off area process: %#v", runtime.OwnedProcesses(1))
	}
}

func TestAreaFinalLeaveFinishSuppressesTerminalCallback(t *testing.T) {
	finish := `{"flow":"finish","reason":"area_complete"}`
	command := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$event.target","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":1,"emit_leave_on_stop":true,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":1}},"on":{"leave":` + finish + `,"cancel":` + command + `}},{"flow":"wait","ticks":5,"then":{"flow":"finish"}}]}`
	input := strings.Replace(strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1), `"timeout_ticks":0`, `"timeout_ticks":10`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)

	host := &areaSnapshotHost{MemoryHost: runtimeTestHost(DefaultCompileEnvironment()), snapshots: [][]EntityID{{2}}}
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Cancel(castID); err != nil {
		t.Fatal(err)
	}

	callbacks := areaCallbackSignals(runtime.RuntimeEvents())
	if len(callbacks) != 1 || callbacks[0].Kind != ProcessSignalLeave || callbacks[0].Target != 2 {
		t.Fatalf("callbacks = %#v, want only final leave for 2", callbacks)
	}
	if cast := runtime.casts[castID]; cast == nil || !cast.areaCallbackFinish || cast.status != CastFinished {
		t.Fatalf("cast = %#v, want final leave to finish it", cast)
	}
}

type areaSnapshotHost struct {
	*MemoryHost
	snapshots [][]EntityID
	queries   int
	failAt    int
	stops     int
}

func (host *areaSnapshotHost) Select(request SelectRequest) (SelectResult, error) {
	if host.failAt > 0 && host.queries == host.failAt {
		host.queries++
		return SelectResult{}, errors.New("area select failed")
	}
	if host.queries >= len(host.snapshots) {
		return host.MemoryHost.Select(request)
	}
	members := host.snapshots[host.queries]
	host.queries++
	meta := QueryResultMeta{Revision: host.CurrentRevision()}
	selection := Selection{elementType: selectionEntity, meta: meta, elements: make([]selectionElement, len(members))}
	for index, member := range members {
		selection.elements[index].entity = member
	}
	return SelectResult{Meta: meta, Selection: selection}, nil
}

func (host *areaSnapshotHost) StopProcess(command ProcessStopCommand, state ProcessHostState) (CommitReceipt, error) {
	host.stops++
	return host.MemoryHost.StopProcess(command, state)
}

func assertAreaCallbackEvents(t *testing.T, events []RuntimeEvent, want []ProcessSignal) {
	t.Helper()
	got := areaCallbackSignals(events)
	if len(got) != len(want) {
		t.Fatalf("area callback events = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("area callback event %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func areaCallbackSignals(events []RuntimeEvent) []ProcessSignal {
	got := make([]ProcessSignal, 0)
	for _, event := range events {
		const prefix = "owned_process_callback_"
		if !strings.HasPrefix(event.Kind, prefix) {
			continue
		}
		got = append(got, ProcessSignal{Kind: ProcessSignalKind(strings.TrimPrefix(event.Kind, prefix)), Target: event.Context.Target, MembershipTicks: event.Context.MembershipTicks, EnterCount: event.Context.EnterCount})
	}
	return got
}

func TestAreaBudget(t *testing.T) {
	input := areaProcessSkillJSON(3)
	environment := DefaultCompileEnvironment()
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), environment)
	requireNoErrors(t, diagnostics)
	if artifacts.limits.AreaMembers != 3 || artifacts.limits.Mutations != 19 || artifacts.limits.LifetimeTicks != 4 || artifacts.limits.Schedules != 2 {
		t.Fatalf("area limits = members %d mutations %d lifetime %d schedules %d, want 3, 19, 4, 2", artifacts.limits.AreaMembers, artifacts.limits.Mutations, artifacts.limits.LifetimeTicks, artifacts.limits.Schedules)
	}

	environment.Limits.MaxAreaMembers = 2
	environment.Digest = authorityDigest(environment)
	_, diagnostics = Compile(mustParseJSON(t, input), environment)
	requireDiagnostic(t, diagnostics, DiagnosticBudgetExceeded)

	environment = DefaultCompileEnvironment()
	environment.Limits.MaxMutations = 18
	environment.Digest = authorityDigest(environment)
	_, diagnostics = Compile(mustParseJSON(t, input), environment)
	requireDiagnostic(t, diagnostics, DiagnosticBudgetExceeded)

	t.Run("spawn count multiplies area work", func(t *testing.T) {
		input := strings.Replace(areaProcessSkillJSON(3), `"count":1`, `"count":2`, 1)
		artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		if artifacts.limits.Processes != 2 || artifacts.limits.AreaMembers != 6 || artifacts.limits.Schedules != 4 || artifacts.limits.Mutations != 37 {
			t.Fatalf("counted area limits = processes %d members %d schedules %d mutations %d, want 2, 6, 4, 37", artifacts.limits.Processes, artifacts.limits.AreaMembers, artifacts.limits.Schedules, artifacts.limits.Mutations)
		}
	})

	t.Run("spawn count multiplies moving area numeric tracks", func(t *testing.T) {
		input := strings.Replace(areaProcessSkillJSON(3), `"count":1`, `"count":2`, 1)
		motionAndTrack := `"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}},"numeric_tracks":[{"property":"speed","operation":"set","value":12,"over_ticks":0}],`
		input = strings.Replace(input, `"process":{"kind":"area","duration_ticks":4,`, `"process":{"kind":"area","duration_ticks":4,`+motionAndTrack, 1)
		artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		if artifacts.limits.Mutations != 39 {
			t.Fatalf("moving count-two area mutations = %d, want 39", artifacts.limits.Mutations)
		}
	})
}

func areaProcessSkillJSON(maxMembers int) string {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$event.target","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":2,"emit_leave_on_stop":true,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":` + intToDecimal(maxMembers) + `}},"on":{"enter":` + callback + `,"leave":` + callback + `,"tick":` + callback + `}},{"flow":"finish"}]}`
	return strings.Replace(strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1), `"timeout_ticks":0`, `"timeout_ticks":10`, 1)
}

func assertEntityIDs(t *testing.T, got, want []EntityID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entities = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("entities = %v, want %v", got, want)
		}
	}
}
