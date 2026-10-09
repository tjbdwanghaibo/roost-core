package skill

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNumericPropertyCatalogIsClosed(t *testing.T) {
	valid := []struct {
		property string
		spawn    string
	}{
		{"speed", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`},
		{"radius", `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}}`},
		{"arc_height", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$caster.position","height":10,"duration_ticks":10},"completion":{"type":"end"}}`},
		{"turn_rate_mdeg_per_tick", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`},
		{"angular_speed_mdeg_per_tick", `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}}`},
		{"offset_amplitude", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":2,"period_ticks":2}],"completion":{"type":"end"}}`},
		{"offset_radius", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"circular","radius":2,"angular_speed":1000}],"completion":{"type":"end"}}`},
		{"return_speed_bp", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"boomerang","max_return_ticks":10}}`},
		{"collision_force", `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"collision":{"layers":["terrain"],"response":"stop"},"completion":{"type":"end"}}`},
	}
	for index, test := range valid {
		t.Run(test.property, func(t *testing.T) {
			spawn := test.spawn + fmt.Sprintf(`,"numeric_tracks":[{"property":%q,"operation":"set","value":1,"over_ticks":0}]`, test.property)
			program, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(spawn, ""))
			requireNoErrors(t, diagnostics)
			tracks := reflect.ValueOf(program.spawnTemplates[0]).FieldByName("numericTracks")
			if !tracks.IsValid() || tracks.Len() != 1 {
				t.Fatalf("lowered numeric tracks = %#v, want one immutable typed track", tracks)
			}
			track := tracks.Index(0)
			property := track.FieldByName("property")
			if !property.IsValid() || property.Kind() != reflect.Uint16 || property.Uint() != uint64(index+1) {
				t.Fatalf("lowered property handle = %#v, want %d", property, index+1)
			}
		})
	}
	t.Run("speed binds parabola trajectory", func(t *testing.T) {
		spawn := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$caster.position","height":10,"duration_ticks":10},"completion":{"type":"end"}},"numeric_tracks":[{"property":"speed","operation":"set","value":1,"over_ticks":0}]`
		_, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(spawn, ""))
		requireNoErrors(t, diagnostics)
	})

	for _, property := range []string{"duration", "interval", "target", "collision", "trajectory", "completion", "carry"} {
		t.Run("rejects structural "+property, func(t *testing.T) {
			spawn := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}` + fmt.Sprintf(`,"numeric_tracks":[{"property":%q,"operation":"set","value":1,"over_ticks":0}]`, property)
			_, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(spawn, ""))
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("structural property %q compiled", property)
			}
		})
	}

	for _, test := range []struct {
		name  string
		spawn string
	}{
		{
			name:  "slot is absent",
			spawn: `"kind":"area","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"completion":{"type":"end"}},"numeric_tracks":[{"property":"speed","operation":"set","value":1,"over_ticks":0}]`,
		},
		{
			name:  "slot is ambiguous",
			spawn: `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":2,"period_ticks":1}],"completion":{"type":"end"}},"numeric_tracks":[{"property":"offset_amplitude","operation":"set","value":1,"over_ticks":0}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(test.spawn, ""))
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("invalid property binding compiled")
			}
		})
	}
}

func TestModifySpawnRejectsInvalidOwnershipOrValue(t *testing.T) {
	validEffect := `{"type":"modify_spawn","spawn":"$spawn","property":"speed","operation":"mul_bp","value":8500,"over_ticks":6}`
	program, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(numericLinearSpawn(), validEffect))
	requireNoErrors(t, diagnostics)
	found := false
	for _, operation := range program.operations {
		if operationKind(operation) == "modify_spawn" {
			found = true
			if reflect.TypeOf(operation).Name() != "modifySpawnOperation" {
				t.Fatalf("modify_spawn lowered as %T, want a typed operation", operation)
			}
		}
	}
	if !found {
		t.Fatal("modify_spawn operation was not lowered")
	}

	invalid := []struct {
		name   string
		spawn  string
		effect string
		code   DiagnosticCode
	}{
		{"non-int initial value", numericLinearSpawnWithTracks(`{"property":"speed","operation":"set","value":"fast","over_ticks":0}`), "", DiagnosticTypeMismatch},
		{"duplicate initial property", numericLinearSpawnWithTracks(`{"property":"speed","operation":"set","value":1,"over_ticks":0},{"property":"speed","operation":"add","value":1,"over_ticks":1}`), "", DiagnosticShapeInvalid},
		{"negative initial duration", numericLinearSpawnWithTracks(`{"property":"speed","operation":"set","value":1,"over_ticks":-1}`), "", DiagnosticShapeInvalid},
		{"unsupported initial operation", numericLinearSpawnWithTracks(`{"property":"speed","operation":"divide","value":1,"over_ticks":0}`), "", DiagnosticShapeInvalid},
		{"non-int modify value", numericLinearSpawn(), `{"type":"modify_spawn","spawn":"$spawn","property":"speed","operation":"set","value":"fast","over_ticks":0}`, DiagnosticTypeMismatch},
		{"negative modify duration", numericLinearSpawn(), `{"type":"modify_spawn","spawn":"$spawn","property":"speed","operation":"set","value":1,"over_ticks":-1}`, DiagnosticShapeInvalid},
		{"non-spawn reference", numericLinearSpawn(), `{"type":"modify_spawn","spawn":"$event.target","property":"speed","operation":"set","value":1,"over_ticks":0}`, DiagnosticTypeMismatch},
		{"structural property", numericLinearSpawn(), `{"type":"modify_spawn","spawn":"$spawn","property":"duration","operation":"set","value":1,"over_ticks":0}`, DiagnosticShapeInvalid},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(test.spawn, test.effect))
			requireDiagnostic(t, diagnostics, test.code)
		})
	}
}

func TestModifySpawnIsCallbackScopedAndDoesNotEmitWorldEffect(t *testing.T) {
	const effect = `{"type":"modify_spawn","spawn":"$spawn","property":"speed","operation":"set","value":20,"over_ticks":2}`
	newFixture := func(t *testing.T) (*Runtime, *numericMutationHost, *Program, *SpawnInstance, OperationIndex) {
		t.Helper()
		program, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(numericLinearSpawn(), effect))
		requireNoErrors(t, diagnostics)
		var modifyIndex OperationIndex
		found := false
		for index, candidate := range program.operations {
			if _, ok := candidate.(modifySpawnOperation); ok {
				modifyIndex, found = OperationIndex(index), true
				break
			}
		}
		if !found {
			t.Fatal("modify_spawn operation was not lowered")
		}
		host := &numericMutationHost{numericSnapshotHost: &numericSnapshotHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}}
		runtime := NewRuntime(host, RuntimeOptions{})
		spawn := &SpawnInstance{
			ID: 7, CastID: 11, TemplateIndex: 0, Status: SpawnRunning, StartTick: 0, EndTick: 10, Program: program,
			Owner: 1, LifecycleEntity: 2, HostState: SpawnHostState{SpawnID: 7, Active: true},
			Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}}, locals: detachedSpawnLocals(program), snapshots: make(map[int]RuntimeValue), randomInvocations: make(map[RandomSiteIndex]uint64),
		}
		if _, err := runtime.stepSpawnMotion(runtime.detachedSpawnCast(spawn, evalSpawnStep), spawn); err != nil {
			t.Fatal(err)
		}
		return runtime, host, program, spawn, modifyIndex
	}
	executeWithSpawnValue := func(runtime *Runtime, program *Program, spawn *SpawnInstance, index OperationIndex, value programValue, castID CastID) error {
		operation := program.operations[index].(modifySpawnOperation)
		operation.spawn = value
		program.operations[index] = operation
		callbackCast := runtime.detachedSpawnCast(spawn, evalSpawnCallback)
		callbackCast.id = castID
		_, err := runtime.executeOperation(callbackCast, index)
		return err
	}

	t.Run("current running callback spawn replaces its track locally", func(t *testing.T) {
		runtime, host, _, spawn, _ := newFixture(t)
		revision := host.CurrentRevision()
		if err := runtime.runOwnedSpawnCallback(spawn, "tick"); err != nil {
			t.Fatal(err)
		}
		if len(host.effects) != 0 {
			t.Fatalf("modify_spawn emitted %d world effects", len(host.effects))
		}
		if host.CurrentRevision() != revision {
			t.Fatalf("modify_spawn changed Host revision from %d to %d", revision, host.CurrentRevision())
		}
		state := lookupSpawnNumericState(spawn, 1)
		if state == nil || state.Track == nil || state.Track.Start != 10 || state.Track.Target != 20 || state.Track.OverTicks != 2 {
			t.Fatalf("replacement track = %#v, want 10 -> 20 over 2 ticks", state)
		}
		runtime.currentTick = 1
		if _, err := runtime.stepSpawnMotion(runtime.detachedSpawnCast(spawn, evalSpawnStep), spawn); err != nil {
			t.Fatal(err)
		}
		if got := host.snapshots[len(host.snapshots)-1].Speed; got != 15 {
			t.Fatalf("next Motion numeric speed = %d, want bounded replacement snapshot 15", got)
		}
		if len(host.effects) != 0 {
			t.Fatalf("modify_spawn and subsequent Motion emitted %d world effects", len(host.effects))
		}
	})

	t.Run("other spawn reference rejects", func(t *testing.T) {
		runtime, _, program, spawn, index := newFixture(t)
		spawn.locals = []RuntimeValue{SpawnRuntimeValue(spawn.ID + 1)}
		err := executeWithSpawnValue(runtime, program, spawn, index, referenceProgramValue{kind: referenceLocal, index: 0, typ: valueType{Base: valueKindSpawn}}, spawn.CastID)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("other spawn reference error = %v, want %v", err, ErrCastInputRejected)
		}
	})

	t.Run("missing spawn reference rejects", func(t *testing.T) {
		runtime, _, program, spawn, index := newFixture(t)
		spawn.locals = []RuntimeValue{MissingRuntimeValue(valueType{Base: valueKindSpawn})}
		err := executeWithSpawnValue(runtime, program, spawn, index, referenceProgramValue{kind: referenceLocal, index: 0, typ: valueType{Base: valueKindSpawn, Optional: true}}, spawn.CastID)
		if !errors.Is(err, ErrRuntimeValueMissing) {
			t.Fatalf("missing spawn reference error = %v, want %v", err, ErrRuntimeValueMissing)
		}
	})

	t.Run("different cast rejects", func(t *testing.T) {
		runtime, _, program, spawn, index := newFixture(t)
		err := executeWithSpawnValue(runtime, program, spawn, index, referenceProgramValue{kind: referenceBuiltin, builtin: "$spawn", typ: valueType{Base: valueKindSpawn}}, spawn.CastID+1)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("different cast error = %v, want %v", err, ErrCastInputRejected)
		}
	})

	t.Run("ended callback spawn rejects", func(t *testing.T) {
		runtime, _, program, spawn, index := newFixture(t)
		spawn.Status = SpawnEnded
		err := executeWithSpawnValue(runtime, program, spawn, index, referenceProgramValue{kind: referenceBuiltin, builtin: "$spawn", typ: valueType{Base: valueKindSpawn}}, spawn.CastID)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("ended spawn error = %v, want %v", err, ErrCastInputRejected)
		}
	})
}

func TestNumericCatalogAuthorityValidation(t *testing.T) {
	environment := DefaultCompileEnvironment()
	originalDigest := environment.Digest
	environment.SpawnProperties.Properties = append(environment.SpawnProperties.Properties, SpawnPropertyPolicy{
		Key: "duration", Minimum: 0, Maximum: 10, Operations: []string{"set"},
	})
	environment.Digest = authorityDigest(environment)
	if environment.Digest == originalDigest {
		t.Fatal("spawn property policy did not participate in the authority digest")
	}
	requireDiagnostic(t, validateCompileEnvironment(environment), DiagnosticCatalogMotionPolicy)

	environment = DefaultCompileEnvironment()
	environment.SpawnProperties.Revision = ""
	environment.Digest = authorityDigest(environment)
	requireDiagnostic(t, validateCompileEnvironment(environment), DiagnosticCatalogMotionPolicy)
}

func TestNumericCatalogRequiresExactCanonicalPolicyFacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*CompileEnvironment)
	}{
		{
			name: "cannot omit a canonical property",
			mutate: func(environment *CompileEnvironment) {
				environment.SpawnProperties.Properties = environment.SpawnProperties.Properties[:len(environment.SpawnProperties.Properties)-1]
			},
		},
		{
			name: "cannot remap a canonical slot",
			mutate: func(environment *CompileEnvironment) {
				environment.SpawnProperties.Properties[0].SlotBindings = []SpawnPropertySlotBinding{{Stage: "collision", Variant: "present", Field: "force"}}
			},
		},
		{
			name: "cannot change a stable handle",
			mutate: func(environment *CompileEnvironment) {
				environment.SpawnProperties.Properties[0].Handle = 99
			},
		},
		{
			name: "cannot omit a required operation",
			mutate: func(environment *CompileEnvironment) {
				environment.SpawnProperties.Properties[0].Operations = []string{"set", "add"}
			},
		},
		{
			name: "cannot omit a required spawn kind",
			mutate: func(environment *CompileEnvironment) {
				environment.SpawnProperties.Properties[0].SpawnKinds = []string{"projectile"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			test.mutate(&environment)
			environment.Digest = authorityDigest(environment)
			requireDiagnostic(t, validateCompileEnvironment(environment), DiagnosticCatalogMotionPolicy)
		})
	}
}

func TestNumericProgramLowersCanonicalPolicyIdentityAndBindings(t *testing.T) {
	program, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(numericLinearSpawn(), ""))
	requireNoErrors(t, diagnostics)
	policies := reflect.ValueOf(program).Elem().FieldByName("spawnProperties")
	if !policies.IsValid() || policies.Len() != 9 {
		t.Fatalf("lowered spawn policies = %#v, want all nine canonical policies", policies)
	}
	speed := policies.Index(0)
	key := speed.FieldByName("key")
	if !key.IsValid() || key.Kind() != reflect.Uint8 || key.Uint() == 0 {
		t.Fatalf("lowered speed key = %#v, want non-string typed identity", key)
	}
	spawnKinds := speed.FieldByName("spawnKinds")
	if !spawnKinds.IsValid() || spawnKinds.Kind() != reflect.Slice || spawnKinds.Len() != 3 || spawnKinds.Index(0).Kind() != reflect.Uint8 {
		t.Fatalf("lowered speed spawn kinds = %#v, want three typed canonical kinds", spawnKinds)
	}
	slotBindings := speed.FieldByName("slotBindings")
	if !slotBindings.IsValid() || slotBindings.Kind() != reflect.Slice || slotBindings.Len() != 3 {
		t.Fatalf("lowered speed slot bindings = %#v, want linear/path/parabola typed bindings", slotBindings)
	}
	for index := 0; index < slotBindings.Len(); index++ {
		binding := slotBindings.Index(index)
		for _, field := range []string{"stage", "variant", "field"} {
			value := binding.FieldByName(field)
			if !value.IsValid() || value.Kind() != reflect.Uint8 || value.Uint() == 0 {
				t.Fatalf("lowered speed slot binding %d %s = %#v, want a typed non-zero fact", index, field, value)
			}
		}
	}
}

func TestNumericTrackSamplingAndReplacement(t *testing.T) {
	program := &Program{spawnProperties: []spawnPropertyProgram{
		{handle: 1, key: spawnPropertySpeed, minimum: 0, maximum: 100, interpolation: spawnNumericLinearInteger, rounding: spawnNumericTruncateTowardZero},
	}}
	newSpawn := func(base int64) *SpawnInstance {
		return &SpawnInstance{Program: program, Numeric: SpawnNumericState{Initialized: true, Properties: []numericPropertyState{{Property: 1, Base: base, Current: base}}}}
	}
	requireSpeed := func(t *testing.T, spawn *SpawnInstance, tick Tick, want int64) {
		t.Helper()
		snapshot, err := advanceSpawnNumeric(spawn, tick)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Speed != want {
			t.Fatalf("tick %d speed = %d, want %d", tick, snapshot.Speed, want)
		}
	}

	t.Run("immediate set and catalog clamp", func(t *testing.T) {
		spawn := newSpawn(10)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericSet, 200, 0, 5); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, spawn, 5, 100)
		state := spawn.Numeric.Properties[0]
		if state.Base != 10 || state.Current != 100 || state.Track != nil {
			t.Fatalf("completed state = %#v, want base 10, current 100, no active track", state)
		}
	})

	t.Run("active target clamps before interpolation", func(t *testing.T) {
		spawn := newSpawn(10)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericSet, 200, 2, 0); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, spawn, 0, 10)
		requireSpeed(t, spawn, 1, 55)
		requireSpeed(t, spawn, 2, 100)
	})

	t.Run("linear add truncates toward zero and completes exactly", func(t *testing.T) {
		spawn := newSpawn(10)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericAdd, 7, 3, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{10, 12, 14, 17, 17} {
			requireSpeed(t, spawn, Tick(tick), want)
		}

		negative := newSpawn(10)
		if err := replaceSpawnNumericTrack(negative, 1, spawnNumericAdd, -20, 3, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{10, 7, 4, 0} {
			requireSpeed(t, negative, Tick(tick), want)
		}
	})

	t.Run("mul basis points uses checked integer target", func(t *testing.T) {
		spawn := newSpawn(8)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericMulBP, 15000, 2, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{8, 10, 12} {
			requireSpeed(t, spawn, Tick(tick), want)
		}
	})

	t.Run("replacement begins from already sampled current value", func(t *testing.T) {
		spawn := newSpawn(10)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericSet, 20, 4, 0); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, spawn, 2, 15)
		if err := replaceSpawnNumericTrack(spawn, 1, spawnNumericAdd, 9, 3, 2); err != nil {
			t.Fatal(err)
		}
		for _, sample := range []struct {
			tick Tick
			want int64
		}{{2, 15}, {3, 18}, {4, 21}, {5, 24}, {8, 24}} {
			requireSpeed(t, spawn, sample.tick, sample.want)
		}
	})

	t.Run("motion consumes current snapshot before dispatch", func(t *testing.T) {
		definition := numericLinearSpawnWithTracks(`{"property":"speed","operation":"add","value":9,"over_ticks":2}`)
		compiled, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(definition, ""))
		requireNoErrors(t, diagnostics)
		host := &numericSnapshotHost{MemoryHost: NewMemoryHost(compiled.AuthorityIdentity())}
		runtime := NewRuntime(host, RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		spawn := &SpawnInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: SpawnHostState{SpawnID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position.X != 10 {
			t.Fatalf("first motion position = %d, want base speed 10", spawn.Motion.Position.X)
		}
		firstStepCommands := len(host.snapshots)
		runtime.currentTick = 1
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position.X != 24 {
			t.Fatalf("second motion position = %d, want sampled speed 14", spawn.Motion.Position.X)
		}
		if len(host.snapshots) == 0 {
			t.Fatal("Host received no typed numeric snapshots")
		}
		for index, snapshot := range host.snapshots {
			want := int64(10)
			if index >= firstStepCommands {
				want = 14
			}
			if snapshot.Speed != want {
				t.Fatalf("Host snapshot %d speed = %d, want %d", index, snapshot.Speed, want)
			}
		}
	})

	t.Run("non-speed initial track is resolved in the typed snapshot", func(t *testing.T) {
		definition := `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}},"numeric_tracks":[{"property":"radius","operation":"add","value":10,"over_ticks":2}]`
		compiled, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(definition, ""))
		requireNoErrors(t, diagnostics)
		host := &numericSnapshotHost{MemoryHost: NewMemoryHost(compiled.AuthorityIdentity())}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
		runtime := NewRuntime(host, RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, caster: 1, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		spawn := &SpawnInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: SpawnHostState{SpawnID: 1, Active: true},
		}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		firstStepCommands := len(host.snapshots)
		if firstStepCommands == 0 {
			t.Fatal("Host received no typed numeric snapshots")
		}
		runtime.currentTick = 1
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		for index, snapshot := range host.snapshots {
			want := int64(10)
			if index >= firstStepCommands {
				want = 15
			}
			if snapshot.Radius != want {
				t.Fatalf("Host snapshot %d radius = %d, want %d", index, snapshot.Radius, want)
			}
		}
	})

	t.Run("untracked duplicate slots retain their static values", func(t *testing.T) {
		zigzag := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":2},{"type":"zigzag","amplitude":2,"period_ticks":2}],"completion":{"type":"end"}}`
		compiled, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(zigzag, ""))
		requireNoErrors(t, diagnostics)
		runtime := NewRuntime(NewMemoryHost(compiled.AuthorityIdentity()), RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, visibleRevision: runtime.host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		spawn := &SpawnInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: SpawnHostState{SpawnID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position != (Position{X: 10, Y: 3}) {
			t.Fatalf("zigzag position = %#v, want independent amplitudes 1 and 2", spawn.Motion.Position)
		}

		orbit := `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"offsets":[{"type":"circular","radius":2,"angular_speed":2000}],"completion":{"type":"end"}}`
		compiled, diagnostics = compileNumericSkill(t, numericSpawnSkillJSON(orbit, ""))
		requireNoErrors(t, diagnostics)
		host := NewMemoryHost(compiled.AuthorityIdentity())
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
		runtime = NewRuntime(host, RuntimeOptions{})
		cast = &castInstance{id: 1, program: compiled, caster: 1, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		spawn = &SpawnInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: SpawnHostState{SpawnID: 1, Active: true},
		}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		orbitPosition := motionPolarOffset(10, 1000, 0)
		offset := motionPolarOffset(2, 2000, 0)
		want := Position{X: saturatingInt64Add(orbitPosition.X, offset.X), Y: saturatingInt64Add(orbitPosition.Y, offset.Y)}
		if spawn.Motion.Position != want {
			t.Fatalf("orbit position = %#v, want distinct trajectory and offset angular speeds %#v", spawn.Motion.Position, want)
		}
	})
}

