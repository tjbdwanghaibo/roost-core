package skill

import "math"

type numericPropertyState struct {
	Property ProcessPropertyHandle
	Base     int64
	Current  int64
	Track    *numericTrackState
	Binding  processPropertySlotBindingProgram
	Bound    bool
}

type numericTrackState struct {
	Start     int64
	Target    int64
	StartTick Tick
	OverTicks Tick
}

func advanceProcessNumeric(process *ProcessInstance, tick Tick) (ProcessNumericSnapshot, error) {
	if process == nil || process.Program == nil || !process.Numeric.Initialized {
		return ProcessNumericSnapshot{}, ErrProgramInvariant
	}
	for index := range process.Numeric.Properties {
		state := &process.Numeric.Properties[index]
		policy, found := lookupProcessNumericProperty(process.Program, state.Property)
		if !found {
			return ProcessNumericSnapshot{}, ErrProgramInvariant
		}
		if state.Track != nil {
			current, complete, err := sampleProcessNumericTrack(*state.Track, tick)
			if err != nil {
				return ProcessNumericSnapshot{}, err
			}
			state.Current = clampProcessNumeric(current, policy.minimum, policy.maximum)
			if complete {
				state.Track = nil
			}
		}
	}
	return snapshotProcessNumeric(process), nil
}

func replaceProcessNumericTrack(process *ProcessInstance, property ProcessPropertyHandle, operation processNumericOperation, operand int64, overTicks, tick Tick) error {
	if process == nil || process.Program == nil || !process.Numeric.Initialized || overTicks < 0 {
		return ErrProgramInvariant
	}
	policy, found := lookupProcessNumericProperty(process.Program, property)
	if !found || policy.interpolation != processNumericLinearInteger || policy.rounding != processNumericTruncateTowardZero {
		return ErrProgramInvariant
	}
	state := lookupProcessNumericState(process, property)
	if state == nil {
		return ErrProgramInvariant
	}

	var target int64
	var ok bool
	switch operation {
	case processNumericSet:
		target = operand
	case processNumericAdd:
		target, ok = checkedProcessNumericAdd(state.Current, operand)
		if !ok {
			return ErrRuntimeArithmeticOverflow
		}
	case processNumericMulBP:
		target, ok = checkedInt64Mul(state.Current, operand)
		if !ok {
			return ErrRuntimeArithmeticOverflow
		}
		target /= 10000
	default:
		return ErrProgramInvariant
	}
	target = clampProcessNumeric(target, policy.minimum, policy.maximum)
	state.Track = &numericTrackState{Start: state.Current, Target: target, StartTick: tick, OverTicks: overTicks}
	bindProcessNumericState(process, state, policy)
	return nil
}

func (runtime *Runtime) executeModifyProcess(cast *castInstance, operation modifyProcessOperation) error {
	processValue, err := runtime.evalValue(cast, operation.process)
	if err != nil {
		return err
	}
	if !processValue.Present() {
		return ErrRuntimeValueMissing
	}
	processID, ok := processValue.Process()
	if !ok {
		return ErrRuntimeTypeMismatch
	}
	process, err := activeCallbackProcess(cast, processID)
	if err != nil {
		return err
	}
	value, err := runtime.evalInt(cast, operation.value)
	if err != nil {
		return err
	}
	return replaceProcessNumericTrack(process, operation.property, operation.operation, value, operation.overTicks, runtime.currentTick)
}

func sampleProcessNumericTrack(track numericTrackState, tick Tick) (int64, bool, error) {
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
	current, ok := checkedProcessNumericAdd(track.Start, offset)
	if !ok {
		return 0, false, ErrRuntimeArithmeticOverflow
	}
	return current, false, nil
}

func checkedProcessNumericAdd(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right || right < 0 && left < math.MinInt64-right {
		return 0, false
	}
	return left + right, true
}

func clampProcessNumeric(value, minimum, maximum int64) int64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func lookupProcessNumericProperty(program *Program, handle ProcessPropertyHandle) (processPropertyProgram, bool) {
	for _, property := range program.processProperties {
		if property.handle == handle {
			return property, true
		}
	}
	return processPropertyProgram{}, false
}

