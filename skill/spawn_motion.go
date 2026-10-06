package skill

import "errors"

// stepSpawnMotion is the only Runtime path that turns a lowered motion
// program into Host work. Stages are deliberately written in canonical order;
// a Host only receives resolved integer facts for one stage at a time.
func (runtime *Runtime) stepSpawnMotion(cast *castInstance, spawn *SpawnInstance) ([]SpawnSignal, error) {
	if cast == nil || spawn == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return nil, ErrProgramInvariant
	}
	if err := runtime.initializeSpawnNumeric(cast, spawn); err != nil {
		return nil, err
	}
	_, err := advanceSpawnNumeric(spawn, runtime.currentTick)
	if err != nil {
		return nil, err
	}
	template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
	if template.motion == nil {
		initialized := spawn.Motion.Initialized
		spawn.Motion.Initialized = true
		signals, err := runtime.applySpawnMotionStep(cast, spawn, StaticMotionStep{Position: spawn.Motion.Position})
		if err != nil {
			return nil, err
		}
		if !initialized {
			return signals, nil
		}
		signals = appendMotionStageSignals(spawn, nil, signals)
		signals = append(signals, SpawnSignal{Kind: SpawnSignalTick})
		result, err := runtime.applySpawnMotionStep(cast, spawn, SignalsMotionStep{Signals: normalizeSpawnSignals(signals)})
		if err == nil {
			spawn.Motion.Tick++
		}
		return result, err
	}
	motion := template.motion
	stageSignals := make([]SpawnSignal, 0)
	if !spawn.Motion.Initialized {
		spawn.Motion.Initialized = true
		spawn.Motion.Stage = MotionStageOutbound
		spawn.Motion.Position = spawn.HostState.Position
		spawn.Motion.TrajectoryPosition = spawn.HostState.Position
		spawn.Motion.Origin = spawn.HostState.Position
		if motion.collision != nil {
			spawn.Motion.ReflectCount = motion.collision.maxReflects
			spawn.Motion.PierceCount = motion.collision.maxPierces
		}
	}
	if spawn.Motion.Stage == MotionStageCompleted {
		return nil, nil
	}

	position := spawn.Motion.TrajectoryPosition
	if follow, ok := motion.frame.(followMotionFrameProgram); ok {
		target, err := runtime.evalEntity(cast, follow.target)
		if err != nil {
			return nil, err
		}
		value, err := runtime.readEntityPosition(cast, target)
		if err != nil {
			return nil, err
		}
		var valid bool
		position, valid = value.Position()
		if !valid {
			return nil, ErrRuntimeTypeMismatch
		}
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, FrameMotionStep{Position: position})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position, spawn.Motion.TrajectoryPosition = position, position

	direction := spawn.Motion.Direction
	if _, fixed := motion.steering.(fixedMotionSteeringProgram); fixed && direction == (Direction{}) {
		direction = Direction{X: normalizedDirectionScale}
	}
	if tracking, ok := motion.steering.(trackingMotionSteeringProgram); ok && spawn.Motion.Tick < tracking.durationTicks {
		target, err := runtime.evalEntity(cast, tracking.target)
		if err != nil {
			return nil, err
		}
		value, err := runtime.readEntityPosition(cast, target)
		if err != nil {
			return nil, err
		}
		targetPosition, valid := value.Position()
		if !valid {
			return nil, ErrRuntimeTypeMismatch
		}
		// Euclidean length: the normalized direction's Euclidean norm is the
		// direction scale, so diagonal tracking moves at the same speed as
		// axis-aligned tracking (the historical Chebyshev length made
		// diagonal projectiles up to 41% faster).
		length := integerDistance(position, targetPosition)
		if length > 0 {
			direction = normalizedDirection(position, targetPosition, length)
		}
	}
	if spawn.Motion.Stage == MotionStageReturning && spawn.Motion.FrameAnchored {
		length := integerDistance(position, spawn.Motion.FrameAnchor)
		if length == 0 {
			direction = Direction{}
		} else {
			direction = normalizedDirection(position, spawn.Motion.FrameAnchor, length)
		}
	}
	signals, err = runtime.applySpawnMotionStep(cast, spawn, SteeringMotionStep{Direction: direction})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Direction = direction

	next := position
	if spawn.Motion.Stage != MotionStagePaused {
		next, err = runtime.advanceMotionTrajectory(cast, spawn, motion.trajectory, direction)
		if err != nil {
			return nil, err
		}
		if spawn.Motion.Stage == MotionStageReturning && spawn.Motion.FrameAnchored {
			before := motionDistance(position, spawn.Motion.FrameAnchor)
			after := motionDistance(next, spawn.Motion.FrameAnchor)
			if after >= before {
				next = spawn.Motion.FrameAnchor
			}
		}
	}
	signals, err = runtime.applySpawnMotionStep(cast, spawn, TrajectoryMotionStep{From: position, Position: next, Direction: direction})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position, spawn.Motion.TrajectoryPosition = next, next

	for _, offset := range motion.offsets {
		if spawn.Motion.Stage == MotionStagePaused {
			break
		}
		switch typed := offset.(type) {
		case zigzagMotionOffsetProgram:
			amplitude, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantZigzag, field: spawnPropertyFieldAmplitude}, typed.amplitude)
			if evalErr != nil {
				return nil, evalErr
			}
			if typed.periodTicks > 0 && (spawn.Motion.Tick/typed.periodTicks)%2 == 0 {
				next.Y = saturatingInt64Add(next.Y, amplitude)
			} else {
				next.Y = saturatingInt64Sub(next.Y, amplitude)
			}
		case circularMotionOffsetProgram:
			radius, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantCircular, field: spawnPropertyFieldRadius}, typed.radius)
			if evalErr != nil {
				return nil, evalErr
			}
			angularSpeed, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantCircular, field: spawnPropertyFieldAngularSpeed}, typed.angularSpeed)
			if evalErr != nil {
				return nil, evalErr
			}
			offset := motionPolarOffset(radius, angularSpeed, spawn.Motion.Tick)
			next.X = saturatingInt64Add(next.X, offset.X)
			next.Y = saturatingInt64Add(next.Y, offset.Y)
		}
	}
	signals, err = runtime.applySpawnMotionStep(cast, spawn, OffsetsMotionStep{Position: next})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position = next

	collisionTerminated := false
	if motion.collision != nil && spawn.Motion.Stage != MotionStagePaused {
		signals, err = runtime.applySpawnMotionStep(cast, spawn, CollisionMotionStep{From: position, Position: next, Layers: append([]CollisionLayerHandle(nil), motion.collision.layers...)})
		if err != nil {
			return nil, err
		}
		stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
		if hasSpawnSignal(signals, SpawnSignalCollision) {
			switch motion.collision.response {
			case motionCollisionStop:
				collisionTerminated = true
			case motionCollisionReflect:
				if spawn.Motion.ReflectCount > 0 {
					spawn.Motion.ReflectCount--
					direction.X = saturatingInt64Sub(0, direction.X)
					direction.Y = saturatingInt64Sub(0, direction.Y)
					spawn.Motion.Direction = direction
					stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
				}
				collisionTerminated = spawn.Motion.ReflectCount == 0
			case motionCollisionPierce:
				if spawn.Motion.PierceCount > 0 {
					spawn.Motion.PierceCount--
					stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
				}
				collisionTerminated = spawn.Motion.PierceCount == 0
			}
		}
	}
	if motion.carry != nil {
		target, evalErr := runtime.evalEntity(cast, motion.carry.target)
		if evalErr != nil {
			return nil, evalErr
		}
		signals, err = runtime.applySpawnMotionStep(cast, spawn, CarryMotionStep{Target: target, Position: next, Attached: true})
		if err != nil {
			return nil, err
		}
		carryTargetLost := hasTargetLostSignal(signals, target)
		stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
		if carryTargetLost {
			spawn.Motion.CarryTarget, spawn.Motion.CarryAttached = 0, false
		} else {
			spawn.Motion.CarryTarget, spawn.Motion.CarryAttached = target, true
		}
	}
	complete := collisionTerminated
	if !complete {
		switch spawn.Motion.Stage {
		case MotionStageOutbound:
			if runtime.currentTick >= spawn.EndTick && spawn.EndTick != 0 {
				switch completion := motion.completion.(type) {
				case endMotionCompletionProgram:
					complete = true
				case pauseThenEndMotionCompletionProgram:
					spawn.Motion.Stage = MotionStagePaused
					spawn.Motion.PauseCount = completion.pauseTicks
					spawn.EndTick = saturatingTickAdd(runtime.currentTick, completion.pauseTicks)
					stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
				case boomerangMotionCompletionProgram:
					spawn.Motion.Stage = MotionStagePaused
					spawn.Motion.PauseCount = 1
					spawn.EndTick = saturatingTickAdd(runtime.currentTick, saturatingTickAdd(1, completion.maxReturnTicks))
					stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
				default:
					return nil, ErrProgramInvariant
				}
			}
		case MotionStagePaused:
			switch motion.completion.(type) {
			case pauseThenEndMotionCompletionProgram:
				if spawn.Motion.PauseCount > 0 {
					spawn.Motion.PauseCount--
				}
				complete = spawn.Motion.PauseCount == 0
			case boomerangMotionCompletionProgram:
				value, readErr := runtime.readEntityPosition(cast, spawn.Owner)
				if readErr != nil {
					if !errors.Is(readErr, ErrEntityNotFound) {
						return nil, readErr
					}
					stageSignals = appendMotionStageSignals(spawn, stageSignals, []SpawnSignal{{Kind: SpawnSignalTargetLost, Target: spawn.Owner}})
					complete = true
					break
				}
				anchor, valid := value.Position()
				if !valid {
					return nil, ErrRuntimeTypeMismatch
				}
				spawn.Motion.FrameAnchor = anchor
				spawn.Motion.FrameAnchored = true
				spawn.Motion.PauseCount = 0
				spawn.Motion.ReturnCount = 0
				spawn.Motion.Stage = MotionStageReturning
				stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
			default:
				return nil, ErrProgramInvariant
			}
		case MotionStageReturning:
			completion, ok := motion.completion.(boomerangMotionCompletionProgram)
			if !ok {
				return nil, ErrProgramInvariant
			}
			spawn.Motion.ReturnCount++
			complete = spawn.Motion.Position == spawn.Motion.FrameAnchor || spawn.Motion.ReturnCount >= completion.maxReturnTicks
		}
	}
	if complete {
		if err := runtime.detachMotionCarry(cast, spawn); err != nil {
			return nil, err
		}
	}
	signals, err = runtime.applySpawnMotionStep(cast, spawn, CompletionMotionStep{Complete: complete})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	if complete {
		spawn.Motion.Stage = MotionStageCompleted
		stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalEnd})
	}
	stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTick})
	signals, err = runtime.applySpawnMotionStep(cast, spawn, SignalsMotionStep{Signals: normalizeSpawnSignals(stageSignals)})
	if err != nil {
		return nil, err
	}
	spawn.Motion.Tick++
	return signals, nil
}

