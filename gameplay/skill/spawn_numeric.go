package skill

import "math"

type numericPropertyState struct {
	Property SpawnPropertyHandle
	Base     int64
	Current  int64
	Track    *numericTrackState
	Binding  spawnPropertySlotBindingProgram
	Bound    bool
}

type numericTrackState struct {
	Start     int64
	Target    int64
	StartTick Tick
	OverTicks Tick
}

func advanceSpawnNumeric(spawn *SpawnInstance, tick Tick) (SpawnNumericSnapshot, error) {
	if spawn == nil || spawn.Program == nil || !spawn.Numeric.Initialized {
		return SpawnNumericSnapshot{}, ErrProgramInvariant
	}
	for index := range spawn.Numeric.Properties {
		state := &spawn.Numeric.Properties[index]
		policy, found := lookupSpawnNumericProperty(spawn.Program, state.Property)
		if !found {
			return SpawnNumericSnapshot{}, ErrProgramInvariant
		}
		if state.Track != nil {
			current, complete, err := sampleSpawnNumericTrack(*state.Track, tick)
			if err != nil {
				return SpawnNumericSnapshot{}, err
			}
			state.Current = clampSpawnNumeric(current, policy.minimum, policy.maximum)
			if complete {
				state.Track = nil
			}
		}
	}
	return snapshotSpawnNumeric(spawn), nil
}

func replaceSpawnNumericTrack(spawn *SpawnInstance, property SpawnPropertyHandle, operation spawnNumericOperation, operand int64, overTicks, tick Tick) error {
	if spawn == nil || spawn.Program == nil || !spawn.Numeric.Initialized || overTicks < 0 {
		return ErrProgramInvariant
	}
	policy, found := lookupSpawnNumericProperty(spawn.Program, property)
	if !found || policy.interpolation != spawnNumericLinearInteger || policy.rounding != spawnNumericTruncateTowardZero {
		return ErrProgramInvariant
	}
	state := lookupSpawnNumericState(spawn, property)
	if state == nil {
		return ErrProgramInvariant
	}

	var target int64
	var ok bool
	switch operation {
	case spawnNumericSet:
		target = operand
	case spawnNumericAdd:
		target, ok = checkedSpawnNumericAdd(state.Current, operand)
		if !ok {
			return ErrRuntimeArithmeticOverflow
		}
	case spawnNumericMulBP:
		target, ok = checkedInt64Mul(state.Current, operand)
		if !ok {
			return ErrRuntimeArithmeticOverflow
		}
		target /= 10000
	default:
		return ErrProgramInvariant
	}
	target = clampSpawnNumeric(target, policy.minimum, policy.maximum)
	state.Track = &numericTrackState{Start: state.Current, Target: target, StartTick: tick, OverTicks: overTicks}
	bindSpawnNumericState(spawn, state, policy)
	return nil
}

func (runtime *Runtime) executeModifySpawn(cast *castInstance, operation modifySpawnOperation) error {
	spawnValue, err := runtime.evalValue(cast, operation.spawn)
	if err != nil {
		return err
	}
	if !spawnValue.Present() {
		return ErrRuntimeValueMissing
	}
	spawnID, ok := spawnValue.Spawn()
	if !ok {
		return ErrRuntimeTypeMismatch
	}
	spawn, err := activeCallbackSpawn(cast, spawnID)
	if err != nil {
		return err
	}
	value, err := runtime.evalInt(cast, operation.value)
	if err != nil {
		return err
	}
	return replaceSpawnNumericTrack(spawn, operation.property, operation.operation, value, operation.overTicks, runtime.currentTick)
}

func sampleSpawnNumericTrack(track numericTrackState, tick Tick) (int64, bool, error) {
	if track.OverTicks <= 0 || tick-track.StartTick >= track.OverTicks {
		return track.Target, true, nil
	}
	if tick <= track.StartTick {
		return track.Start, false, nil
	}
	delta, ok := checkedInt64Sub(track.Target, track.Start)
	if !ok {
		return 0, false, ErrRuntimeArithmeticOverflow
	}
	scaled, ok := checkedInt64Mul(delta, int64(tick-track.StartTick))
	if !ok {
		return 0, false, ErrRuntimeArithmeticOverflow
	}
	offset := scaled / int64(track.OverTicks)
	current, ok := checkedSpawnNumericAdd(track.Start, offset)
	if !ok {
		return 0, false, ErrRuntimeArithmeticOverflow
	}
	return current, false, nil
}

func checkedSpawnNumericAdd(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right || right < 0 && left < math.MinInt64-right {
		return 0, false
	}
	return left + right, true
}

func clampSpawnNumeric(value, minimum, maximum int64) int64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func lookupSpawnNumericProperty(program *Program, handle SpawnPropertyHandle) (spawnPropertyProgram, bool) {
	for _, property := range program.spawnProperties {
		if property.handle == handle {
			return property, true
		}
	}
	return spawnPropertyProgram{}, false
}

