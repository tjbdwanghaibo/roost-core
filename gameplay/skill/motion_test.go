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
		spawn := &SpawnInstance{
			ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, EndTick: 10, Program: program,
			HostState: SpawnHostState{SpawnID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
		}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
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
	spawn := &SpawnInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, EndTick: 10, Program: program,
		HostState: SpawnHostState{SpawnID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: 5000, Y: -5000}},
	}
	if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
		t.Fatal(err)
	}
	if spawn.Motion.Position != (Position{X: 1, Y: -1}) {
		t.Fatalf("position = %#v, want half-away-from-zero displacement (1,-1)", spawn.Motion.Position)
	}
}

func TestMotionPipelineAggregatesStageSignalsAndEmitsTargetLostOnce(t *testing.T) {
	program, diagnostics := Compile(mustParseJSON(t, motionSkillJSON(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"collision":{"layers":["terrain"],"response":"stop"},"carry":{"target":"$caster"},"completion":{"type":"end"}}`)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := &signalMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
	runtime := NewRuntime(host, RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, caster: 99, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	spawn := &SpawnInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, EndTick: 10, Program: program,
		HostState: SpawnHostState{SpawnID: 1, Active: true},
	}

	first, err := runtime.stepSpawnMotion(cast, spawn)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []SpawnSignal{
		{Kind: SpawnSignalHit, Target: 3, Distance: 20, ContactOrdinal: 2},
		{Kind: SpawnSignalCollision, Target: 4, Distance: 20, ContactOrdinal: 2},
		{Kind: SpawnSignalTargetLost, Target: 99},
		{Kind: SpawnSignalTransition, Target: 6},
		{Kind: SpawnSignalLeave, Target: 2},
		{Kind: SpawnSignalEnter, Target: 1},
		{Kind: SpawnSignalTick},
	}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Fatalf("first signals = %#v, want %#v", first, wantFirst)
	}
	if !reflect.DeepEqual(host.signalInputs[0], wantFirst) {
		t.Fatalf("signals stage input = %#v, want %#v", host.signalInputs[0], wantFirst)
	}
	if !spawn.Motion.TargetLostEmitted {
		t.Fatal("target loss was not recorded in spawn state")
	}
	if spawn.Motion.CarryAttached || spawn.Motion.CarryTarget != 0 {
		t.Fatalf("missing carry target marked attached: target=%d attached=%v", spawn.Motion.CarryTarget, spawn.Motion.CarryAttached)
	}

	second, err := runtime.stepSpawnMotion(cast, spawn)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range second {
		if signal.Kind == SpawnSignalTargetLost {
			t.Fatalf("duplicate target-lost signal on second step: %#v", second)
		}
	}
}

func TestMotionPayloadsControlTrajectoryAndSteering(t *testing.T) {
	t.Run("path speed advances through segments", func(t *testing.T) {
		// 实体衍生物的字段在移交后按衍生物上下文求值，读不到施法输入（RR-20261005-NC-224），
		// 所以 `points: "$input.path"` 不再能编译；这里直接驱动路径推进的算术。
		points := []Position{{}, {X: 10}, {X: 10, Y: 10}}
		position, index := Position{}, 0
		want := []Position{{X: 4}, {X: 8}, {X: 10, Y: 2}}
		for step, expected := range want {
			position = advanceMotionPath(position, points, 4, &index)
			if position != expected {
				t.Fatalf("step %d position = %#v, want %#v", step+1, position, expected)
			}
		}
	})

	t.Run("orbit angular speed advances angle", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":90000},"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		cast.caster = 1
		runtime.host.(*MemoryHost).UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}})
		cast.visibleRevision = runtime.host.CurrentRevision()
		want := []Position{{Y: 10}, {X: -10}}
		for index, expected := range want {
			if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
				t.Fatal(err)
			}
			if spawn.Motion.Position != expected {
				t.Fatalf("step %d position = %#v, want %#v", index+1, spawn.Motion.Position, expected)
			}
		}
	})

	t.Run("parabola uses height and reaches destination at duration", func(t *testing.T) {
		// 目的地取施法者位置：实体衍生物字段读不到施法输入（RR-20261005-NC-224）。
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$caster.position","height":4,"duration_ticks":4},"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		cast.caster = 1
		runtime.host.(*MemoryHost).UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 8}})
		cast.visibleRevision = runtime.host.CurrentRevision()
		want := []Position{{X: 2, Y: 3}, {X: 4, Y: 4}, {X: 6, Y: 3}, {X: 8}}
		for index, expected := range want {
			if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
				t.Fatal(err)
			}
			if spawn.Motion.Position != expected {
				t.Fatalf("step %d position = %#v, want %#v", index+1, spawn.Motion.Position, expected)
			}
		}
	})

	t.Run("circular offset angular speed advances angle", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"circular","radius":2,"angular_speed":90000}],"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		spawn.Motion.Direction = Direction{X: normalizedDirectionScale}
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position != (Position{X: 1, Y: 2}) {
			t.Fatalf("position = %#v, want angular offset (1,2)", spawn.Motion.Position)
		}
	})

	t.Run("tracking stops refreshing after duration", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		cast.caster = 1
		host := runtime.host.(*MemoryHost)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{Y: 10}})
		cast.visibleRevision = host.CurrentRevision()
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 10}})
		cast.visibleRevision = host.CurrentRevision()
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position != (Position{Y: 2}) || spawn.Motion.Direction != (Direction{Y: normalizedDirectionScale}) {
			t.Fatalf("position=%#v direction=%#v, want expired tracking to retain north", spawn.Motion.Position, spawn.Motion.Direction)
		}
	})
}

func TestMotionFixedSteeringAndOffsetsUseStableTrajectoryBase(t *testing.T) {
	t.Run("fixed steering starts east", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":2},"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Direction != (Direction{X: normalizedDirectionScale}) || spawn.Motion.Position != (Position{X: 2}) {
			t.Fatalf("direction=%#v position=%#v, want deterministic eastward start", spawn.Motion.Direction, spawn.Motion.Position)
		}
	})

	t.Run("offsets do not feed back into trajectory", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":2},"offsets":[{"type":"zigzag","amplitude":3,"period_ticks":1}],"completion":{"type":"end"}}`)
		runtime, cast, spawn := newMotionTestRuntime(program, nil)
		spawn.Motion.Direction = Direction{X: normalizedDirectionScale}
		want := []struct {
			position   Position
			trajectory Position
		}{{Position{X: 2, Y: 3}, Position{X: 2}}, {Position{X: 4, Y: -3}, Position{X: 4}}}
		for index, expected := range want {
			if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
				t.Fatal(err)
			}
			if spawn.Motion.Position != expected.position || spawn.Motion.TrajectoryPosition != expected.trajectory {
				t.Fatalf("step %d position=%#v trajectory=%#v, want position=%#v trajectory=%#v", index+1, spawn.Motion.Position, spawn.Motion.TrajectoryPosition, expected.position, expected.trajectory)
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
			runtime, cast, spawn := newBoundedMotionRuntime(program, host)

			first, err := runtime.stepSpawnMotion(cast, spawn)
			if err != nil {
				t.Fatal(err)
			}
			if got := test.remaining(spawn.Motion); got != 1 {
				t.Fatalf("remaining collision budget = %d, want 1", got)
			}
			if spawn.Motion.Stage != MotionStageOutbound || spawn.Motion.Direction != test.expectedDirection {
				t.Fatalf("first collision state = stage %d direction %#v", spawn.Motion.Stage, spawn.Motion.Direction)
			}
			if got := countSpawnSignals(first, SpawnSignalTransition); got != 1 {
				t.Fatalf("first collision transitions = %d, want 1: %#v", got, first)
			}

			if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
				t.Fatal(err)
			}
			if got := test.remaining(spawn.Motion); got != 0 {
				t.Fatalf("exhausted collision budget = %d, want 0", got)
			}
			if spawn.Motion.Stage != MotionStageCompleted {
				t.Fatalf("stage after budget exhaustion = %d, want completed", spawn.Motion.Stage)
			}
		})
	}

	t.Run("boomerang pauses and returns within its tick bound", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":1,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"boomerang","max_return_ticks":2}}`)
		host := &boundedMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}})
		runtime, cast, spawn := newBoundedMotionRuntime(program, host)
		spawn.Owner = 1
		spawn.EndTick = 1
		spawn.HostState.Position = Position{X: 3}
		spawn.Motion.Position = Position{X: 3}
		runtime.currentTick = 1
		cast.visibleRevision = host.CurrentRevision()

		outbound, err := runtime.stepSpawnMotion(cast, spawn)
		if err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Stage != MotionStagePaused || countSpawnSignals(outbound, SpawnSignalTransition) != 1 {
			t.Fatalf("outbound completion = stage %d signals %#v, want one transition into pause", spawn.Motion.Stage, outbound)
		}
		pausedAt := spawn.Motion.Position

		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}})
		cast.visibleRevision = host.CurrentRevision()
		runtime.currentTick = 2
		paused, err := runtime.stepSpawnMotion(cast, spawn)
		if err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Position != pausedAt {
			t.Fatalf("boomerang moved during pause: %#v -> %#v", pausedAt, spawn.Motion.Position)
		}
		if spawn.Motion.Stage != MotionStageReturning || spawn.Motion.FrameAnchor != (Position{X: 1}) || countSpawnSignals(paused, SpawnSignalTransition) != 1 {
			t.Fatalf("return entry = stage %d anchor %#v signals %#v", spawn.Motion.Stage, spawn.Motion.FrameAnchor, paused)
		}

		for tick := Tick(3); tick <= 4 && spawn.Motion.Stage != MotionStageCompleted; tick++ {
			runtime.currentTick = tick
			if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
				t.Fatal(err)
			}
		}
		if spawn.Motion.Stage != MotionStageCompleted || spawn.Motion.ReturnCount > 2 {
			t.Fatalf("bounded return = stage %d ticks %d", spawn.Motion.Stage, spawn.Motion.ReturnCount)
		}
	})

	t.Run("missing return target emits target lost once and ends", func(t *testing.T) {
		program := compileMotionTestProgram(t, `{"type":"none"}`, `"kind":"projectile","duration_ticks":1,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"boomerang","max_return_ticks":2}}`)
		host := &boundedMotionHost{MemoryHost: NewMemoryHost(program.AuthorityIdentity())}
		runtime, cast, spawn := newBoundedMotionRuntime(program, host)
		spawn.Owner = 99
		spawn.EndTick = 1
		runtime.currentTick = 1
		if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
			t.Fatal(err)
		}

		runtime.currentTick = 2
		lost, err := runtime.stepSpawnMotion(cast, spawn)
		if err != nil {
			t.Fatal(err)
		}
		if spawn.Motion.Stage != MotionStageCompleted || countSpawnSignals(lost, SpawnSignalTargetLost) != 1 {
			t.Fatalf("missing return target = stage %d signals %#v", spawn.Motion.Stage, lost)
		}
		runtime.currentTick = 3
		again, err := runtime.stepSpawnMotion(cast, spawn)
		if err != nil {
			t.Fatal(err)
		}
		if countSpawnSignals(again, SpawnSignalTargetLost) != 0 {
			t.Fatalf("duplicate target-lost signal: %#v", again)
		}
	})
}

func TestCarryDetachesExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		collision bool
		run       func(*Runtime, *castInstance, *SpawnInstance) error
	}{
		{
			name: "end",
			run: func(runtime *Runtime, cast *castInstance, spawn *SpawnInstance) error {
				runtime.currentTick = spawn.EndTick
				if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
					return err
				}
				return runtime.stopSpawn(cast, spawn, StopCauseEnd)
			},
		},
		{
			name:      "collision",
			collision: true,
			run: func(runtime *Runtime, cast *castInstance, spawn *SpawnInstance) error {
				if _, err := runtime.stepSpawnMotion(cast, spawn); err != nil {
					return err
				}
				return runtime.stopSpawn(cast, spawn, StopCauseEnd)
			},
		},
		{
			name: "cancel",
			run: func(runtime *Runtime, cast *castInstance, spawn *SpawnInstance) error {
				spawn.Scope = SpawnScopeEntity
				fileSpawnForTest(runtime, spawn)
				if err := runtime.stopCastSpawns(cast); err != nil {
					return err
				}
				return runtime.stopCastSpawns(cast)
			},
		},
		{
			name: "host error",
			run: func(runtime *Runtime, cast *castInstance, spawn *SpawnInstance) error {
				// 步进被宿主拒绝之后停止：解除 carry 恰好一次。之前经 spawnStepTask 步进；那类任务已删除，衍生物只由
				// advanceOwnedSpawns 推进（RR-20261006-51），这里直接走同一个步进函数。
				spawn.Scope = SpawnScopeEntity
				fileSpawnForTest(runtime, spawn)
				runtime.casts[cast.id] = cast
				if _, err := runtime.stepSpawnMotion(cast, spawn); err == nil {
					return errors.New("expected Host step error")
				}
				return runtime.stopSpawn(cast, spawn, StopCauseFailure)
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
			runtime, cast, spawn := newBoundedMotionRuntime(program, host)
			cast.caster = 2
			cast.visibleRevision = host.CurrentRevision()
			spawn.Owner = 2
			spawn.EndTick = 3
			spawn.Motion = MotionState{
				Initialized: true, Stage: MotionStageOutbound, Direction: Direction{X: normalizedDirectionScale},
				CarryTarget: 2, CarryAttached: true,
			}

			if err := test.run(runtime, cast, spawn); err != nil {
				t.Fatal(err)
			}
			if host.detachCommands != 1 {
				t.Fatalf("typed detach commands = %d, want exactly 1", host.detachCommands)
			}
			if spawn.Motion.CarryAttached || spawn.Motion.CarryTarget != 0 {
				t.Fatalf("spawn retained carry attachment: target=%d attached=%v", spawn.Motion.CarryTarget, spawn.Motion.CarryAttached)
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
			runtime, host, spawn := startOwnedCarryLifecycle(t, test.completion, test.collision)
			callbacks := 0
			callbackSawAttached := false
			host.observeApply = func() {
				callbacks++
				callbackSawAttached = spawn.Motion.CarryAttached || spawn.Motion.CarryTarget != 0
			}

			if err := test.run(runtime, host); err != nil {
				t.Fatal(err)
			}
			if callbacks != 1 || callbackSawAttached {
				t.Fatalf("end callback count=%d saw_attached=%v, want one detached callback", callbacks, callbackSawAttached)
			}
			if len(runtime.OwnedSpawns(1)) != 0 || spawn.Status != SpawnEnded {
				t.Fatalf("terminal spawn still active: owned=%#v status=%q", runtime.OwnedSpawns(1), spawn.Status)
			}
			if host.detachCommands != 1 {
				t.Fatalf("detach commands=%d, want exactly 1", host.detachCommands)
			}
			if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_spawn_callback_end"); got != 1 {
				t.Fatalf("end callbacks=%d, want 1", got)
			}
		})
	}
}

