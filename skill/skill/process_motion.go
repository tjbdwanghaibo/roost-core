package skill

import "errors"

// stepProcessMotion is the only Runtime path that turns a lowered motion
// program into Host work. Stages are deliberately written in canonical order;
// a Host only receives resolved integer facts for one stage at a time.
func (runtime *Runtime) stepProcessMotion(cast *castInstance, process *ProcessInstance) ([]ProcessSignal, error) {
	if cast == nil || process == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return nil, ErrProgramInvariant
	}
	if err := runtime.initializeProcessNumeric(cast, process); err != nil {
		return nil, err
	}
	_, err := advanceProcessNumeric(process, runtime.currentTick)
	if err != nil {
		return nil, err
	}
	template := process.Program.processTemplates[process.TemplateIndex]
	if template.motion == nil {
		initialized := process.Motion.Initialized
		process.Motion.Initialized = true
		signals, err := runtime.applyProcessMotionStep(cast, process, StaticMotionStep{Position: process.Motion.Position})
		if err != nil {
			return nil, err
		}
		if !initialized {
			return signals, nil
		}
		signals = appendMotionStageSignals(process, nil, signals)
		signals = append(signals, ProcessSignal{Kind: ProcessSignalTick})
		result, err := runtime.applyProcessMotionStep(cast, process, SignalsMotionStep{Signals: normalizeProcessSignals(signals)})
		if err == nil {
			process.Motion.Tick++
		}
		return result, err
	}
	motion := template.motion
	stageSignals := make([]ProcessSignal, 0)
	if !process.Motion.Initialized {
		process.Motion.Initialized = true
		process.Motion.Stage = MotionStageOutbound
		process.Motion.Position = process.HostState.Position
		process.Motion.TrajectoryPosition = process.HostState.Position
		process.Motion.Origin = process.HostState.Position
		if motion.collision != nil {
			process.Motion.ReflectCount = motion.collision.maxReflects
			process.Motion.PierceCount = motion.collision.maxPierces
		}
	}
	if process.Motion.Stage == MotionStageCompleted {
		return nil, nil
	}

	position := process.Motion.TrajectoryPosition
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
	signals, err := runtime.applyProcessMotionStep(cast, process, FrameMotionStep{Position: position})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(process, stageSignals, signals)
	process.Motion.Position, process.Motion.TrajectoryPosition = position, position

	direction := process.Motion.Direction
	if _, fixed := motion.steering.(fixedMotionSteeringProgram); fixed && direction == (Direction{}) {
		direction = Direction{X: normalizedDirectionScale}
	}
	if tracking, ok := motion.steering.(trackingMotionSteeringProgram); ok && process.Motion.Tick < tracking.durationTicks {
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
	if process.Motion.Stage == MotionStageReturning && process.Motion.FrameAnchored {
		length := integerDistance(position, process.Motion.FrameAnchor)
		if length == 0 {
			direction = Direction{}
		} else {
			direction = normalizedDirection(position, process.Motion.FrameAnchor, length)
		}
	}
	signals, err = runtime.applyProcessMotionStep(cast, process, SteeringMotionStep{Direction: direction})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(process, stageSignals, signals)
	process.Motion.Direction = direction

	next := position
	if process.Motion.Stage != MotionStagePaused {
		next, err = runtime.advanceMotionTrajectory(cast, process, motion.trajectory, direction)
		if err != nil {
			return nil, err
		}
		if process.Motion.Stage == MotionStageReturning && process.Motion.FrameAnchored {
			before := motionDistance(position, process.Motion.FrameAnchor)
			after := motionDistance(next, process.Motion.FrameAnchor)
			if after >= before {
				next = process.Motion.FrameAnchor
			}
		}
	}
	signals, err = runtime.applyProcessMotionStep(cast, process, TrajectoryMotionStep{From: position, Position: next, Direction: direction})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(process, stageSignals, signals)
	process.Motion.Position, process.Motion.TrajectoryPosition = next, next

	for _, offset := range motion.offsets {
		if process.Motion.Stage == MotionStagePaused {
			break
		}
		switch typed := offset.(type) {
		case zigzagMotionOffsetProgram:
			amplitude, evalErr := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotOffset, variant: processPropertyVariantZigzag, field: processPropertyFieldAmplitude}, typed.amplitude)
			if evalErr != nil {
				return nil, evalErr
			}
			if typed.periodTicks > 0 && (process.Motion.Tick/typed.periodTicks)%2 == 0 {
				next.Y = saturatingInt64Add(next.Y, amplitude)
			} else {
				next.Y = saturatingInt64Sub(next.Y, amplitude)
			}
		case circularMotionOffsetProgram:
			radius, evalErr := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotOffset, variant: processPropertyVariantCircular, field: processPropertyFieldRadius}, typed.radius)
			if evalErr != nil {
				return nil, evalErr
			}
			angularSpeed, evalErr := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotOffset, variant: processPropertyVariantCircular, field: processPropertyFieldAngularSpeed}, typed.angularSpeed)
			if evalErr != nil {
				return nil, evalErr
			}
			offset := motionPolarOffset(radius, angularSpeed, process.Motion.Tick)
			next.X = saturatingInt64Add(next.X, offset.X)
			next.Y = saturatingInt64Add(next.Y, offset.Y)
		}
	}
	signals, err = runtime.applyProcessMotionStep(cast, process, OffsetsMotionStep{Position: next})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(process, stageSignals, signals)
	process.Motion.Position = next

	collisionTerminated := false
	if motion.collision != nil && process.Motion.Stage != MotionStagePaused {
		signals, err = runtime.applyProcessMotionStep(cast, process, CollisionMotionStep{From: position, Position: next, Layers: append([]CollisionLayerHandle(nil), motion.collision.layers...)})
		if err != nil {
			return nil, err
		}
		stageSignals = appendMotionStageSignals(process, stageSignals, signals)
		if hasProcessSignal(signals, ProcessSignalCollision) {
			switch motion.collision.response {
			case motionCollisionStop:
				collisionTerminated = true
			case motionCollisionReflect:
				if process.Motion.ReflectCount > 0 {
					process.Motion.ReflectCount--
					direction.X = saturatingInt64Sub(0, direction.X)
					direction.Y = saturatingInt64Sub(0, direction.Y)
					process.Motion.Direction = direction
					stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTransition})
				}
				collisionTerminated = process.Motion.ReflectCount == 0
			case motionCollisionPierce:
				if process.Motion.PierceCount > 0 {
					process.Motion.PierceCount--
					stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTransition})
				}
				collisionTerminated = process.Motion.PierceCount == 0
			}
		}
	}
	if motion.carry != nil {
		target, evalErr := runtime.evalEntity(cast, motion.carry.target)
		if evalErr != nil {
			return nil, evalErr
		}
		signals, err = runtime.applyProcessMotionStep(cast, process, CarryMotionStep{Target: target, Position: next, Attached: true})
		if err != nil {
			return nil, err
		}
		carryTargetLost := hasTargetLostSignal(signals, target)
		stageSignals = appendMotionStageSignals(process, stageSignals, signals)
		if carryTargetLost {
			process.Motion.CarryTarget, process.Motion.CarryAttached = 0, false
		} else {
			process.Motion.CarryTarget, process.Motion.CarryAttached = target, true
		}
	}
	complete := collisionTerminated
	if !complete {
		switch process.Motion.Stage {
		case MotionStageOutbound:
			if runtime.currentTick >= process.EndTick && process.EndTick != 0 {
				switch completion := motion.completion.(type) {
				case endMotionCompletionProgram:
					complete = true
				case pauseThenEndMotionCompletionProgram:
					process.Motion.Stage = MotionStagePaused
					process.Motion.PauseCount = completion.pauseTicks
					process.EndTick = saturatingTickAdd(runtime.currentTick, completion.pauseTicks)
					stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTransition})
				case boomerangMotionCompletionProgram:
					process.Motion.Stage = MotionStagePaused
					process.Motion.PauseCount = 1
					process.EndTick = saturatingTickAdd(runtime.currentTick, saturatingTickAdd(1, completion.maxReturnTicks))
					stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTransition})
				default:
					return nil, ErrProgramInvariant
				}
			}
		case MotionStagePaused:
			switch motion.completion.(type) {
			case pauseThenEndMotionCompletionProgram:
				if process.Motion.PauseCount > 0 {
					process.Motion.PauseCount--
				}
				complete = process.Motion.PauseCount == 0
			case boomerangMotionCompletionProgram:
				value, readErr := runtime.readEntityPosition(cast, process.Owner)
				if readErr != nil {
					if !errors.Is(readErr, ErrEntityNotFound) {
						return nil, readErr
					}
					stageSignals = appendMotionStageSignals(process, stageSignals, []ProcessSignal{{Kind: ProcessSignalTargetLost, Target: process.Owner}})
					complete = true
					break
				}
				anchor, valid := value.Position()
				if !valid {
					return nil, ErrRuntimeTypeMismatch
				}
				process.Motion.FrameAnchor = anchor
				process.Motion.FrameAnchored = true
				process.Motion.PauseCount = 0
				process.Motion.ReturnCount = 0
				process.Motion.Stage = MotionStageReturning
				stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTransition})
			default:
				return nil, ErrProgramInvariant
			}
		case MotionStageReturning:
			completion, ok := motion.completion.(boomerangMotionCompletionProgram)
			if !ok {
				return nil, ErrProgramInvariant
			}
			process.Motion.ReturnCount++
			complete = process.Motion.Position == process.Motion.FrameAnchor || process.Motion.ReturnCount >= completion.maxReturnTicks
		}
	}
	if complete {
		if err := runtime.detachMotionCarry(cast, process); err != nil {
			return nil, err
		}
	}
	signals, err = runtime.applyProcessMotionStep(cast, process, CompletionMotionStep{Complete: complete})
	if err != nil {
		return nil, err
	}
	stageSignals = appendMotionStageSignals(process, stageSignals, signals)
	if complete {
		process.Motion.Stage = MotionStageCompleted
		stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalEnd})
	}
	stageSignals = append(stageSignals, ProcessSignal{Kind: ProcessSignalTick})
	signals, err = runtime.applyProcessMotionStep(cast, process, SignalsMotionStep{Signals: normalizeProcessSignals(stageSignals)})
	if err != nil {
		return nil, err
	}
	process.Motion.Tick++
	return signals, nil
}

