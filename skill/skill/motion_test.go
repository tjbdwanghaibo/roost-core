package skill

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMotionPipelineHasFixedStageOrder(t *testing.T) {
	motions := []string{
		`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":2,"period_ticks":2}],"collision":{"layers":["terrain"],"response":"stop"},"carry":{"target":"$caster"},"completion":{"type":"end"}}`,
		`"kind":"projectile","duration_ticks":10,"motion":{"completion":{"type":"end"},"carry":{"target":"$caster"},"collision":{"response":"stop","layers":["terrain"]},"offsets":[{"period_ticks":2,"amplitude":2,"type":"zigzag"}],"trajectory":{"speed":10,"type":"linear"},"steering":{"duration_ticks":10,"target":"$caster","type":"tracking"},"frame":{"type":"world"}}`,
	}
	want := []string{"frame", "steering", "trajectory", "offsets", "collision", "carry", "completion", "signals"}
	for index, motion := range motions {
		program, diagnostics := Compile(mustParseJSON(t, motionSkillJSON(motion)), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		host := &recordingMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 100}})
		runtime := NewRuntime(host, RuntimeOptions{})
		cast := &castInstance{id: 1, program: program, caster: 1, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
		process := &ProcessInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, EndTick: 10, Program: program,
			HostState: ProcessHostState{ProcessID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatalf("motion %d: %v", index, err)
		}
		if !reflect.DeepEqual(host.stages, want) {
			t.Fatalf("motion %d stage trace = %v, want %v", index, host.stages, want)
		}
	}
}

func TestMotionLinearTrajectoryRoundsHalfAwayFromZero(t *testing.T) {
	program, diagnostics := Compile(mustParseJSON(t, motionSkillJSON(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}`)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := NewMemoryHost(program.AuthorityIdentity())
	runtime := NewRuntime(host, RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	process := &ProcessInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, EndTick: 10, Program: program,
		HostState: ProcessHostState{ProcessID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: 5000, Y: -5000}},
	}
	if _, err := runtime.stepProcessMotion(cast, process); err != nil {
		t.Fatal(err)
	}
	if process.Motion.Position != (Position{X: 1, Y: -1}) {
		t.Fatalf("position = %#v, want half-away-from-zero displacement (1,-1)", process.Motion.Position)
	}
}

func TestMotionPipelineAggregatesStageSignalsAndEmitsTargetLostOnce(t *testing.T) {
	program, diagnostics := Compile(mustParseJSON(t, motionSkillJSON(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"collision":{"layers":["terrain"],"response":"stop"},"carry":{"target":"$caster"},"completion":{"type":"end"}}`)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := &signalMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
	runtime := NewRuntime(host, RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, caster: 99, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	process := &ProcessInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, EndTick: 10, Program: program,
		HostState: ProcessHostState{ProcessID: 1, Active: true},
	}

	first, err := runtime.stepProcessMotion(cast, process)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []ProcessSignal{
		{Kind: ProcessSignalHit, Target: 3, Distance: 20, ContactOrdinal: 2},
		{Kind: ProcessSignalCollision, Target: 4, Distance: 20, ContactOrdinal: 2},
		{Kind: ProcessSignalTargetLost, Target: 99},
		{Kind: ProcessSignalTransition, Target: 6},
		{Kind: ProcessSignalLeave, Target: 2},
		{Kind: ProcessSignalEnter, Target: 1},
		{Kind: ProcessSignalTick},
	}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Fatalf("first signals = %#v, want %#v", first, wantFirst)
	}
	if !reflect.DeepEqual(host.signalInputs[0], wantFirst) {
		t.Fatalf("signals stage input = %#v, want %#v", host.signalInputs[0], wantFirst)
	}
	if !process.Motion.TargetLostEmitted {
		t.Fatal("target loss was not recorded in process state")
	}
	if process.Motion.CarryAttached || process.Motion.CarryTarget != 0 {
		t.Fatalf("missing carry target marked attached: target=%d attached=%v", process.Motion.CarryTarget, process.Motion.CarryAttached)
	}

	second, err := runtime.stepProcessMotion(cast, process)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range second {
		if signal.Kind == ProcessSignalTargetLost {
			t.Fatalf("duplicate target-lost signal on second step: %#v", second)
		}
	}
}