type numericSnapshotHost struct {
	*MemoryHost
	snapshots []SpawnNumericSnapshot
}

type numericMutationHost struct {
	*numericSnapshotHost
	effects []EffectCommand
}

func (host *numericMutationHost) Apply(command EffectCommand) (EffectResult, error) {
	host.effects = append(host.effects, command)
	return host.MemoryHost.Apply(command)
}

func (host *numericSnapshotHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	host.snapshots = append(host.snapshots, command.Numeric)
	return host.MemoryHost.StepSpawn(command, state)
}

func compileNumericSkill(t *testing.T, input string) (*Program, []Diagnostic) {
	t.Helper()
	definition, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("numeric form did not parse: %v", err)
	}
	return Compile(definition, DefaultCompileEnvironment())
}

func numericSpawnSkillJSON(spawn, callbackEffect string) string {
	callback := ""
	if callbackEffect != "" {
		callback = `,"on":{"tick":{"flow":"effect","effect":` + callbackEffect + `}}`
	}
	effect := `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{` + spawn + `}` + callback + `}`
	// enter 以 wait + finish 结束：phase 没有计时，落空的 enter 不能编译（RR-20261005-NC-151）。
	return strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[`+effect+`,{"flow":"wait","ticks":10,"then":{"flow":"finish"}}]}`, 1)
}

func numericLinearSpawn() string {
	return `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`
}

func numericLinearSpawnWithTracks(tracks string) string {
	return numericLinearSpawn() + `,"numeric_tracks":[` + tracks + `]`
}