func hasSpawnSignal(signals []SpawnSignal, kind SpawnSignalKind) bool {
	for _, signal := range signals {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

// motionDistance is the Euclidean distance used by every motion-stage
// comparison, matching the input-validation metric so one world has one
// distance semantic.
func motionDistance(left, right Position) int64 {
	return integerDistance(left, right)
}

func appendMotionStageSignals(spawn *SpawnInstance, aggregate, signals []SpawnSignal) []SpawnSignal {
	for _, signal := range signals {
		if signal.Kind == SpawnSignalTargetLost {
			if spawn.Motion.TargetLostEmitted {
				continue
			}
			spawn.Motion.TargetLostEmitted = true
		}
		aggregate = append(aggregate, signal)
	}
	return aggregate
}

func hasTargetLostSignal(signals []SpawnSignal, target EntityID) bool {
	for _, signal := range signals {
		if signal.Kind == SpawnSignalTargetLost && (signal.Target == 0 || signal.Target == target) {
			return true
		}
	}
	return false
}

func (runtime *Runtime) advanceMotionTrajectory(cast *castInstance, spawn *SpawnInstance, trajectory motionTrajectoryProgram, direction Direction) (Position, error) {
	position := spawn.Motion.TrajectoryPosition
	switch typed := trajectory.(type) {
	case stationaryMotionTrajectoryProgram:
		return position, nil
	case linearMotionTrajectoryProgram:
		speed, err := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotTrajectory, variant: spawnPropertyVariantLinear, field: spawnPropertyFieldSpeed}, typed.speed)
		if err != nil {
			return Position{}, err
		}
		return Position{X: saturatingInt64Add(position.X, scaleRatioRounded(direction.X, speed, normalizedDirectionScale)), Y: saturatingInt64Add(position.Y, scaleRatioRounded(direction.Y, speed, normalizedDirectionScale))}, nil
	case pathMotionTrajectoryProgram:
		value, err := runtime.evalValue(cast, typed.points)
		if err != nil {
			return Position{}, err
		}
		points, valid := value.Path()
		if !valid || len(points) == 0 {
			return Position{}, ErrRuntimeTypeMismatch
		}
		speed, err := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotTrajectory, variant: spawnPropertyVariantPath, field: spawnPropertyFieldSpeed}, typed.speed)
		if err != nil {
			return Position{}, err
		}
		return advanceMotionPath(position, points, speed, &spawn.Motion.TrajectoryIndex), nil
	case orbitMotionTrajectoryProgram:
		anchor, err := runtime.evalEntity(cast, typed.anchor)
		if err != nil {
			return Position{}, err
		}
		value, err := runtime.readEntityPosition(cast, anchor)
		if err != nil {
			return Position{}, err
		}
		anchorPosition, valid := value.Position()
		if !valid {
			return Position{}, ErrRuntimeTypeMismatch
		}
		radius, err := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotTrajectory, variant: spawnPropertyVariantOrbit, field: spawnPropertyFieldRadius}, typed.radius)
		if err != nil {
			return Position{}, err
		}
		angularSpeed, err := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotTrajectory, variant: spawnPropertyVariantOrbit, field: spawnPropertyFieldAngularSpeed}, typed.angularSpeed)
		if err != nil {
			return Position{}, err
		}
		offset := motionPolarOffset(radius, angularSpeed, spawn.Motion.Tick)
		return Position{X: saturatingInt64Add(anchorPosition.X, offset.X), Y: saturatingInt64Add(anchorPosition.Y, offset.Y)}, nil
	case parabolaMotionTrajectoryProgram:
		destination, err := runtime.evalPosition(cast, typed.destination)
		if err != nil {
			return Position{}, err
		}
		if typed.durationTicks <= 0 {
			return destination, nil
		}
		height, err := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotTrajectory, variant: spawnPropertyVariantParabola, field: spawnPropertyFieldHeight}, typed.height)
		if err != nil {
			return Position{}, err
		}
		duration := int64(typed.durationTicks)
		elapsed := minInt64(int64(spawn.Motion.Tick)+1, duration)
		lineX := saturatingInt64Add(spawn.Motion.Origin.X, scaleRatioRounded(saturatingInt64Sub(destination.X, spawn.Motion.Origin.X), elapsed, duration))
		lineY := saturatingInt64Add(spawn.Motion.Origin.Y, scaleRatioRounded(saturatingInt64Sub(destination.Y, spawn.Motion.Origin.Y), elapsed, duration))
		arcNumerator := saturatingInt64Mul(4, saturatingInt64Mul(elapsed, duration-elapsed))
		arc := scaleRatioRounded(height, arcNumerator, saturatingInt64Mul(duration, duration))
		return Position{X: lineX, Y: saturatingInt64Add(lineY, arc)}, nil
	default:
		return Position{}, ErrProgramInvariant
	}
}