func hasProcessSignal(signals []ProcessSignal, kind ProcessSignalKind) bool {
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

func appendMotionStageSignals(process *ProcessInstance, aggregate, signals []ProcessSignal) []ProcessSignal {
	for _, signal := range signals {
		if signal.Kind == ProcessSignalTargetLost {
			if process.Motion.TargetLostEmitted {
				continue
			}
			process.Motion.TargetLostEmitted = true
		}
		aggregate = append(aggregate, signal)
	}
	return aggregate
}

func hasTargetLostSignal(signals []ProcessSignal, target EntityID) bool {
	for _, signal := range signals {
		if signal.Kind == ProcessSignalTargetLost && (signal.Target == 0 || signal.Target == target) {
			return true
		}
	}
	return false
}

func (runtime *Runtime) advanceMotionTrajectory(cast *castInstance, process *ProcessInstance, trajectory motionTrajectoryProgram, direction Direction) (Position, error) {
	position := process.Motion.TrajectoryPosition
	switch typed := trajectory.(type) {
	case stationaryMotionTrajectoryProgram:
		return position, nil
	case linearMotionTrajectoryProgram:
		speed, err := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotTrajectory, variant: processPropertyVariantLinear, field: processPropertyFieldSpeed}, typed.speed)
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
		speed, err := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotTrajectory, variant: processPropertyVariantPath, field: processPropertyFieldSpeed}, typed.speed)
		if err != nil {
			return Position{}, err
		}
		return advanceMotionPath(position, points, speed, &process.Motion.TrajectoryIndex), nil
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
		radius, err := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotTrajectory, variant: processPropertyVariantOrbit, field: processPropertyFieldRadius}, typed.radius)
		if err != nil {
			return Position{}, err
		}
		angularSpeed, err := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotTrajectory, variant: processPropertyVariantOrbit, field: processPropertyFieldAngularSpeed}, typed.angularSpeed)
		if err != nil {
			return Position{}, err
		}
		offset := motionPolarOffset(radius, angularSpeed, process.Motion.Tick)
		return Position{X: saturatingInt64Add(anchorPosition.X, offset.X), Y: saturatingInt64Add(anchorPosition.Y, offset.Y)}, nil
	case parabolaMotionTrajectoryProgram:
		destination, err := runtime.evalPosition(cast, typed.destination)
		if err != nil {
			return Position{}, err
		}
		if typed.durationTicks <= 0 {
			return destination, nil
		}
		height, err := runtime.resolveProcessNumeric(cast, process, processPropertySlotBindingProgram{stage: processPropertySlotTrajectory, variant: processPropertyVariantParabola, field: processPropertyFieldHeight}, typed.height)
		if err != nil {
			return Position{}, err
		}
		duration := int64(typed.durationTicks)
		elapsed := minInt64(int64(process.Motion.Tick)+1, duration)
		lineX := saturatingInt64Add(process.Motion.Origin.X, scaleRatioRounded(saturatingInt64Sub(destination.X, process.Motion.Origin.X), elapsed, duration))
		lineY := saturatingInt64Add(process.Motion.Origin.Y, scaleRatioRounded(saturatingInt64Sub(destination.Y, process.Motion.Origin.Y), elapsed, duration))
		arcNumerator := saturatingInt64Mul(4, saturatingInt64Mul(elapsed, duration-elapsed))
		arc := scaleRatioRounded(height, arcNumerator, saturatingInt64Mul(duration, duration))
		return Position{X: lineX, Y: saturatingInt64Add(lineY, arc)}, nil
	default:
		return Position{}, ErrProgramInvariant
	}
}

func (runtime *Runtime) resolveProcessNumeric(cast *castInstance, process *ProcessInstance, binding processPropertySlotBindingProgram, fallback programValue) (int64, error) {
	if value, found := processNumericBoundValue(process, binding); found {
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

func (runtime *Runtime) applyProcessMotionStep(cast *castInstance, process *ProcessInstance, step MotionStep) ([]ProcessSignal, error) {
	result, err := runtime.host.StepProcess(ProcessStepCommand{Meta: ProcessCommandMeta{RequiredRevision: cast.visibleRevision, ProcessID: process.ID}, Motion: step, Numeric: snapshotProcessNumeric(process)}, process.HostState)
	if err != nil {
		return nil, err
	}
	process.HostState = result.State
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
	process.visibleRevision = cast.visibleRevision
	if err := runtime.drainHostEvents(cast); err != nil {
		return nil, err
	}
	return result.Signals, nil
}