func TestOwnedSpawnCancellationDetachesBeforeCallback(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Runtime, *ownedCarryLifecycleHost, *castInstance, *SpawnInstance) error
	}{
		{
			name: "invalid handoff",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, cast *castInstance, spawn *SpawnInstance) error {
				runtime.spawns.setState(spawn, spawn.Status, false)
				host.missingLifecycle = true
				return runtime.handoffEntitySpawns(cast)
			},
		},
		{
			name: "reap unhanded",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, spawn *SpawnInstance) error {
				runtime.spawns.setState(spawn, spawn.Status, false)
				host.missingLifecycle = true
				return runtime.reapUnhandedEntitySpawns()
			},
		},
		{
			name: "reap invalid owned",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, _ *SpawnInstance) error {
				host.missingLifecycle = true
				return runtime.reapInvalidOwnedSpawns()
			},
		},
		{
			name: "reap owned",
			run: func(runtime *Runtime, host *ownedCarryLifecycleHost, _ *castInstance, _ *SpawnInstance) error {
				host.missingLifecycle = true
				return runtime.reapOwnedSpawns()
			},
		},
		{
			name: "remove program",
			run: func(runtime *Runtime, _ *ownedCarryLifecycleHost, _ *castInstance, spawn *SpawnInstance) error {
				return runtime.RemoveProgram(spawn.Program.id)
			},
		},
		{
			name: "shutdown",
			run: func(runtime *Runtime, _ *ownedCarryLifecycleHost, _ *castInstance, _ *SpawnInstance) error {
				return runtime.Shutdown()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, host, spawn := startOwnedCarryLifecycle(t, `{"type":"end"}`, "")
			cast := runtime.casts[spawn.CastID]
			callbacks := 0
			callbackSawAttached := false
			host.observeApply = func() {
				callbacks++
				callbackSawAttached = spawn.Motion.CarryAttached || spawn.Motion.CarryTarget != 0
			}

			if err := test.run(runtime, host, cast, spawn); err != nil {
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
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"carry":{"target":"$caster"},"completion":{"type":"end"}}}},{"flow":"finish"}]}`
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

func countSpawnSignals(signals []SpawnSignal, kind SpawnSignalKind) int {
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

func (host *postAttachFailureHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok {
		if carry.Attached {
			host.attachCommands++
		} else {
			host.detachCommands++
		}
	}
	if _, ok := command.Motion.(CompletionMotionStep); ok {
		return SpawnStepResult{}, errors.New("completion step failure")
	}
	return host.MemoryHost.StepSpawn(command, state)
}

func (host *ownedCarryLifecycleHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok && !carry.Attached {
		host.detachCommands++
	}
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err != nil {
		return result, err
	}
	if _, ok := command.Motion.(CollisionMotionStep); ok {
		host.collisionSteps++
		if host.collisionAt != 0 && host.collisionSteps == host.collisionAt {
			result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalCollision, Target: 7})
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

func startOwnedCarryLifecycle(t *testing.T, completion, collision string) (*Runtime, *ownedCarryLifecycleHost, *SpawnInstance) {
	t.Helper()
	duration := "10"
	if strings.Contains(completion, `"boomerang"`) {
		duration = "1"
	}
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"projectile","duration_ticks":` + duration + `,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1}` + collision + `,"carry":{"target":"$caster"},"completion":` + completion + `}},"on":{"end":` + callback + `,"cancel":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "motion-terminal-lifecycle", flow)
	host := &ownedCarryLifecycleHost{MemoryHost: runtimeTestHost(environment)}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if runtime.spawns.count(spawnHandedOff) != 1 {
		t.Fatalf("owned spawn count=%d, want 1", runtime.spawns.count(spawnHandedOff))
	}
	for _, spawn := range handedOffSpawnRecords(runtime) {
		return runtime, host, spawn
	}
	t.Fatal("missing owned spawn")
	return nil, nil, nil
}

func (host *carryLifecycleHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok && !carry.Attached {
		host.detachCommands++
	}
	if host.failFrame {
		if _, ok := command.Motion.(FrameMotionStep); ok {
			return SpawnStepResult{}, errors.New("motion Host failure")
		}
	}
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err == nil && host.collide {
		if _, ok := command.Motion.(CollisionMotionStep); ok {
			result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalCollision, Target: 7})
		}
	}
	return result, err
}

func (host *boundedMotionHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err == nil && host.collide {
		if _, ok := command.Motion.(CollisionMotionStep); ok {
			result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalCollision, Target: 7})
		}
	}
	return result, err
}

func newBoundedMotionRuntime(program *Program, host Host) (*Runtime, *castInstance, *SpawnInstance) {
	runtime := NewRuntime(host, RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, visibleRevision: host.CurrentRevision(), snapshots: make(map[int]RuntimeValue)}
	spawn := &SpawnInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, EndTick: 20, Program: program,
		HostState: SpawnHostState{SpawnID: 1, Active: true}, Motion: MotionState{Direction: Direction{X: normalizedDirectionScale}},
	}
	return runtime, cast, spawn
}

func TestMovingSpawnUsesTemplateLifetimeAndCompletesOnTerminalTick(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"projectile","duration_ticks":2,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}},"on":{"tick":` + callback + `,"end":` + callback + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "motion-terminal", flow)
	host := &terminalMotionHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	spawns := runtime.OwnedSpawns(1)
	if len(spawns) != 1 || spawns[0].EndTick != 2 {
		t.Fatalf("owned spawns = %#v, want spawn template end tick 2", spawns)
	}
	if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_spawn_callback_tick"); got != 1 {
		t.Fatalf("start tick callbacks = %d, want 1", got)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_spawn_callback_tick"); got != 2 {
		t.Fatalf("tick-1 callbacks = %d, want 2", got)
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	if len(runtime.OwnedSpawns(1)) != 0 {
		t.Fatalf("terminal spawn was not reaped: %#v", runtime.OwnedSpawns(1))
	}
	if !reflect.DeepEqual(host.completions, []terminalCompletion{{Tick: 0, Complete: false}, {Tick: 1, Complete: false}, {Tick: 2, Complete: true}}) {
		t.Fatalf("completion steps = %#v", host.completions)
	}
	events := runtime.RuntimeEvents()
	if got := countRuntimeEvents(events, "owned_spawn_callback_tick"); got != 3 {
		t.Fatalf("terminal tick callbacks = %d, want exactly 3 total", got)
	}
	if got := countRuntimeEvents(events, "owned_spawn_callback_end"); got != 1 {
		t.Fatalf("end callbacks = %d, want 1", got)
	}
}