func lookupProcessNumericState(process *ProcessInstance, property ProcessPropertyHandle) *numericPropertyState {
	for index := range process.Numeric.Properties {
		if process.Numeric.Properties[index].Property == property {
			return &process.Numeric.Properties[index]
		}
	}
	return nil
}

func bindProcessNumericState(process *ProcessInstance, state *numericPropertyState, policy processPropertyProgram) {
	state.Bound = false
	if process == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return
	}
	binding, unique := uniqueProcessNumericBinding(process.Program.processTemplates[process.TemplateIndex].motion, policy)
	if unique {
		state.Binding, state.Bound = binding, true
	}
}

func processNumericBoundValue(process *ProcessInstance, binding processPropertySlotBindingProgram) (int64, bool) {
	if process == nil {
		return 0, false
	}
	for _, state := range process.Numeric.Properties {
		if state.Bound && state.Binding == binding {
			return state.Current, true
		}
	}
	return 0, false
}

func snapshotProcessNumeric(process *ProcessInstance) ProcessNumericSnapshot {
	var snapshot ProcessNumericSnapshot
	if process == nil || process.Program == nil {
		return snapshot
	}
	for _, state := range process.Numeric.Properties {
		policy, found := lookupProcessNumericProperty(process.Program, state.Property)
		if !found {
			continue
		}
		switch policy.key {
		case processPropertySpeed:
			snapshot.Speed = state.Current
		case processPropertyRadius:
			snapshot.Radius = state.Current
		case processPropertyArcHeight:
			snapshot.ArcHeight = state.Current
		case processPropertyTurnRateMDegPerTick:
			snapshot.TurnRateMDegPerTick = state.Current
		case processPropertyAngularSpeedMDegPerTick:
			snapshot.AngularSpeedMDegPerTick = state.Current
		case processPropertyOffsetAmplitude:
			snapshot.OffsetAmplitude = state.Current
		case processPropertyOffsetRadius:
			snapshot.OffsetRadius = state.Current
		case processPropertyReturnSpeedBP:
			snapshot.ReturnSpeedBP = state.Current
		case processPropertyCollisionForce:
			snapshot.CollisionForce = state.Current
		}
	}
	return snapshot
}

func (runtime *Runtime) initializeProcessNumeric(cast *castInstance, process *ProcessInstance) error {
	if process.Numeric.Initialized {
		return nil
	}
	if cast == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return ErrProgramInvariant
	}
	template := process.Program.processTemplates[process.TemplateIndex]
	properties := make([]numericPropertyState, 0, len(process.Program.processProperties))
	for _, policy := range process.Program.processProperties {
		base, bound, err := runtime.processNumericBase(cast, template, policy)
		if err != nil {
			return err
		}
		if bound {
			base = clampProcessNumeric(base, policy.minimum, policy.maximum)
			properties = append(properties, numericPropertyState{Property: policy.handle, Base: base, Current: base})
		}
	}

	shadow := *process
	shadow.Numeric = ProcessNumericState{Initialized: true, Properties: properties}
	for _, track := range template.numericTracks {
		value, err := runtime.evalInt(cast, track.value)
		if err != nil {
			return err
		}
		if err := replaceProcessNumericTrack(&shadow, track.property, track.operation, value, track.overTicks, runtime.currentTick); err != nil {
			return err
		}
	}
	process.Numeric = shadow.Numeric
	return nil
}