func TestMotionPayloadsControlTrajectoryAndSteering(t *testing.T) {
	t.Run("path speed advances through segments", func(t *testing.T) {
		inputSchema := `{"type":"path","maximum_points":4,"maximum_total_length":40,"minimum_segment_length":1,"simplification_policy":"reject","clamp_policy":"reject"}`
		program := compileMotionTestProgram(t, inputSchema, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"path","points":"$input.path","speed":4},"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, map[string]RuntimeValue{"$input.path": PathRuntimeValue([]Position{{}, {X: 10}, {X: 10, Y: 10}})})
		want := []Position{{X: 4}, {X: 8}, {X: 10, Y: 2}}
		for index, expected := range want {
			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
			if process.Motion.Position != expected {
				t.Fatalf("step %d position = %#v, want %#v", index+1, process.Motion.Position, expected)
			}
		}
	})

	t.Run("orbit angular speed advances angle", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":90000},"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, nil)
		cast.caster = 1
		runtime.host.(*MemoryHost).UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}})
		cast.visibleRevision = runtime.host.CurrentRevision()
		want := []Position{{Y: 10}, {X: -10}}
		for index, expected := range want {
			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
			if process.Motion.Position != expected {
				t.Fatalf("step %d position = %#v, want %#v", index+1, process.Motion.Position, expected)
			}
		}
	})

	t.Run("parabola uses height and reaches destination at duration", func(t *testing.T) {
		inputSchema := `{"type":"position","maximum_range":20,"clamp_policy":"reject"}`
		program := compileMotionTestProgram(t, inputSchema, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$input.position","height":4,"duration_ticks":4},"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, map[string]RuntimeValue{"$input.position": PositionRuntimeValue(Position{X: 8})})
		want := []Position{{X: 2, Y: 3}, {X: 4, Y: 4}, {X: 6, Y: 3}, {X: 8}}
		for index, expected := range want {
			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
			if process.Motion.Position != expected {
				t.Fatalf("step %d position = %#v, want %#v", index+1, process.Motion.Position, expected)
			}
		}
	})

	t.Run("circular offset angular speed advances angle", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"circular","radius":2,"angular_speed":90000}],"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, nil)
		process.Motion.Direction = Direction{X: normalizedDirectionScale}
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position != (Position{X: 1, Y: 2}) {
			t.Fatalf("position = %#v, want angular offset (1,2)", process.Motion.Position)
		}
	})

	t.Run("tracking stops refreshing after duration", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, nil)
		cast.caster = 1
		host := runtime.host.(*MemoryHost)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{Y: 10}})
		cast.visibleRevision = host.CurrentRevision()
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 10}})
		cast.visibleRevision = host.CurrentRevision()
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position != (Position{Y: 2}) || process.Motion.Direction != (Direction{Y: normalizedDirectionScale}) {
			t.Fatalf("position=%#v direction=%#v, want expired tracking to retain north", process.Motion.Position, process.Motion.Direction)
		}
	})
}

func TestMotionFixedSteeringAndOffsetsUseStableTrajectoryBase(t *testing.T) {
	t.Run("fixed steering starts east", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":2},"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, nil)
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}
		if process.Motion.Direction != (Direction{X: normalizedDirectionScale}) || process.Motion.Position != (Position{X: 2}) {
			t.Fatalf("direction=%#v position=%#v, want deterministic eastward start", process.Motion.Direction, process.Motion.Position)
		}
	})

	t.Run("offsets do not feed back into trajectory", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":2},"offsets":[{"type":"zigzag","amplitude":3,"period_ticks":1}],"completion":{"type":"end"}}`)
		runtime, cast, process := newMotionTestRuntime(program, nil)
		process.Motion.Direction = Direction{X: normalizedDirectionScale}
		want := []struct {
			position   Position
			trajectory Position
		}{{Position{X: 2, Y: 3}, Position{X: 2}}, {Position{X: 4, Y: -3}, Position{X: 4}}}
		for index, expected := range want {
			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
			if process.Motion.Position != expected.position || process.Motion.TrajectoryPosition != expected.trajectory {
				t.Fatalf("step %d position=%#v trajectory=%#v, want position=%#v trajectory=%#v", index+1, process.Motion.Position, process.Motion.TrajectoryPosition, expected.position, expected.trajectory)
			}
		}
	})
}