func lookupSpawnNumericState(spawn *SpawnInstance, property SpawnPropertyHandle) *numericPropertyState {
	for index := range spawn.Numeric.Properties {
		if spawn.Numeric.Properties[index].Property == property {
			return &spawn.Numeric.Properties[index]
		}
	}
	return nil
}

func bindSpawnNumericState(spawn *SpawnInstance, state *numericPropertyState, policy spawnPropertyProgram) {
	state.Bound = false
	if spawn == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return
	}
	binding, unique := uniqueSpawnNumericBinding(spawn.Program.spawnTemplates[spawn.TemplateIndex].motion, policy)
	if unique {
		state.Binding, state.Bound = binding, true
	}
}

func spawnNumericBoundValue(spawn *SpawnInstance, binding spawnPropertySlotBindingProgram) (int64, bool) {
	if spawn == nil {
		return 0, false
	}
	for _, state := range spawn.Numeric.Properties {
		if state.Bound && state.Binding == binding {
			return state.Current, true
		}
	}
	return 0, false
}

func snapshotSpawnNumeric(spawn *SpawnInstance) SpawnNumericSnapshot {
	var snapshot SpawnNumericSnapshot
	if spawn == nil || spawn.Program == nil {
		return snapshot
	}
	for _, state := range spawn.Numeric.Properties {
		policy, found := lookupSpawnNumericProperty(spawn.Program, state.Property)
		if !found {
			continue
		}
		switch policy.key {
		case spawnPropertySpeed:
			snapshot.Speed = state.Current
		case spawnPropertyRadius:
			snapshot.Radius = state.Current
		case spawnPropertyArcHeight:
			snapshot.ArcHeight = state.Current
		case spawnPropertyTurnRateMDegPerTick:
			snapshot.TurnRateMDegPerTick = state.Current
		case spawnPropertyAngularSpeedMDegPerTick:
			snapshot.AngularSpeedMDegPerTick = state.Current
		case spawnPropertyOffsetAmplitude:
			snapshot.OffsetAmplitude = state.Current
		case spawnPropertyOffsetRadius:
			snapshot.OffsetRadius = state.Current
		case spawnPropertyReturnSpeedBP:
			snapshot.ReturnSpeedBP = state.Current
		case spawnPropertyCollisionForce:
			snapshot.CollisionForce = state.Current
		}
	}
	return snapshot
}

func (runtime *Runtime) initializeSpawnNumeric(cast *castInstance, spawn *SpawnInstance) error {
	if spawn.Numeric.Initialized {
		return nil
	}
	if cast == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return ErrProgramInvariant
	}
	// numeric track 初值与绑定到数值属性的字段在启动时用施法求一次：施法流程上下文
	// （求值上下文表 cast_flow 列），即使调用方正在按 spawn_step 求其余衍生物字段。
	previous := cast.switchEvalContext(evalCastFlow)
	defer cast.switchEvalContext(previous)
	template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
	properties := make([]numericPropertyState, 0, len(spawn.Program.spawnProperties))
	for _, policy := range spawn.Program.spawnProperties {
		base, bound, err := runtime.spawnNumericBase(cast, template, policy)
		if err != nil {
			return err
		}
		if bound {
			base = clampSpawnNumeric(base, policy.minimum, policy.maximum)
			properties = append(properties, numericPropertyState{Property: policy.handle, Base: base, Current: base})
		}
	}

	shadow := *spawn
	shadow.Numeric = SpawnNumericState{Initialized: true, Properties: properties}
	for _, track := range template.numericTracks {
		value, err := runtime.evalInt(cast, track.value)
		if err != nil {
			return err
		}
		if err := replaceSpawnNumericTrack(&shadow, track.property, track.operation, value, track.overTicks, runtime.currentTick); err != nil {
			return err
		}
	}
	spawn.Numeric = shadow.Numeric
	return nil
}

func (runtime *Runtime) spawnNumericBase(cast *castInstance, template spawnTemplateProgram, policy spawnPropertyProgram) (int64, bool, error) {
	if template.motion == nil {
		return 0, false, nil
	}
	for _, binding := range policy.slotBindings {
		value, bound := spawnNumericBinding(template.motion, binding)
		if !bound {
			continue
		}
		if value == nil {
			return 0, true, nil
		}
		base, err := runtime.evalInt(cast, value)
		return base, true, err
	}
	return 0, false, nil
}

