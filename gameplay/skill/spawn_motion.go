package skill

import "errors"

// stepSpawnMotion 是 Runtime 把 lower 后的运动程序变成 Host 工作的唯一路径。各阶段刻意按规范顺序写出；Host 每次
// 只收到一个阶段已解析的整数事实。
//
// 阶段顺序：frame → steering → trajectory → offsets → collision → carry → completion → signals。每个阶段把 Host 回报的
// 信号并进 stageSignals，最后一次性经 signals 阶段交给 Host；任一阶段失败立即返回，本步不再推进 tick。
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
		return runtime.stepStaticSpawn(cast, spawn)
	}
	motion := template.motion
	stageSignals := make([]SpawnSignal, 0)
	if !spawn.Motion.Initialized {
		initializeSpawnMotion(spawn, motion)
	}
	if spawn.Motion.Stage == MotionStageCompleted {
		return nil, nil
	}

	position, stageSignals, err := runtime.stepMotionFrame(cast, spawn, motion, stageSignals)
	if err != nil {
		return nil, err
	}
	direction, stageSignals, err := runtime.stepMotionSteering(cast, spawn, motion, position, stageSignals)
	if err != nil {
		return nil, err
	}
	next, stageSignals, err := runtime.stepMotionTrajectory(cast, spawn, motion, position, direction, stageSignals)
	if err != nil {
		return nil, err
	}
	next, stageSignals, err = runtime.stepMotionOffsets(cast, spawn, motion, next, stageSignals)
	if err != nil {
		return nil, err
	}
	collisionTerminated, stageSignals, err := runtime.stepMotionCollision(cast, spawn, motion, position, next, direction, stageSignals)
	if err != nil {
		return nil, err
	}
	if motion.carry != nil {
		stageSignals, err = runtime.stepMotionCarry(cast, spawn, motion, next, stageSignals)
		if err != nil {
			return nil, err
		}
	}
	complete := collisionTerminated
	if !complete {
		complete, stageSignals, err = runtime.motionStageComplete(cast, spawn, motion, stageSignals)
		if err != nil {
			return nil, err
		}
	}
	return runtime.finishMotionStep(cast, spawn, complete, stageSignals)
}