func TestMotionCollisionAndCompletionAreBounded(t *testing.T) {
	for _, test := range []struct {
		name              string
		collision         string
		remaining         func(MotionState) int
		expectedDirection Direction
	}{
		{
			name:              "reflect consumes its catalog budget then stops",
			collision:         `"layers":["terrain"],"response":"reflect","max_reflects":2`,
			remaining:         func(state MotionState) int { return state.ReflectCount },
			expectedDirection: Direction{X: -normalizedDirectionScale},
		},
		{
			name:              "pierce consumes its catalog budget then stops",
			collision:         `"layers":["terrain"],"response":"pierce","max_pierces":2`,
			remaining:         func(state MotionState) int { return state.PierceCount },
			expectedDirection: Direction{X: normalizedDirectionScale},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":20,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"collision":{`+test.collision+`},"completion":{"type":"end"}}`)
			host := &boundedMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity()), collide: true}
			runtime, cast, process := newBoundedMotionRuntime(program, host)

			first, err := runtime.stepProcessMotion(cast, process)
			if err != nil {
				t.Fatal(err)
			}
			if got := test.remaining(process.Motion); got != 1 {
				t.Fatalf("remaining collision budget = %d, want 1", got)
			}
			if process.Motion.Stage != MotionStageOutbound || process.Motion.Direction != test.expectedDirection {
				t.Fatalf("first collision state = stage %d direction %#v", process.Motion.Stage, process.Motion.Direction)
			}
			if got := countProcessSignals(first, ProcessSignalTransition); got != 1 {
				t.Fatalf("first collision transitions = %d, want 1: %#v", got, first)
			}

			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
			if got := test.remaining(process.Motion); got != 0 {
				t.Fatalf("exhausted collision budget = %d, want 0", got)
			}
			if process.Motion.Stage != MotionStageCompleted {
				t.Fatalf("stage after budget exhaustion = %d, want completed", process.Motion.Stage)
			}
		})
	}

	t.Run("boomerang pauses and returns within its tick bound", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":1,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"boomerang","max_return_ticks":2}}`)
		host := &boundedMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}})
		runtime, cast, process := newBoundedMotionRuntime(program, host)
		process.Owner = 1
		process.EndTick = 1
		process.HostState.Position = Position{X: 3}
		process.Motion.Position = Position{X: 3}
		runtime.currentTick = 1
		cast.visibleRevision = host.CurrentRevision()

		outbound, err := runtime.stepProcessMotion(cast, process)
		if err != nil {
			t.Fatal(err)
		}
		if process.Motion.Stage != MotionStagePaused || countProcessSignals(outbound, ProcessSignalTransition) != 1 {
			t.Fatalf("outbound completion = stage %d signals %#v, want one transition into pause", process.Motion.Stage, outbound)
		}
		pausedAt := process.Motion.Position

		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}})
		cast.visibleRevision = host.CurrentRevision()
		runtime.currentTick = 2
		paused, err := runtime.stepProcessMotion(cast, process)
		if err != nil {
			t.Fatal(err)
		}
		if process.Motion.Position != pausedAt {
			t.Fatalf("boomerang moved during pause: %#v -> %#v", pausedAt, process.Motion.Position)
		}
		if process.Motion.Stage != MotionStageReturning || process.Motion.FrameAnchor != (Position{X: 1}) || countProcessSignals(paused, ProcessSignalTransition) != 1 {
			t.Fatalf("return entry = stage %d anchor %#v signals %#v", process.Motion.Stage, process.Motion.FrameAnchor, paused)
		}

		for tick := Tick(3); tick <= 4 && process.Motion.Stage != MotionStageCompleted; tick++ {
			runtime.currentTick = tick
			if _, err := runtime.stepProcessMotion(cast, process); err != nil {
				t.Fatal(err)
			}
		}
		if process.Motion.Stage != MotionStageCompleted || process.Motion.ReturnCount > 2 {
			t.Fatalf("bounded return = stage %d ticks %d", process.Motion.Stage, process.Motion.ReturnCount)
		}
	})

	t.Run("missing return target emits target lost once and ends", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":1,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"boomerang","max_return_ticks":2}}`)
		host := &boundedMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
		runtime, cast, process := newBoundedMotionRuntime(program, host)
		process.Owner = 99
		process.EndTick = 1
		runtime.currentTick = 1
		if _, err := runtime.stepProcessMotion(cast, process); err != nil {
			t.Fatal(err)
		}

		runtime.currentTick = 2
		lost, err := runtime.stepProcessMotion(cast, process)
		if err != nil {
			t.Fatal(err)
		}
		if process.Motion.Stage != MotionStageCompleted || countProcessSignals(lost, ProcessSignalTargetLost) != 1 {
			t.Fatalf("missing return target = stage %d signals %#v", process.Motion.Stage, lost)
		}
		runtime.currentTick = 3
		again, err := runtime.stepProcessMotion(cast, process)
		if err != nil {
			t.Fatal(err)
		}
		if countProcessSignals(again, ProcessSignalTargetLost) != 0 {
			t.Fatalf("duplicate target-lost signal: %#v", again)
		}
	})
}

func TestCarryDetachesExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		collision bool
		run       func(*Runtime, *castInstance, *ProcessInstance) error
	}{
		{
			name: "end",
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				runtime.currentTick = process.EndTick
				if _, err := runtime.stepProcessMotion(cast, process); err != nil {
					return err
				}
				return runtime.stopProcess(cast, process, StopCauseEnd)
			},
		},
		{
			name:      "collision",
			collision: true,
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				if _, err := runtime.stepProcessMotion(cast, process); err != nil {
					return err
				}
				return runtime.stopProcess(cast, process, StopCauseEnd)
			},
		},
		{
			name: "goto",
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				process.Scope = ProcessScopePhase
				runtime.processes[process.ID] = process
				if err := runtime.stopProcesses(cast, false); err != nil {
					return err
				}
				return runtime.stopProcesses(cast, false)
			},
		},
		{
			name: "finish",
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				process.Scope = ProcessScopeCast
				runtime.processes[process.ID] = process
				if err := runtime.stopFinishingProcesses(cast); err != nil {
					return err
				}
				return runtime.stopFinishingProcesses(cast)
			},
		},
		{
			name: "cancel",
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				process.Scope = ProcessScopePhase
				runtime.processes[process.ID] = process
				if err := runtime.stopProcesses(cast, true); err != nil {
					return err
				}
				return runtime.stopProcesses(cast, true)
			},
		},
		{
			name: "host error",
			run: func(runtime *Runtime, cast *castInstance, process *ProcessInstance) error {
				process.Scope = ProcessScopePhase
				runtime.processes[process.ID] = process
				runtime.casts[cast.id] = cast
				cast.status = CastSuspended
				cast.pendingTasks = 1
				frame := FrameID(1)
				runtime.frames[frame] = nil
				err := runtime.executeScheduledTask(scheduledTask{Payload: &processStepTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame, ProcessID: process.ID}})
				if err == nil {
					return errors.New("expected Host step error")
				}
				return runtime.stopProcess(cast, process, StopCauseFailure)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			collision := ""
			if test.collision {
				collision = `,"collision":{"layers":["terrain"],"response":"stop"}`
			}
			program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":3,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1}`+collision+`,"carry":{"target":"$caster"},"completion":{"type":"end"}}`)
			host := &carryLifecycleHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity()), collide: test.collision, failFrame: test.name == "host error"}
			host.UpsertEntity(MemoryEntity{ID: 2, Alive: true})
			runtime, cast, process := newBoundedMotionRuntime(program, host)
			cast.caster = 2
			cast.visibleRevision = host.CurrentRevision()
			process.Owner = 2
			process.EndTick = 3
			process.Motion = MotionState{
				Initialized: true, Stage: MotionStageOutbound, Direction: Direction{X: normalizedDirectionScale},
				CarryTarget: 2, CarryAttached: true,
			}

			if err := test.run(runtime, cast, process); err != nil {
				t.Fatal(err)
			}
			if host.detachCommands != 1 {
				t.Fatalf("typed detach commands = %d, want exactly 1", host.detachCommands)
			}
			if process.Motion.CarryAttached || process.Motion.CarryTarget != 0 {
				t.Fatalf("process retained carry attachment: target=%d attached=%v", process.Motion.CarryTarget, process.Motion.CarryAttached)
			}
		})
	}
}

func TestOwnedMotionTerminalCompletionReapsImmediately(t *testing.T) {
	for _, test := range []struct {
		name       string
		completion string
		collision  string
		run        func(*Runtime, *ownedCarryLifecycleHost) error
	}{
		{
			name:       "collision",
			completion: `{"type":"end"}`,
			collision:  `,"collision":{"layers":["terrain"],"response":"stop"}`,
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost) error {
				host.collisionAt = 2
				return runtime.Advance(1)
			},
		},
		{
			name:       "missing return target",
			completion: `{"type":"boomerang","max_return_ticks":2}`,
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost) error {
				if err := runtime.Advance(1); err != nil {
					return err
				}
				host.missingOwner = true
				return runtime.Advance(2)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, host, process := startOwnedCarryLifecycle(t, test.completion, test.collision)
			callbacks := 0
			callbackSawAttached := false
			host.observeApply = func() {
				callbacks++
				callbackSawAttached = process.Motion.CarryAttached || process.Motion.CarryTarget != 0
			}

			if err := test.run(runtime, host); err != nil {
				t.Fatal(err)
			}
			if callbacks != 1 || callbackSawAttached {
				t.Fatalf("end callback count=%d saw_attached=%v, want one detached callback", callbacks, callbackSawAttached)
			}
			if len(runtime.OwnedProcesses(1)) != 0 || process.Status != ProcessEnded {
				t.Fatalf("terminal process still active: owned=%#v status=%q", runtime.OwnedProcesses(1), process.Status)
			}
			if host.detachCommands != 1 {
				t.Fatalf("detach commands=%d, want exactly 1", host.detachCommands)
			}
			if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_process_callback_end"); got != 1 {
				t.Fatalf("end callbacks=%d, want 1", got)
			}
		})
	}
}