func TestTerminalCompletionEndSignalInvokesEndCallbackOnce(t *testing.T) {
	callback := `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"projectile","duration_ticks":2,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}},"on":{"tick":` + callback + `,"end":` + callback + `}},{"flow":"finish"}]}`
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
	if got := countRuntimeEvents(events, "owned_spawn_callback_end"); got != 1 {
		t.Fatalf("end callbacks = %d, want exactly 1 after terminal completion signal", got)
	}
	if got := countRuntimeEvents(events, "owned_spawn_callback_tick"); got != 3 {
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

func (host *terminalMotionHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if completion, ok := command.Motion.(CompletionMotionStep); ok {
		host.completions = append(host.completions, terminalCompletion{Tick: host.tick, Complete: completion.Complete})
	}
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err != nil {
		return result, err
	}
	if completion, ok := command.Motion.(CompletionMotionStep); ok && completion.Complete && host.emitEndOnCompletion {
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalEnd})
	}
	return result, nil
}

func compileMotionTestProgram(t *testing.T, inputSchema, spawn string) *Program {
	t.Helper()
	json := strings.Replace(motionSkillJSON(spawn), `"input_schema":{"type":"none"}`, `"input_schema":`+inputSchema, 1)
	program, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

func newMotionTestRuntime(program *Program, values map[string]RuntimeValue) (*Runtime, *castInstance, *SpawnInstance) {
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
	spawn := &SpawnInstance{
		ID: 1, CastID: cast.id, TemplateIndex: 0, Status: SpawnRunning, EndTick: 10, Program: program,
		HostState: SpawnHostState{SpawnID: 1, Active: true},
	}
	return runtime, cast, spawn
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
	signalInputs [][]SpawnSignal
}

func (host *signalMotionHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if signals, ok := command.Motion.(SignalsMotionStep); ok {
		host.signalInputs = append(host.signalInputs, append([]SpawnSignal(nil), signals.Signals...))
		return host.MemoryHost.StepSpawn(command, state)
	}
	result, err := host.MemoryHost.StepSpawn(command, state)
	if err != nil {
		return result, err
	}
	switch command.Motion.(type) {
	case FrameMotionStep:
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalEnter, Target: 1})
	case SteeringMotionStep:
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalLeave, Target: 2})
	case TrajectoryMotionStep:
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalHit, Target: 3, Distance: 20, ContactOrdinal: 2})
	case OffsetsMotionStep:
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalCollision, Target: 4, Distance: 20, ContactOrdinal: 2})
	case CollisionMotionStep:
		result.Signals = append(result.Signals, SpawnSignal{Kind: SpawnSignalTransition, Target: 6})
	}
	return result, nil
}