func (runtime *Runtime) resolveSpawnNumeric(cast *castInstance, spawn *SpawnInstance, binding spawnPropertySlotBindingProgram, fallback programValue) (int64, error) {
	if value, found := spawnNumericBoundValue(spawn, binding); found {
		return value, nil
	}
	return runtime.evalInt(cast, fallback)
}

func advanceMotionPath(position Position, points []Position, speed int64, index *int) Position {
	remaining := speed
	if remaining <= 0 || len(points) == 0 {
		return position
	}
	for *index < len(points) {
		target := points[*index]
		deltaX, deltaY := saturatingInt64Sub(target.X, position.X), saturatingInt64Sub(target.Y, position.Y)
		distance := integerDistance(position, target)
		if distance == 0 {
			(*index)++
			continue
		}
		if remaining >= distance {
			position = target
			remaining -= distance
			(*index)++
			continue
		}
		position.X = saturatingInt64Add(position.X, scaleRatioRounded(deltaX, remaining, distance))
		position.Y = saturatingInt64Add(position.Y, scaleRatioRounded(deltaY, remaining, distance))
		return position
	}
	return position
}

func motionPolarOffset(radius, angularSpeed int64, tick Tick) Position {
	angle := motionAngleMDeg(angularSpeed, tick)
	x, y := motionSinCos(angle)
	return Position{X: scaleRatioRounded(x, radius, motionTrigScale), Y: scaleRatioRounded(y, radius, motionTrigScale)}
}