func TestOwnedProcessCancellationDetachesBeforeCallback(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Runtime, *ownedCarryLifecycleHost, *castInstance, *ProcessInstance) error
	}{
		{
			name: "invalid handoff",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, cast *castInstance, process *ProcessInstance) error {
				process.handedOff = false
				delete(runtime.ownedProcesses, process.ID)
				host.missingLifecycle = true
				return runtime.handoffEntityProcesses(cast)
			},
		},
		{
			name: "reap unhanded",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, process *ProcessInstance) error {
				process.handedOff = false
				delete(runtime.ownedProcesses, process.ID)
				host.missingLifecycle = true
				return runtime.reapUnhandedEntityProcesses()
			},
		},
		{
			name: "reap invalid owned",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, _ *ProcessInstance) error {
				host.missingLifecycle = true
				return runtime.reapInvalidOwnedProcesses()
			},
		},
		{
			name: "reap owned",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, _ *ProcessInstance) error {
				host.missingLifecycle = true
				return runtime.reapOwnedProcesses()
			},
		},
		{
			name: "remove program",
			run: func(runtime *Runtime, _ *ownedCarryLifecycleHost, _ *castInstance, process *ProcessInstance) error {
				return runtime.RemoveProgram(process.Program.id)
			},
		},
		{
			name: "shutdown",
			run: func(runtime *Runtime, _ *ownedCarryLifecycleHost, _ *castInstance, _ *ProcessInstance) error {
				return runtime.Shutdown()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, host, process := startOwnedCarryLifecycle(t, `{"type":"end"}`, "")
			cast := runtime.casts[process.CastID]
			callbacks := 0
			callbackSawAttached := false
			host.observeApply = func() {
				callbacks++
				callbackSawAttached = process.Motion.CarryAttached || process.Motion.CarryTarget != 0
			}

			if err := test.run(runtime, host, cast, process); err != nil {
				t.Fatal(err)
			}
			if callbacks != 1 || callbackSawAttached {
				t.Fatalf("cancel callback count=%d saw_attached=%v, want one detached callback", callbacks, callbackSawAttached)
			}
			if host.detachCommands != 1 {
				t.Fatalf("detach commands=%d, want exactly 1", host.detachCommands)
			}
		})
	}
}

func TestInitialMotionStepFailureDetachesCarry(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"carry":{"target":"$caster"},"completion":{"type":"end"}}}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "initial-motion-carry-cleanup", flow)
	host := &postAttachFailureHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err == nil {
		t.Fatal("expected initial motion Host error")
	}
	if host.attachCommands != 1 || host.detachCommands != 1 {
		t.Fatalf("carry commands attach=%d detach=%d, want exactly one of each", host.attachCommands, host.detachCommands)
	}
}

func countProcessSignals(signals []ProcessSignal, kind ProcessSignalKind) int {
	count := 0
	for _, signal := range signals {
		if signal.Kind == kind {
			count++
		}
	}
	return count
}

type boundedMotionHost struct {
	*MemoryHost
	collide bool
}

type carryLifecycleHost struct {
	*MemoryHost
	collide        bool
	failFrame      bool
	detachCommands int
}

type ownedCarryLifecycleHost struct {
	*MemoryHost
	collisionAt      int
	collisionSteps   int
	missingOwner     bool
	missingLifecycle bool
	detachCommands   int
	observeApply     func()
}

type postAttachFailureHost struct {
	*MemoryHost
	attachCommands int
	detachCommands int
}

func (host *postAttachFailureHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok {
		if carry.Attached {
			host.attachCommands++
		} else {
			host.detachCommands++
		}
	}
	if _, ok := command.Motion.(CompletionMotionStep); ok {
		return ProcessStepResult{}, errors.New("completion step failure")
	}
	return host.MemoryHost.StepProcess(command, state)
}

func (host *ownedCarryLifecycleHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok && !carry.Attached {
		host.detachCommands++
	}
	result, err := host.MemoryHost.StepProcess(command, state)
	if err != nil {
		return result, err
	}
	if _, ok := command.Motion.(CollisionMotionStep); ok {
		host.collisionSteps++
		if host.collisionAt != 0 && host.collisionSteps == host.collisionAt {
			result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalCollision, Target: 7})
		}
	}
	return result, nil
}

func (host *ownedCarryLifecycleHost) Read(request ReadRequest) (ReadResult, error) {
	if position, ok := request.Payload.(PositionRead); ok && host.missingOwner && position.Entity == 1 {
		return ReadResult{}, ErrEntityNotFound
	}
	return host.MemoryHost.Read(request)
}