func (runtime *Runtime) processNumericBase(cast *castInstance, template processTemplateProgram, policy processPropertyProgram) (int64, bool, error) {
	if template.motion == nil {
		return 0, false, nil
	}
	for _, binding := range policy.slotBindings {
		value, bound := processNumericBinding(template.motion, binding)
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

func processNumericBinding(motion *motionProgram, binding processPropertySlotBindingProgram) (programValue, bool) {
	if motion == nil {
		return nil, false
	}
	switch binding.stage {
	case processPropertySlotTrajectory:
		switch trajectory := motion.trajectory.(type) {
		case linearMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantLinear && binding.field == processPropertyFieldSpeed {
				return trajectory.speed, true
			}
		case pathMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantPath && binding.field == processPropertyFieldSpeed {
				return trajectory.speed, true
			}
		case orbitMotionTrajectoryProgram:
			if binding.variant != processPropertyVariantOrbit {
				return nil, false
			}
			switch binding.field {
			case processPropertyFieldRadius:
				return trajectory.radius, true
			case processPropertyFieldAngularSpeed:
				return trajectory.angularSpeed, true
			}
		case parabolaMotionTrajectoryProgram:
			if binding.variant != processPropertyVariantParabola {
				return nil, false
			}
			switch binding.field {
			case processPropertyFieldHeight:
				return trajectory.height, true
			case processPropertyFieldSpeed:
				return nil, true
			}
		}
	case processPropertySlotSteering:
		if _, ok := motion.steering.(trackingMotionSteeringProgram); ok && binding.variant == processPropertyVariantTracking && binding.field == processPropertyFieldTurnRateMDegPerTick {
			return nil, true
		}
	case processPropertySlotOffset:
		for _, offset := range motion.offsets {
			switch typed := offset.(type) {
			case zigzagMotionOffsetProgram:
				if binding.variant == processPropertyVariantZigzag && binding.field == processPropertyFieldAmplitude {
					return typed.amplitude, true
				}
			case circularMotionOffsetProgram:
				if binding.variant != processPropertyVariantCircular {
					continue
				}
				switch binding.field {
				case processPropertyFieldRadius:
					return typed.radius, true
				case processPropertyFieldAngularSpeed:
					return typed.angularSpeed, true
				}
			}
		}
	case processPropertySlotCompletion:
		if _, ok := motion.completion.(boomerangMotionCompletionProgram); ok && binding.variant == processPropertyVariantBoomerang && binding.field == processPropertyFieldReturnSpeedBP {
			return nil, true
		}
	case processPropertySlotCollision:
		if motion.collision != nil && binding.variant == processPropertyVariantPresent && binding.field == processPropertyFieldForce {
			return nil, true
		}
	}
	return nil, false
}

func uniqueProcessNumericBinding(motion *motionProgram, policy processPropertyProgram) (processPropertySlotBindingProgram, bool) {
	var unique processPropertySlotBindingProgram
	count := 0
	for _, binding := range policy.slotBindings {
		matches := processNumericBindingCount(motion, binding)
		if matches == 0 {
			continue
		}
		unique = binding
		count += matches
	}
	return unique, count == 1
}

func processNumericBindingCount(motion *motionProgram, binding processPropertySlotBindingProgram) int {
	if motion == nil {
		return 0
	}
	switch binding.stage {
	case processPropertySlotTrajectory:
		switch motion.trajectory.(type) {
		case linearMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantLinear && binding.field == processPropertyFieldSpeed {
				return 1
			}
		case pathMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantPath && binding.field == processPropertyFieldSpeed {
				return 1
			}
		case orbitMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantOrbit && (binding.field == processPropertyFieldRadius || binding.field == processPropertyFieldAngularSpeed) {
				return 1
			}
		case parabolaMotionTrajectoryProgram:
			if binding.variant == processPropertyVariantParabola && (binding.field == processPropertyFieldHeight || binding.field == processPropertyFieldSpeed) {
				return 1
			}
		}
	case processPropertySlotSteering:
		if _, ok := motion.steering.(trackingMotionSteeringProgram); ok && binding.variant == processPropertyVariantTracking && binding.field == processPropertyFieldTurnRateMDegPerTick {
			return 1
		}
	case processPropertySlotOffset:
		count := 0
		for _, offset := range motion.offsets {
			switch offset.(type) {
			case zigzagMotionOffsetProgram:
				if binding.variant == processPropertyVariantZigzag && binding.field == processPropertyFieldAmplitude {
					count++
				}
			case circularMotionOffsetProgram:
				if binding.variant == processPropertyVariantCircular && (binding.field == processPropertyFieldRadius || binding.field == processPropertyFieldAngularSpeed) {
					count++
				}
			}
		}
		return count
	case processPropertySlotCompletion:
		if _, ok := motion.completion.(boomerangMotionCompletionProgram); ok && binding.variant == processPropertyVariantBoomerang && binding.field == processPropertyFieldReturnSpeedBP {
			return 1
		}
	case processPropertySlotCollision:
		if motion.collision != nil && binding.variant == processPropertyVariantPresent && binding.field == processPropertyFieldForce {
			return 1
		}
	}
	return 0
}