func spawnNumericBinding(motion *motionProgram, binding spawnPropertySlotBindingProgram) (programValue, bool) {
	if motion == nil {
		return nil, false
	}
	switch binding.stage {
	case spawnPropertySlotTrajectory:
		switch trajectory := motion.trajectory.(type) {
		case linearMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantLinear && binding.field == spawnPropertyFieldSpeed {
				return trajectory.speed, true
			}
		case pathMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantPath && binding.field == spawnPropertyFieldSpeed {
				return trajectory.speed, true
			}
		case orbitMotionTrajectoryProgram:
			if binding.variant != spawnPropertyVariantOrbit {
				return nil, false
			}
			switch binding.field {
			case spawnPropertyFieldRadius:
				return trajectory.radius, true
			case spawnPropertyFieldAngularSpeed:
				return trajectory.angularSpeed, true
			}
		case parabolaMotionTrajectoryProgram:
			if binding.variant != spawnPropertyVariantParabola {
				return nil, false
			}
			switch binding.field {
			case spawnPropertyFieldHeight:
				return trajectory.height, true
			case spawnPropertyFieldSpeed:
				return nil, true
			}
		}
	case spawnPropertySlotSteering:
		if _, ok := motion.steering.(trackingMotionSteeringProgram); ok && binding.variant == spawnPropertyVariantTracking && binding.field == spawnPropertyFieldTurnRateMDegPerTick {
			return nil, true
		}
	case spawnPropertySlotOffset:
		for _, offset := range motion.offsets {
			switch typed := offset.(type) {
			case zigzagMotionOffsetProgram:
				if binding.variant == spawnPropertyVariantZigzag && binding.field == spawnPropertyFieldAmplitude {
					return typed.amplitude, true
				}
			case circularMotionOffsetProgram:
				if binding.variant != spawnPropertyVariantCircular {
					continue
				}
				switch binding.field {
				case spawnPropertyFieldRadius:
					return typed.radius, true
				case spawnPropertyFieldAngularSpeed:
					return typed.angularSpeed, true
				}
			}
		}
	case spawnPropertySlotCompletion:
		if _, ok := motion.completion.(boomerangMotionCompletionProgram); ok && binding.variant == spawnPropertyVariantBoomerang && binding.field == spawnPropertyFieldReturnSpeedBP {
			return nil, true
		}
	case spawnPropertySlotCollision:
		if motion.collision != nil && binding.variant == spawnPropertyVariantPresent && binding.field == spawnPropertyFieldForce {
			return nil, true
		}
	}
	return nil, false
}

func uniqueSpawnNumericBinding(motion *motionProgram, policy spawnPropertyProgram) (spawnPropertySlotBindingProgram, bool) {
	var unique spawnPropertySlotBindingProgram
	count := 0
	for _, binding := range policy.slotBindings {
		matches := spawnNumericBindingCount(motion, binding)
		if matches == 0 {
			continue
		}
		unique = binding
		count += matches
	}
	return unique, count == 1
}

func spawnNumericBindingCount(motion *motionProgram, binding spawnPropertySlotBindingProgram) int {
	if motion == nil {
		return 0
	}
	switch binding.stage {
	case spawnPropertySlotTrajectory:
		switch motion.trajectory.(type) {
		case linearMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantLinear && binding.field == spawnPropertyFieldSpeed {
				return 1
			}
		case pathMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantPath && binding.field == spawnPropertyFieldSpeed {
				return 1
			}
		case orbitMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantOrbit && (binding.field == spawnPropertyFieldRadius || binding.field == spawnPropertyFieldAngularSpeed) {
				return 1
			}
		case parabolaMotionTrajectoryProgram:
			if binding.variant == spawnPropertyVariantParabola && (binding.field == spawnPropertyFieldHeight || binding.field == spawnPropertyFieldSpeed) {
				return 1
			}
		}
	case spawnPropertySlotSteering:
		if _, ok := motion.steering.(trackingMotionSteeringProgram); ok && binding.variant == spawnPropertyVariantTracking && binding.field == spawnPropertyFieldTurnRateMDegPerTick {
			return 1
		}
	case spawnPropertySlotOffset:
		count := 0
		for _, offset := range motion.offsets {
			switch offset.(type) {
			case zigzagMotionOffsetProgram:
				if binding.variant == spawnPropertyVariantZigzag && binding.field == spawnPropertyFieldAmplitude {
					count++
				}
			case circularMotionOffsetProgram:
				if binding.variant == spawnPropertyVariantCircular && (binding.field == spawnPropertyFieldRadius || binding.field == spawnPropertyFieldAngularSpeed) {
					count++
				}
			}
		}
		return count
	case spawnPropertySlotCompletion:
		if _, ok := motion.completion.(boomerangMotionCompletionProgram); ok && binding.variant == spawnPropertyVariantBoomerang && binding.field == spawnPropertyFieldReturnSpeedBP {
			return 1
		}
	case spawnPropertySlotCollision:
		if motion.collision != nil && binding.variant == spawnPropertyVariantPresent && binding.field == spawnPropertyFieldForce {
			return 1
		}
	}
	return 0
}