func (host *ownedCarryLifecycleHost) Apply(command EffectCommand) (EffectResult, error) {
	if host.observeApply != nil {
		host.observeApply()
	}
	return host.MemoryHost.Apply(command)
}

func (host *ownedCarryLifecycleHost) OwnedEntity(entity EntityID) (OwnedEntityMetadata, bool) {
	if host.missingLifecycle {
		return OwnedEntityMetadata{}, false
	}
	return host.MemoryHost.OwnedEntity(entity)
}

func startOwnedCarryLifecycle(t *testing.T, completion, collision string) (*Runtime, *ownedCarryLifecycleHost, *ProcessInstance) {
	t.Helper()
	duration := "10"
	if strings.Contains(completion, `"boomerang"`) {
		duration = "1"
	}
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{"kind":"projectile","duration_ticks":` + duration + `,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1}` + collision + `,"carry":{"target":"$caster"},"completion":` + completion + `}},"on":{"end":` + callback + `,"cancel":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "motion-terminal-lifecycle", flow)
	host := &ownedCarryLifecycleHost{MemoryHost: runtimeTestHost(environment)}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ownedProcesses) != 1 {
		t.Fatalf("owned process count=%d, want 1", len(runtime.ownedProcesses))
	}
	for _, process := range runtime.ownedProcesses {
		return runtime, host, process
	}
	t.Fatal("missing owned process")
	return nil, nil, nil
}

func (host *carryLifecycleHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok && !carry.Attached {
		host.detachCommands++
	}
	if host.failFrame {
		if _, ok := command.Motion.(FrameMotionStep); ok {
			return ProcessStepResult{}, errors.New("motion Host failure")
		}
	}
	result, err := host.MemoryHost.StepProcess(command, state)
	if err == nil && host.collide {
		if _, ok := command.Motion.(CollisionMotionStep); ok {
			result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalCollision, Target: 7})
		}
	}
	return result, err
}

func (host *boundedMotionHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	result, err := host.MemoryHost.StepProcess(command, state)
	if err == nil && host.collide {
		if _, ok := command.Motion.(CollisionMotionStep); ok {
			result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalCollision, Target: 7})
		}
	}
	return result, err
}

func newBoundedMotionRuntime(program *Program, host Host) (*Runtime, *castInstance, *ProcessInstance) {
	runtime := NewRuntime(host, RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	process := &ProcessInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, EndTick: 20, Program: program,
		HostState: ProcessHostState{ProcessID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
	}
	return runtime, cast, process
}

func TestMovingProcessUsesTemplateLifetimeAndCompletesOnTerminalTick(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"process":{"kind":"projectile","duration_ticks":2,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}},"on":{"tick":` + callback + `,"end":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "motion-terminal", flow)
	host := &terminalMotionHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	processes := runtime.OwnedProcesses(1)
	if len(processes) != 1 || processes[0].EndTick != 2 {
		t.Fatalf("owned processes = %#v, want process template end tick 2", processes)
	}
	if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_process_callback_tick"); got != 1 {
		t.Fatalf("start tick callbacks = %d, want 1", got)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_process_callback_tick"); got != 2 {
		t.Fatalf("tick-1 callbacks = %d, want 2", got)
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	if len(runtime.OwnedProcesses(1)) != 0 {
		t.Fatalf("terminal process was not reaped: %#v", runtime.OwnedProcesses(1))
	}
	if !reflect.DeepEqual(host.completions, []terminalCompletion{{Tick: 0, Complete: false}, {Tick: 1, Complete: false}, {Tick: 2, Complete: true}}) {
		t.Fatalf("completion steps = %#v", host.completions)
	}
	events := runtime.RuntimeEvents()
	if got := countRuntimeEvents(events, "owned_process_callback_tick"); got != 3 {
		t.Fatalf("terminal tick callbacks = %d, want exactly 3 total", got)
	}
	if got := countRuntimeEvents(events, "owned_process_callback_end"); got != 1 {
		t.Fatalf("end callbacks = %d, want 1", got)
	}
}

func TestTerminalCompletionEndSignalInvokesEndCallbackOnce(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"process":{"kind":"projectile","duration_ticks":2,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}},"on":{"tick":` + callback + `,"end":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "motion-terminal-end-signal", flow)
	host := &terminalMotionHost{MemoryHost: runtimeTestHost(environment), emitEndOnCompletion: true}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	events := runtime.RuntimeEvents()
	if got := countRuntimeEvents(events, "owned_process_callback_end"); got != 1 {
		t.Fatalf("end callbacks = %d, want exactly 1 after terminal completion signal", got)
	}
	if got := countRuntimeEvents(events, "owned_process_callback_tick"); got != 3 {
		t.Fatalf("tick callbacks = %d, want 3 motion-pipeline ticks", got)
	}
}