// stepStaticSpawn 推进没有运动程序的衍生物：第一步只发静态位置；之后每步发静态位置，再把阶段信号连同 tick
// 信号经 signals 阶段交给 Host，成功后推进 tick。
func (runtime *Runtime) stepStaticSpawn(cast *castInstance, spawn *SpawnInstance) ([]SpawnSignal, error) {
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

// initializeSpawnMotion 在第一步前建立运动状态：进入外出阶段，位置、轨迹基准与起点取 Host 的当前位置，
// 并装填碰撞的反弹 / 穿透次数。
func initializeSpawnMotion(spawn *SpawnInstance, motion *motionProgram) {
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

// stepMotionFrame 是 frame 阶段：world 参照系沿用轨迹基准位置，follow 参照系取跟随目标的当前位置。
func (runtime *Runtime) stepMotionFrame(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, stageSignals []SpawnSignal) (Position, []SpawnSignal, error) {
	position := spawn.Motion.TrajectoryPosition
	if follow, ok := motion.frame.(followMotionFrameProgram); ok {
		target, err := runtime.evalEntity(cast, follow.target)
		if err != nil {
			return Position{}, nil, err
		}
		value, err := runtime.readEntityPosition(cast, target)
		if err != nil {
			return Position{}, nil, err
		}
		var valid bool
		position, valid = value.Position()
		if !valid {
			return Position{}, nil, ErrRuntimeTypeMismatch
		}
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, FrameMotionStep{Position: position})
	if err != nil {
		return Position{}, nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position, spawn.Motion.TrajectoryPosition = position, position
	return position, stageSignals, nil
}

// stepMotionSteering 是 steering 阶段：fixed 未设方向时朝 +X；tracking 在持续时间内朝目标；返程阶段朝锚点。
func (runtime *Runtime) stepMotionSteering(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, position Position, stageSignals []SpawnSignal) (Direction, []SpawnSignal, error) {
	direction := spawn.Motion.Direction
	if _, fixed := motion.steering.(fixedMotionSteeringProgram); fixed && direction == (Direction{}) {
		direction = Direction{X: normalizedDirectionScale}
	}
	if tracking, ok := motion.steering.(trackingMotionSteeringProgram); ok && spawn.Motion.Tick < tracking.durationTicks {
		target, err := runtime.evalEntity(cast, tracking.target)
		if err != nil {
			return Direction{}, nil, err
		}
		value, err := runtime.readEntityPosition(cast, target)
		if err != nil {
			return Direction{}, nil, err
		}
		targetPosition, valid := value.Position()
		if !valid {
			return Direction{}, nil, ErrRuntimeTypeMismatch
		}
		// 欧氏长度：归一化方向的欧氏范数等于方向刻度，斜向追踪与轴向追踪速度相同（用切比雪夫长度会让斜向投射物
		// 快到 41%）。
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
	signals, err := runtime.applySpawnMotionStep(cast, spawn, SteeringMotionStep{Direction: direction})
	if err != nil {
		return Direction{}, nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Direction = direction
	return direction, stageSignals, nil
}

// stepMotionTrajectory 是 trajectory 阶段：暂停时原地不动；返程阶段不越过锚点（距离不再缩短就落在锚点上）。
func (runtime *Runtime) stepMotionTrajectory(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, position Position, direction Direction, stageSignals []SpawnSignal) (Position, []SpawnSignal, error) {
	next := position
	if spawn.Motion.Stage != MotionStagePaused {
		var err error
		next, err = runtime.advanceMotionTrajectory(cast, spawn, motion.trajectory, direction)
		if err != nil {
			return Position{}, nil, err
		}
		if spawn.Motion.Stage == MotionStageReturning && spawn.Motion.FrameAnchored {
			before := motionDistance(position, spawn.Motion.FrameAnchor)
			after := motionDistance(next, spawn.Motion.FrameAnchor)
			if after >= before {
				next = spawn.Motion.FrameAnchor
			}
		}
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, TrajectoryMotionStep{From: position, Position: next, Direction: direction})
	if err != nil {
		return Position{}, nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position, spawn.Motion.TrajectoryPosition = next, next
	return next, stageSignals, nil
}

// stepMotionOffsets 是 offsets 阶段：在轨迹位置上依次叠加 zigzag / circular 偏移（暂停时不叠加），轨迹基准不变。
func (runtime *Runtime) stepMotionOffsets(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, next Position, stageSignals []SpawnSignal) (Position, []SpawnSignal, error) {
	for _, offset := range motion.offsets {
		if spawn.Motion.Stage == MotionStagePaused {
			break
		}
		switch typed := offset.(type) {
		case zigzagMotionOffsetProgram:
			amplitude, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantZigzag, field: spawnPropertyFieldAmplitude}, typed.amplitude)
			if evalErr != nil {
				return Position{}, nil, evalErr
			}
			if typed.periodTicks > 0 && (spawn.Motion.Tick/typed.periodTicks)%2 == 0 {
				next.Y = saturatingInt64Add(next.Y, amplitude)
			} else {
				next.Y = saturatingInt64Sub(next.Y, amplitude)
			}
		case circularMotionOffsetProgram:
			radius, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantCircular, field: spawnPropertyFieldRadius}, typed.radius)
			if evalErr != nil {
				return Position{}, nil, evalErr
			}
			angularSpeed, evalErr := runtime.resolveSpawnNumeric(cast, spawn, spawnPropertySlotBindingProgram{stage: spawnPropertySlotOffset, variant: spawnPropertyVariantCircular, field: spawnPropertyFieldAngularSpeed}, typed.angularSpeed)
			if evalErr != nil {
				return Position{}, nil, evalErr
			}
			offset := motionPolarOffset(radius, angularSpeed, spawn.Motion.Tick)
			next.X = saturatingInt64Add(next.X, offset.X)
			next.Y = saturatingInt64Add(next.Y, offset.Y)
		}
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, OffsetsMotionStep{Position: next})
	if err != nil {
		return Position{}, nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	spawn.Motion.Position = next
	return next, stageSignals, nil
}

// stepMotionCollision 是 collision 阶段（有碰撞程序且未暂停时）：发生碰撞时按 stop / reflect / pierce 处理，
// 返回本步是否因碰撞结束。reflect 反转方向并消耗一次反弹，pierce 消耗一次穿透，次数用完即结束。
func (runtime *Runtime) stepMotionCollision(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, position, next Position, direction Direction, stageSignals []SpawnSignal) (bool, []SpawnSignal, error) {
	if motion.collision == nil || spawn.Motion.Stage == MotionStagePaused {
		return false, stageSignals, nil
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, CollisionMotionStep{From: position, Position: next, Layers: append([]CollisionLayerHandle(nil), motion.collision.layers...)})
	if err != nil {
		return false, nil, err
	}
	stageSignals = appendMotionStageSignals(spawn, stageSignals, signals)
	collisionTerminated := false
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
	return collisionTerminated, stageSignals, nil
}

// stepMotionCarry 是 carry 阶段：把携带目标挂到本步位置；Host 报告目标丢失时解除携带记录。
func (runtime *Runtime) stepMotionCarry(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, next Position, stageSignals []SpawnSignal) ([]SpawnSignal, error) {
	target, evalErr := runtime.evalEntity(cast, motion.carry.target)
	if evalErr != nil {
		return nil, evalErr
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, CarryMotionStep{Target: target, Position: next, Attached: true})
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
	return stageSignals, nil
}

// motionStageComplete 按当前运动阶段与完成程序判断本步是否结束（未因碰撞结束时调用）：
//   - 外出阶段到期：end 直接结束；pause_then_end 进入暂停；boomerang 暂停一步后返程；
//   - 暂停阶段：pause_then_end 计满暂停步数结束；boomerang 取所有者位置作锚点进入返程（所有者已不存在则结束）；
//   - 返程阶段：到达锚点或用满返程步数结束。
func (runtime *Runtime) motionStageComplete(cast *castInstance, spawn *SpawnInstance, motion *motionProgram, stageSignals []SpawnSignal) (bool, []SpawnSignal, error) {
	complete := false
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
				return false, nil, ErrProgramInvariant
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
					return false, nil, readErr
				}
				stageSignals = appendMotionStageSignals(spawn, stageSignals, []SpawnSignal{{Kind: SpawnSignalTargetLost, Target: spawn.Owner}})
				complete = true
				break
			}
			anchor, valid := value.Position()
			if !valid {
				return false, nil, ErrRuntimeTypeMismatch
			}
			spawn.Motion.FrameAnchor = anchor
			spawn.Motion.FrameAnchored = true
			spawn.Motion.PauseCount = 0
			spawn.Motion.ReturnCount = 0
			spawn.Motion.Stage = MotionStageReturning
			stageSignals = append(stageSignals, SpawnSignal{Kind: SpawnSignalTransition})
		default:
			return false, nil, ErrProgramInvariant
		}
	case MotionStageReturning:
		completion, ok := motion.completion.(boomerangMotionCompletionProgram)
		if !ok {
			return false, nil, ErrProgramInvariant
		}
		spawn.Motion.ReturnCount++
		complete = spawn.Motion.Position == spawn.Motion.FrameAnchor || spawn.Motion.ReturnCount >= completion.maxReturnTicks
	}
	return complete, stageSignals, nil
}

// finishMotionStep 是 completion 与 signals 阶段：结束时先解除携带，再报告完成；结束的衍生物进入完成阶段并追加
// end 信号。最后追加 tick 信号、经 signals 阶段一次性交给 Host，成功后推进 tick。
func (runtime *Runtime) finishMotionStep(cast *castInstance, spawn *SpawnInstance, complete bool, stageSignals []SpawnSignal) ([]SpawnSignal, error) {
	if complete {
		if err := runtime.detachMotionCarry(cast, spawn); err != nil {
			return nil, err
		}
	}
	signals, err := runtime.applySpawnMotionStep(cast, spawn, CompletionMotionStep{Complete: complete})
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

// motionDistance 是每个运动阶段比较用的欧氏距离，与输入校验的度量一致，同一个世界只有一种距离语义。
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

// motionSinCos 用定点 CORDIC 旋转，输入输出都是整数（毫度角与百万单位向量）。
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
	runtime.drainHostEvents(cast)
	return result.Signals, nil
}
