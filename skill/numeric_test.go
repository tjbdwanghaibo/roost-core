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
		process  string
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
			process := test.process + fmt.Sprintf(`,"numeric_tracks":[{"property":%q,"operation":"set","value":1,"over_ticks":0}]`, test.property)
			program, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(process, ""))
			requireNoErrors(t, diagnostics)
			tracks := reflect.ValueOf(program.processTemplates[0]).FieldByName("numericTracks")
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
		process := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$caster.position","height":10,"duration_ticks":10},"completion":{"type":"end"}},"numeric_tracks":[{"property":"speed","operation":"set","value":1,"over_ticks":0}]`
		_, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(process, ""))
		requireNoErrors(t, diagnostics)
	})

	for _, property := range []string{"duration", "interval", "target", "collision", "trajectory", "completion", "carry"} {
		t.Run("rejects structural "+property, func(t *testing.T) {
			process := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}` + fmt.Sprintf(`,"numeric_tracks":[{"property":%q,"operation":"set","value":1,"over_ticks":0}]`, property)
			_, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(process, ""))
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("structural property %q compiled", property)
			}
		})
	}

	for _, test := range []struct {
		name    string
		process string
	}{
		{
			name:    "slot is absent",
			process: `"kind":"area","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"completion":{"type":"end"}},"numeric_tracks":[{"property":"speed","operation":"set","value":1,"over_ticks":0}]`,
		},
		{
			name:    "slot is ambiguous",
			process: `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":2,"period_ticks":1}],"completion":{"type":"end"}},"numeric_tracks":[{"property":"offset_amplitude","operation":"set","value":1,"over_ticks":0}]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(test.process, ""))
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("invalid property binding compiled")
			}
		})
	}
}

func TestModifyProcessRejectsInvalidOwnershipOrValue(t *testing.T) {
	validEffect := `{"type":"modify_process","process":"$process","property":"speed","operation":"mul_bp","value":8500,"over_ticks":6}`
	program, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(numericLinearProcess(), validEffect))
	requireNoErrors(t, diagnostics)
	found := false
	for _, operation := range program.operations {
		if operationKind(operation) == "modify_process" {
			found = true
			if reflect.TypeOf(operation).Name() != "modifyProcessOperation" {
				t.Fatalf("modify_process lowered as %T, want a typed operation", operation)
			}
		}
	}
	if !found {
		t.Fatal("modify_process operation was not lowered")
	}

	invalid := []struct {
		name    string
		process string
		effect  string
		code    DiagnosticCode
	}{
		{"non-int initial value", numericLinearProcessWithTracks(`{"property":"speed","operation":"set","value":"fast","over_ticks":0}`), "", DiagnosticTypeMismatch},
		{"duplicate initial property", numericLinearProcessWithTracks(`{"property":"speed","operation":"set","value":1,"over_ticks":0},{"property":"speed","operation":"add","value":1,"over_ticks":1}`), "", DiagnosticShapeInvalid},
		{"negative initial duration", numericLinearProcessWithTracks(`{"property":"speed","operation":"set","value":1,"over_ticks":-1}`), "", DiagnosticShapeInvalid},
		{"unsupported initial operation", numericLinearProcessWithTracks(`{"property":"speed","operation":"divide","value":1,"over_ticks":0}`), "", DiagnosticShapeInvalid},
		{"non-int modify value", numericLinearProcess(), `{"type":"modify_process","process":"$process","property":"speed","operation":"set","value":"fast","over_ticks":0}`, DiagnosticTypeMismatch},
		{"negative modify duration", numericLinearProcess(), `{"type":"modify_process","process":"$process","property":"speed","operation":"set","value":1,"over_ticks":-1}`, DiagnosticShapeInvalid},
		{"non-process reference", numericLinearProcess(), `{"type":"modify_process","process":"$event.target","property":"speed","operation":"set","value":1,"over_ticks":0}`, DiagnosticTypeMismatch},
		{"structural property", numericLinearProcess(), `{"type":"modify_process","process":"$process","property":"duration","operation":"set","value":1,"over_ticks":0}`, DiagnosticShapeInvalid},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(test.process, test.effect))
			requireDiagnostic(t, diagnostics, test.code)
		})
	}
}

func TestModifyProcessIsCallbackScopedAndDoesNotEmitWorldEffect(t *testing.T) {
	const effect = `{"type":"modify_process","process":"$process","property":"speed","operation":"set","value":20,"over_ticks":2}`
	newFixture := func(t *testing.T) (*Runtime, *numericMutationHost, *Program, *ProcessInstance, OperationIndex) {
		t.Helper()
		program, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(numericLinearProcess(), effect))
		requireNoErrors(t, diagnostics)
		var modifyIndex OperationIndex
		found := false
		for index, candidate := range program.operations {
			if _, ok := candidate.(modifyProcessOperation); ok {
				modifyIndex, found = OperationIndex(index), true
				break
			}
		}
		if !found {
			t.Fatal("modify_process operation was not lowered")
		}
		host := &numericMutationHost{numericSnapshotHost: &numericSnapshotHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}}
		runtime := NewRuntime(host, RuntimeOptions{})
		process := &ProcessInstance{
			ID: 7, CastID: 11, TemplateIndex: 0, Status: ProcessRunning, StartTick: 0, EndTick: 10, Program: program,
			Owner: 1, LifecycleEntity: 2, HostState: ProcessHostState{ProcessID: 7, Active: true},
			Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}}, locals: detachedProcessLocals(program), snapshots: make(map[int]RuntimeValue), randomInvocations: make(map[RandomSiteIndex]uint64),
		}
		if _, err := runtime.stepProcessMotion(runtime.detachedProcessCast(process), process); err != nil {
			t.Fatal(err)
		}
		return runtime, host, program, process, modifyIndex
	}
	executeWithProcessValue := func(runtime *Runtime, program *Program, process *ProcessInstance, index OperationIndex, value programValue, castID CastID) error {
		operation := program.operations[index].(modifyProcessOperation)
		operation.process = value
		program.operations[index] = operation
		callbackCast := runtime.detachedProcessCast(process)
		callbackCast.id = castID
		_, err := runtime.executeOperation(callbackCast, index)
		return err
	}

	t.Run("current running callback process replaces its track locally", func(t *testing.T) {
		runtime, host, _, process, _ := newFixture(t)
		revision := host.CurrentRevision()
		if err := runtime.runOwnedProcessCallback(process, "tick"); err != nil {
			t.Fatal(err)
		}
		if len(host.effects) != 0 {
			t.Fatalf("modify_process emitted %d world effects", len(host.effects))
		}
		if host.CurrentRevision() != revision {
			t.Fatalf("modify_process changed Host revision from %d to %d", revision, host.CurrentRevision())
		}
		state := lookupProcessNumericState(process, 1)
		if state == nil || state.Track == nil || state.Track.Start != 10 || state.Track.Target != 20 || state.Track.OverTicks != 2 {
			t.Fatalf("replacement track = %#v, want 10 -> 20 over 2 ticks", state)
		}
		runtime.currentTick = 1
		if _, err := runtime.stepProcessMotion(runtime.detachedProcessCast(process), process); err != nil {
			t.Fatal(err)
		}
		if got := host.snapshots[len(host.snapshots)-1].Speed; got != 15 {
			t.Fatalf("next Motion numeric speed = %d, want bounded replacement snapshot 15", got)
		}
		if len(host.effects) != 0 {
			t.Fatalf("modify_process and subsequent Motion emitted %d world effects", len(host.effects))
		}
	})

	t.Run("other process reference rejects", func(t *testing.T) {
		runtime, _, program, process, index := newFixture(t)
		process.locals = []RuntimeValue{ProcessRuntimeValue(process.ID + 1)}
		err := executeWithProcessValue(runtime, program, process, index, referenceProgramValue{kind: referenceLocal, index: 0, typ: valueType{Base: valueKindProcess}}, process.CastID)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("other process reference error = %v, want %v", err, ErrCastInputRejected)
		}
	})

	t.Run("missing process reference rejects", func(t *testing.T) {
		runtime, _, program, process, index := newFixture(t)
		process.locals = []RuntimeValue{MissingRuntimeValue(valueType{Base: valueKindProcess})}
		err := executeWithProcessValue(runtime, program, process, index, referenceProgramValue{kind: referenceLocal, index: 0, typ: valueType{Base: valueKindProcess, Optional: true}}, process.CastID)
		if !errors.Is(err, ErrRuntimeValueMissing) {
			t.Fatalf("missing process reference error = %v, want %v", err, ErrRuntimeValueMissing)
		}
	})

	t.Run("different cast rejects", func(t *testing.T) {
		runtime, _, program, process, index := newFixture(t)
		err := executeWithProcessValue(runtime, program, process, index, referenceProgramValue{kind: referenceBuiltin, builtin: "$process", typ: valueType{Base: valueKindProcess}}, process.CastID+1)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("different cast error = %v, want %v", err, ErrCastInputRejected)
		}
	})

	t.Run("ended callback process rejects", func(t *testing.T) {
		runtime, _, program, process, index := newFixture(t)
		process.Status = ProcessEnded
		err := executeWithProcessValue(runtime, program, process, index, referenceProgramValue{kind: referenceBuiltin, builtin: "$process", typ: valueType{Base: valueKindProcess}}, process.CastID)
		if !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("ended process error = %v, want %v", err, ErrCastInputRejected)
		}
	})
}

func TestNumericCatalogAuthorityValidation(t *testing.T) {
	environment := DefaultCompileEnvironment()
	originalDigest := environment.Digest
	environment.ProcessProperties.Properties = append(environment.ProcessProperties.Properties, ProcessPropertyPolicy{
		Key: "duration", Minimum: 0, Maximum: 10, Operations: []string{"set"},
	})
	environment.Digest = authorityDigest(environment)
	if environment.Digest == originalDigest {
		t.Fatal("process property policy did not participate in the authority digest")
	}
	requireDiagnostic(t, validateCompileEnvironment(environment), DiagnosticCatalogMotionPolicy)

	environment = DefaultCompileEnvironment()
	environment.ProcessProperties.Revision = ""
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
				environment.ProcessProperties.Properties = environment.ProcessProperties.Properties[:len(environment.ProcessProperties.Properties)-1]
			},
		},
		{
			name: "cannot remap a canonical slot",
			mutate: func(environment *CompileEnvironment) {
				environment.ProcessProperties.Properties[0].SlotBindings = []ProcessPropertySlotBinding{{Stage: "collision", Variant: "present", Field: "force"}}
			},
		},
		{
			name: "cannot change a stable handle",
			mutate: func(environment *CompileEnvironment) {
				environment.ProcessProperties.Properties[0].Handle = 99
			},
		},
		{
			name: "cannot omit a required operation",
			mutate: func(environment *CompileEnvironment) {
				environment.ProcessProperties.Properties[0].Operations = []string{"set", "add"}
			},
		},
		{
			name: "cannot omit a required process kind",
			mutate: func(environment *CompileEnvironment) {
				environment.ProcessProperties.Properties[0].ProcessKinds = []string{"projectile"}
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
	program, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(numericLinearProcess(), ""))
	requireNoErrors(t, diagnostics)
	policies := reflect.ValueOf(program).Elem().FieldByName("processProperties")
	if !policies.IsValid() || policies.Len() != 9 {
		t.Fatalf("lowered process policies = %#v, want all nine canonical policies", policies)
	}
	speed := policies.Index(0)
	key := speed.FieldByName("key")
	if !key.IsValid() || key.Kind() != reflect.Uint8 || key.Uint() == 0 {
		t.Fatalf("lowered speed key = %#v, want non-string typed identity", key)
	}
	processKinds := speed.FieldByName("processKinds")
	if !processKinds.IsValid() || processKinds.Kind() != reflect.Slice || processKinds.Len() != 3 || processKinds.Index(0).Kind() != reflect.Uint8 {
		t.Fatalf("lowered speed process kinds = %#v, want three typed canonical kinds", processKinds)
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
	program := &Program{processProperties: []processPropertyProgram{
		{handle: 1, key: processPropertySpeed, minimum: 0, maximum: 100, interpolation: processNumericLinearInteger, rounding: processNumericTruncateTowardZero},
	}}
	newProcess := func(base int64) *ProcessInstance {
		return &ProcessInstance{Program: program, Numeric: ProcessNumericState{Initialized: true, Properties: []numericPropertyState{{Property: 1, Base: base, Current: base}}}}
	}
	requireSpeed := func(t *testing.T, process *ProcessInstance, tick Tick, want int64) {
		t.Helper()
		snapshot, err := advanceProcessNumeric(process, tick)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Speed != want {
			t.Fatalf("tick %d speed = %d, want %d", tick, snapshot.Speed, want)
		}
	}

	t.Run("immediate set and catalog clamp", func(t *testing.T) {
		process := newProcess(10)
		if err := replaceProcessNumericTrack(process, 1, processNumericSet, 200, 0, 5); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, process, 5, 100)
		state := process.Numeric.Properties[0]
		if state.Base != 10 || state.Current != 100 || state.Track != nil {
			t.Fatalf("completed state = %#v, want base 10, current 100, no active track", state)
		}
	})

	t.Run("active target clamps before interpolation", func(t *testing.T) {
		process := newProcess(10)
		if err := replaceProcessNumericTrack(process, 1, processNumericSet, 200, 2, 0); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, process, 0, 10)
		requireSpeed(t, process, 1, 55)
		requireSpeed(t, process, 2, 100)
	})

	t.Run("linear add truncates toward zero and completes exactly", func(t *testing.T) {
		process := newProcess(10)
		if err := replaceProcessNumericTrack(process, 1, processNumericAdd, 7, 3, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{10, 12, 14, 17, 17} {
			requireSpeed(t, process, Tick(tick), want)
		}

		negative := newProcess(10)
		if err := replaceProcessNumericTrack(negative, 1, processNumericAdd, -20, 3, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{10, 7, 4, 0} {
			requireSpeed(t, negative, Tick(tick), want)
		}
	})

	t.Run("mul basis points uses checked integer target", func(t *testing.T) {
		process := newProcess(8)
		if err := replaceProcessNumericTrack(process, 1, processNumericMulBP, 15000, 2, 0); err != nil {
			t.Fatal(err)
		}
		for tick, want := range []int64{8, 10, 12} {
			requireSpeed(t, process, Tick(tick), want)
		}
	})

	t.Run("replacement begins from already sampled current value", func(t *testing.T) {
		process := newProcess(10)
		if err := replaceProcessNumericTrack(process, 1, processNumericSet, 20, 4, 0); err != nil {
			t.Fatal(err)
		}
		requireSpeed(t, process, 2, 15)
		if err := replaceProcessNumericTrack(process, 1, processNumericAdd, 9, 3, 2); err != nil {
			t.Fatal(err)
		}
		for _, sample := range []struct {
			tick Tick
			want int64
		}{{2, 15}, {3, 18}, {4, 21}, {5, 24}, {8, 24}} {
			requireSpeed(t, process, sample.tick, sample.want)
		}
	})

	t.Run("motion consumes current snapshot before dispatch", func(t *testing.T) {
		definition := numericLinearProcessWithTracks(`{"property":"speed","operation":"add","value":9,"over_ticks":2}`)
		compiled, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(definition, ""))
		requireNoErrors(t, diagnostics)
		host := &numericSnapshotHost{MemoryHost: NewMemoryHost(compiled.AuthorityIdentity())}
		runtime := NewRuntime(host, RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		process := &ProcessInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: ProcessHostState{ProcessID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position.X != 10 {
			t.Fatalf("first motion position = %d, want base speed 10", process.Motion.Position.X)
		}
		firstStepCommands := len(host.snapshots)
		runtime.currentTick = 1
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position.X != 24 {
			t.Fatalf("second motion position = %d, want sampled speed 14", process.Motion.Position.X)
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
		compiled, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(definition, ""))
		requireNoErrors(t, diagnostics)
		host := &numericSnapshotHost{MemoryHost: NewMemoryHost(compiled.AuthorityIdentity())}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
		runtime := NewRuntime(host, RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, caster: 1, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		process := &ProcessInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: ProcessHostState{ProcessID: 1, Active: true},
		}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		firstStepCommands := len(host.snapshots)
		if firstStepCommands == 0 {
			t.Fatal("Host received no typed numeric snapshots")
		}
		runtime.currentTick = 1
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
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
		compiled, diagnostics := compileNumericSkill(t, numericProcessSkillJSON(zigzag, ""))
		requireNoErrors(t, diagnostics)
		runtime := NewRuntime(NewMemoryHost(compiled.AuthorityIdentity()), RuntimeOptions{})
		cast := &castInstance{id: 1, program: compiled, visibleRevision: runtime.host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		process := &ProcessInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: ProcessHostState{ProcessID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position != (Position{X: 10, Y: 3}) {
			t.Fatalf("zigzag position = %#v, want independent amplitudes 1 and 2", process.Motion.Position)
		}

		orbit := `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"offsets":[{"type":"circular","radius":2,"angular_speed":2000}],"completion":{"type":"end"}}`
		compiled, diagnostics = compileNumericSkill(t, numericProcessSkillJSON(orbit, ""))
		requireNoErrors(t, diagnostics)
		host := NewMemoryHost(compiled.AuthorityIdentity())
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
		runtime = NewRuntime(host, RuntimeOptions{})
		cast = &castInstance{id: 1, program: compiled, caster: 1, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		process = &ProcessInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, StartTick: 0, EndTick: 10, Program: compiled,
			HostState: ProcessHostState{ProcessID: 1, Active: true},
		}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		orbitPosition := motionPolarOffset(10, 1000, 0)
		offset := motionPolarOffset(2, 2000, 0)
		want := Position{X: saturatingInt64Add(orbitPosition.X, offset.X), Y: saturatingInt64Add(orbitPosition.Y, offset.Y)}
		if process.Motion.Position != want {
			t.Fatalf("orbit position = %#v, want distinct trajectory and offset angular speeds %#v", process.Motion.Position, want)
		}
	})
}

type numericSnapshotHost struct {
	*MemoryHost
	snapshots []ProcessNumericSnapshot
}

type numericMutationHost struct {
	*numericSnapshotHost
	effects []EffectCommand
}

func (host *numericMutationHost) Apply(command EffectCommand) (EffectResult, error) {
	host.effects = append(host.effects, command)
	return host.MemoryHost.Apply(command)
}

func (host *numericSnapshotHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	host.snapshots = append(host.snapshots, command.Numeric)
	return host.MemoryHost.StepProcess(command, state)
}

func compileNumericSkill(t *testing.T, input string) (*Program, []Diagnostic) {
	t.Helper()
	definition, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("numeric form did not parse: %v", err)
	}
	return Compile(definition, DefaultCompileEnvironment())
}

func numericProcessSkillJSON(process, callbackEffect string) string {
	callback := ""
	if callbackEffect != "" {
		callback = `,"on":{"tick":{"flow":"effect","effect":` + callbackEffect + `}}`
	}
	effect := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{` + process + `}` + callback + `}`
	result := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, effect, 1)
	return strings.Replace(result, `"timeout_ticks":0`, `"timeout_ticks":10`, 1)
}

func numericLinearProcess() string {
	return `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`
}

func numericLinearProcessWithTracks(tracks string) string {
	return numericLinearProcess() + `,"numeric_tracks":[` + tracks + `]`
}