func (host *recordingMotionHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
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
	return host.MemoryHost.StepSpawn(command, state)
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

func TestMotionCatalogRestrictsStageVariantsPerSpawnTrajectory(t *testing.T) {
	for _, test := range []struct {
		name    string
		spawn   string
		allowed bool
	}{
		{
			name:  "beam stationary cannot carry",
			spawn: `"kind":"beam","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"carry":{"target":"$caster"},"completion":{"type":"end"}}`,
		},
		{
			name:  "area stationary cannot reflect",
			spawn: `"kind":"area","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"collision":{"layers":["terrain"],"response":"reflect","max_reflects":1},"completion":{"type":"end"}}`,
		},
		{
			name:  "orbit cannot track",
			spawn: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"completion":{"type":"end"}}`,
		},
		{
			name:  "orbit cannot zigzag",
			spawn: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"completion":{"type":"end"}}`,
		},
		{
			name:  "orbit cannot boomerang",
			spawn: `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":1,"angular_speed":1},"completion":{"type":"boomerang","max_return_ticks":1}}`,
		},
		{
			name:    "projectile supports the cataloged composition",
			allowed: true,
			spawn:   `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":1},"trajectory":{"type":"linear","speed":1},"offsets":[{"type":"zigzag","amplitude":1,"period_ticks":1}],"collision":{"layers":["terrain"],"response":"reflect","max_reflects":1},"carry":{"target":"$caster"},"completion":{"type":"boomerang","max_return_ticks":1}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(test.spawn)), DefaultCompileEnvironment())
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

	t.Run("rejects a motion memory target", func(t *testing.T) {
		// 实体衍生物每一步在脱离施法的上下文里求 motion 的值，那里没有 cast memory：
		// 未初始化与否都不能读（RR-20261005-NC-224，此前报 MEMORY_MAYBE_UNINITIALIZED）。
		input := motionSkillJSON(`"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$memory.target","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`)
		input = strings.Replace(input, `"memory":{}`, `"memory":{"target":{"type":"entity","default":null}}`, 1)
		_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireDiagnostic(t, diagnostics, DiagnosticInputUnavailable)
	})
}

func motionSkillJSON(spawn string) string {
	// enter 以 wait + finish 结束：phase 没有计时，落空的 enter 不能编译（RR-20261005-NC-151）。
	return strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{`+spawn+`}},{"flow":"wait","ticks":10,"then":{"flow":"finish"}}]}`, 1)
}