func countRuntimeEvents(events []RuntimeEvent, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

type terminalCompletion struct {
	Tick     Tick
	Complete bool
}

type terminalMotionHost struct {
	*MemoryHost
	completions         []terminalCompletion
	emitEndOnCompletion bool
}

func (host *terminalMotionHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	if completion, ok := command.Motion.(CompletionMotionStep); ok {
		host.completions = append(host.completions, terminalCompletion{Tick: host.tick, Complete: completion.Complete})
	}
	result, err := host.MemoryHost.StepProcess(command, state)
	if err != nil {
		return result, err
	}
	if completion, ok := command.Motion.(CompletionMotionStep); ok && completion.Complete && host.emitEndOnCompletion {
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalEnd})
	}
	return result, nil
}

func compileMotionTestProgram(t *testing.T, inputSchema, process string) *Program {
	t.Helper()
	json := strings.Replace(motionSkillJSON(process), `"input_schema":{"type":"none"}`, `"input_schema":`+inputSchema, 1)
	program, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

func newMotionTestRuntime(program *Program, values map[string]RuntimeValue) (*Runtime, *castInstance, *ProcessInstance) {
	host := NewMemoryHost(program.AuthorityIdentity())
	runtime := NewRuntime(host, RuntimeOptions{})
	inputs := make([]RuntimeValue, len(program.input.slots))
	for index, slot := range program.input.slots {
		if value, ok := values[slot.name]; ok {
			inputs[index] = value
		} else {
			inputs[index] = MissingRuntimeValue(slot.typ)
		}
	}
	cast := &castInstance{id: 1, program: program, inputs: inputs, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	process := &ProcessInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: ProcessRunning, EndTick: 10, Program: program,
		HostState: ProcessHostState{ProcessID: 1, Active: true},
	}
	return runtime, cast, process
}

func TestMotionProgramDigestIncludesConcreteStructure(t *testing.T) {
	compile := func(motion string) string {
		t.Helper()
		program, diagnostics := Compile(mustParseJSON(t, motionSkillJSON(motion)), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		return program.identity.gameplayDigest
	}
	first := compile(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`)
	reordered := compile(`"motion":{"completion":{"type":"end"},"trajectory":{"speed":10,"type":"linear"},"frame":{"type":"world"}},"duration_ticks":10,"kind":"projectile"`)
	changed := compile(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":11},"completion":{"type":"end"}}`)
	if first != reordered {
		t.Fatalf("equivalent concrete motion digests differ: %q != %q", first, reordered)
	}
	if first == changed {
		t.Fatal("trajectory structure did not affect gameplay digest")
	}
}

type recordingMotionHost struct {
	*MemoryHost
	stages []string
}

type signalMotionHost struct {
	*MemoryHost
	signalInputs [][]ProcessSignal
}

func (host *signalMotionHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	if signals, ok := command.Motion.(SignalsMotionStep); ok {
		host.signalInputs = append(host.signalInputs, append([]ProcessSignal(nil), signals.Signals...))
		return host.MemoryHost.StepProcess(command, state)
	}
	result, err := host.MemoryHost.StepProcess(command, state)
	if err != nil {
		return result, err
	}
	switch command.Motion.(type) {
	case FrameMotionStep:
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalEnter, Target: 1})
	case SteeringMotionStep:
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalLeave, Target: 2})
	case TrajectoryMotionStep:
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalHit, Target: 3, Distance: 20, ContactOrdinal: 2})
	case OffsetsMotionStep:
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalCollision, Target: 4, Distance: 20, ContactOrdinal: 2})
	case CollisionMotionStep:
		result.Signals = append(result.Signals, ProcessSignal{Kind: ProcessSignalTransition, Target: 6})
	}
	return result, nil
}

func (host *recordingMotionHost) StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error) {
	switch command.Motion.(type) {
	case FrameMotionStep:
		host.stages = append(host.stages, "frame")
	case SteeringMotionStep:
		host.stages = append(host.stages, "steering")
	case TrajectoryMotionStep:
		host.stages = append(host.stages, "trajectory")
	case OffsetsMotionStep:
		host.stages = append(host.stages, "offsets")
	case CollisionMotionStep:
		host.stages = append(host.stages, "collision")
	case CarryMotionStep:
		host.stages = append(host.stages, "carry")
	case CompletionMotionStep:
		host.stages = append(host.stages, "completion")
	case SignalsMotionStep:
		host.stages = append(host.stages, "signals")
	default:
		typeName := reflect.TypeOf(command.Motion)
		host.stages = append(host.stages, "unexpected:"+typeName.String())
	}
	return host.MemoryHost.StepProcess(command, state)
}