const motionTrigScale int64 = 1_000_000

func motionAngleMDeg(angularSpeed int64, tick Tick) int64 {
	const fullTurn int64 = 360_000
	speed := angularSpeed % fullTurn
	elapsed := (int64(tick) + 1) % fullTurn
	return (speed * elapsed) % fullTurn
}

// motionSinCos uses a fixed-point CORDIC rotation. Its inputs and outputs are
// integers, including the millidegree angle and the one-million-unit vector.
func motionSinCos(angle int64) (int64, int64) {
	const fullTurn int64 = 360_000
	angle %= fullTurn
	if angle < 0 {
		angle += fullTurn
	}
	switch angle {
	case 0:
		return motionTrigScale, 0
	case 90_000:
		return 0, motionTrigScale
	case 180_000:
		return -motionTrigScale, 0
	case 270_000:
		return 0, -motionTrigScale
	}
	if angle > 180_000 {
		angle -= fullTurn
	}
	sign := int64(1)
	if angle > 90_000 {
		angle -= 180_000
		sign = -1
	} else if angle < -90_000 {
		angle += 180_000
		sign = -1
	}
	x, y, z := int64(607_253), int64(0), angle
	angles := [...]int64{45_000, 26_565, 14_036, 7_125, 3_576, 1_790, 895, 448, 224, 112, 56, 28, 14, 7, 3, 2, 1}
	for shift, rotation := range angles {
		oldX := x
		if z >= 0 {
			x -= y >> shift
			y += oldX >> shift
			z -= rotation
		} else {
			x += y >> shift
			y -= oldX >> shift
			z += rotation
		}
	}
	return x * sign, y * sign
}

func (runtime *Runtime) applySpawnMotionStep(cast *castInstance, spawn *SpawnInstance, step MotionStep) ([]SpawnSignal, error) {
	result, err := runtime.host.StepSpawn(SpawnStepCommand{Meta: SpawnCommandMeta{RequiredRevision: cast.visibleRevision, SpawnID: spawn.ID}, Motion: step, Numeric: snapshotSpawnNumeric(spawn)}, spawn.HostState)
	if err != nil {
		return nil, err
	}
	spawn.HostState = result.State
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
	spawn.visibleRevision = cast.visibleRevision
	if err := runtime.drainHostEvents(cast); err != nil {
		return nil, err
	}
	return result.Signals, nil
}