func TestCanonicalMotionRequiresExplicitTypedStages(t *testing.T) {
	for _, test := range []struct {
		name        string
		change      string
		parseReject bool
	}{
		{"moving projectile requires motion", `"kind":"projectile"`, false},
		{"dash rejects flat speed", `"kind":"dash","speed":10`, true},
		{"orbit requires anchor", `"kind":"orbit","motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","radius":5,"angular_speed":10},"completion":{"type":"end"}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, err := Parse([]byte(motionSkillJSON(test.change)))
			if test.parseReject {
				if err == nil {
					t.Fatal("expected strict motion schema rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, diagnostics := compileToArtifacts(definition, DefaultCompileEnvironment())
			if len(diagnostics) == 0 {
				t.Fatal("expected canonical motion diagnostic")
			}
		})
	}
}

func TestMotionRejectsInvalidCombinations(t *testing.T) {
	for _, test := range []struct {
		name   string
		change string
	}{
		{"reflect requires collision layers", `"kind":"projectile","motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"},"collision":{"response":"reflect","max_reflects":1}}`},
		{"boomerang requires bounded return", `"kind":"projectile","motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"boomerang"}}`},
		{"tracking requires bounded duration", `"kind":"projectile","motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$input.target"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`},
		{"offset count is bounded", `"kind":"projectile","motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1},{"type":"zigzag","amplitude":1,"period_ticks":1}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(test.change)), DefaultCompileEnvironment())
			if len(diagnostics) == 0 {
				t.Fatal("expected invalid motion diagnostic")
			}
		})
	}

	t.Run("rejects unknown collision layer", func(t *testing.T) {
		input := `"kind":"projectile","motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"},"collision":{"layers":["unknown"],"response":"stop"}}`
		_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(input)), DefaultCompileEnvironment())
		if len(diagnostics) == 0 {
			t.Fatal("expected unknown collision layer diagnostic")
		}
	})

	valid := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":2,"period_ticks":2}],"collision":{"layers":["terrain"],"response":"reflect","max_reflects":1},"carry":{"target":"$caster"},"completion":{"type":"end"}}`
	_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(valid)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
}

func TestMotionCatalogRestrictsStageVariantsPerProcessTrajectory(t *testing.T) {
	for _, test := range []struct {
		name    string
		process string
		allowed bool
	}{
		{
			name:    "beam stationary cannot carry",
			process: `"kind":"beam","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"carry":{"target":"$caster"},"completion":{"type":"end"}}`,
		},
		{
			name:    "area stationary cannot reflect",
			process: `"kind":"area","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"collision":{"layers":["terrain"],"response":"reflect","max_reflects":1},"completion":{"type":"end"}}`,
		},
		{
			name:    "orbit cannot track",
			process: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"completion":{"type":"end"}}`,
		},
		{
			name:    "orbit cannot zigzag",
			process: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"completion":{"type":"end"}}`,
		},
		{
			name:    "orbit cannot boomerang",
			process: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"completion":{"type":"boomerang","max_return_ticks":1}}`,
		},
		{
			name:    "projectile supports the cataloged composition",
			allowed: true,
			process: `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"collision":{"layers":["terrain"],"response":"reflect","max_reflects":1},"carry":{"target":"$caster"},"completion":{"type":"boomerang","max_return_ticks":1}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(test.process)), DefaultCompileEnvironment())
			if test.allowed {
				requireNoErrors(t, diagnostics)
				return
			}
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("unsupported stage combination compiled: %#v", diagnostics)
			}
		})
	}
}

func TestMotionValuesUseTypeAndMemoryValidation(t *testing.T) {
	t.Run("rejects a non-entity tracking target", func(t *testing.T) {
		input := `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster.position","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`
		_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(input)), DefaultCompileEnvironment())
		requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)
	})

	t.Run("rejects uninitialized motion memory target", func(t *testing.T) {
		input := motionSkillJSON(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$memory.target","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`)
		input = strings.Replace(input, `"memory":{}`, `"memory":{"target":{"type":"entity","default":null}}`, 1)
		_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireDiagnostic(t, diagnostics, DiagnosticMemoryMaybeUninitialized)
	})
}

func motionSkillJSON(process string) string {
	result := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{`+process+`}}`, 1)
	return strings.Replace(result, `"timeout_ticks":0`, `"timeout_ticks":10`, 1)
}
