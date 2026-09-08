package skill

import "math/big"

const normalizedDirectionScale int64 = 10000

func (runtime *Runtime) Input(castID CastID, port InputPort, input InputPayload) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[castID]
	if cast == nil || (cast.status != CastRunning && cast.status != CastSuspended) || cast.logicalFinished || cast.windowStage == CastWindowRecovering || cast.windowStage == CastWindowComplete || cast.windowStage == CastWindowCancelled {
		return ErrCastInputRejected
	}
	if !containsInputPort(cast.program.input.updatePorts, port) {
		return ErrCastInputRejected
	}
	if _, found := phaseRootOperation(cast.program, cast.program.phases[cast.currentPhase], string(port)); !found {
		return ErrCastInputRejected
	}
	candidate := make([]RuntimeValue, len(cast.inputs))
	for index, value := range cast.inputs {
		candidate[index] = cloneRuntimeValue(value)
	}
	switch port {
	case InputPortDirectionChanged:
		if input.Direction == nil || input.Target != 0 || input.Position != nil || input.StartPosition != nil || input.EndPosition != nil || len(input.Path) != 0 || zeroDirection(*input.Direction) {
			return ErrCastInputInvalid
		}
		index, ok := inputSlotIndex(cast.program.input, "$input.direction")
		if !ok {
			return ErrProgramInvariant
		}
		candidate[index] = DirectionRuntimeValue(*input.Direction)
	case InputPortTargetChanged:
		if input.Target == 0 || input.Position != nil || input.Direction != nil || input.StartPosition != nil || input.EndPosition != nil || len(input.Path) != 0 {
			return ErrCastInputInvalid
		}
		if cast.program.input.hasMaximumRange {
			casterPosition, err := readInputEntityPosition(runtime.host, cast.caster)
			if err != nil || !inputEntityWithinRange(runtime.host, casterPosition, input.Target, cast.program.input) {
				return ErrCastInputInvalid
			}
		}
		index, ok := inputSlotIndex(cast.program.input, "$input.target")
		if !ok {
			return ErrProgramInvariant
		}
		candidate[index] = EntityRuntimeValue(input.Target)
	default:
		return ErrCastInputRejected
	}
	cast.inputs = candidate
	if port == InputPortTargetChanged {
		cast.primaryTarget = input.Target
		cast.eventContext.Target = input.Target
	}
	cast.visibleRevision = runtime.host.CurrentRevision()
	return runtime.executeCastEvent(cast, string(port))
}

func containsInputPort(ports []InputPort, wanted InputPort) bool {
	for _, port := range ports {
		if port == wanted {
			return true
		}
	}
	return false
}

func inputSlotIndex(plan inputProgram, name string) (int, bool) {
	for index, slot := range plan.slots {
		if slot.name == name {
			return index, true
		}
	}
	return 0, false
}

func freezeCastInput(program *Program, input CastInput, host Host) ([]RuntimeValue, error) {
	if input.Caster == 0 {
		return nil, ErrCastInputInvalid
	}
	values := make(map[string]RuntimeValue, len(program.input.slots))
	var casterPosition Position
	casterPositionRead := false
	readCasterPosition := func() bool {
		if casterPositionRead {
			return true
		}
		position, err := readInputEntityPosition(host, input.Caster)
		if err != nil {
			return false
		}
		casterPosition, casterPositionRead = position, true
		return true
	}
	noTarget := input.Target == 0
	noPosition := input.Position == nil
	noDirection := input.Direction == nil
	noPoints := input.StartPosition == nil && input.EndPosition == nil
	noPath := len(input.Path) == 0
	switch program.input.kind {
	case inputNone:
		if !noTarget || !noPosition || !noDirection || !noPoints || !noPath {
			return nil, ErrCastInputInvalid
		}
	case inputDirection:
		if !noTarget || !noPosition || input.Direction == nil || !noPoints || !noPath || zeroDirection(*input.Direction) {
			return nil, ErrCastInputInvalid
		}
		values["$input.direction"] = DirectionRuntimeValue(*input.Direction)
	case inputPosition:
		if !noTarget || input.Position == nil || !noDirection || !noPoints || !noPath {
			return nil, ErrCastInputInvalid
		}
		if program.input.hasMaximumRange && !readCasterPosition() {
			return nil, ErrCastInputInvalid
		}
		position, ok := normalizeInputPosition(host, input.Caster, casterPosition, *input.Position, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		values["$input.position"] = PositionRuntimeValue(position)
	case inputEntity:
		if input.Target == 0 || !noPosition || !noDirection || !noPoints || !noPath {
			return nil, ErrCastInputInvalid
		}
		if program.input.hasMaximumRange && (!readCasterPosition() || !inputEntityWithinRange(host, casterPosition, input.Target, program.input)) {
			return nil, ErrCastInputInvalid
		}
		values["$input.target"] = EntityRuntimeValue(input.Target)
	case inputDirectionPosition:
		if !noTarget || input.Position == nil || input.Direction == nil || !noPoints || !noPath || zeroDirection(*input.Direction) {
			return nil, ErrCastInputInvalid
		}
		if program.input.hasMaximumRange && !readCasterPosition() {
			return nil, ErrCastInputInvalid
		}
		position, ok := normalizeInputPosition(host, input.Caster, casterPosition, *input.Position, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		values["$input.direction"], values["$input.position"] = DirectionRuntimeValue(*input.Direction), PositionRuntimeValue(position)
	case inputEntityPosition:
		if input.Target == 0 || input.Position == nil || !noDirection || !noPoints || !noPath {
			return nil, ErrCastInputInvalid
		}
		if program.input.hasMaximumRange && (!readCasterPosition() || !inputEntityWithinRange(host, casterPosition, input.Target, program.input)) {
			return nil, ErrCastInputInvalid
		}
		position, ok := normalizeInputPosition(host, input.Caster, casterPosition, *input.Position, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		values["$input.target"], values["$input.position"] = EntityRuntimeValue(input.Target), PositionRuntimeValue(position)
	case inputTwoPoint, inputDrag:
		if !noTarget || !noPosition || !noDirection || input.StartPosition == nil || input.EndPosition == nil || !noPath {
			return nil, ErrCastInputInvalid
		}
		if !readCasterPosition() {
			return nil, ErrCastInputInvalid
		}
		start, ok := normalizeInputPosition(host, input.Caster, casterPosition, *input.StartPosition, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		end, ok := normalizeInputPosition(host, input.Caster, casterPosition, *input.EndPosition, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		length := integerDistance(start, end)
		if distanceExceeds(start, end, program.input.maximumLength) {
			if program.input.clampPolicy == "reject" {
				return nil, ErrCastInputInvalid
			}
			end = clampPosition(start, end, program.input.maximumLength)
			end, ok = resolveInputPosition(host, input.Caster, end, program.input.clampPolicy)
			if !ok {
				return nil, ErrCastInputInvalid
			}
		}
		length = integerDistance(start, end)
		if length < program.input.minimumLength || distanceExceeds(start, end, program.input.maximumLength) ||
			(program.input.hasMaximumRange && distanceExceeds(casterPosition, end, program.input.maximumRange)) {
			return nil, ErrCastInputInvalid
		}
		values["$input.start_position"], values["$input.end_position"] = PositionRuntimeValue(start), PositionRuntimeValue(end)
		if program.input.kind == inputDrag {
			values["$input.drag_direction"] = DirectionRuntimeValue(normalizedDirection(start, end, length))
			values["$input.drag_length"] = IntRuntimeValue(length, quantityWorldDistance)
		}
	case inputPath:
		if !noTarget || !noPosition || !noDirection || !noPoints || len(input.Path) < 2 || len(input.Path) > program.input.maximumPathPoints {
			return nil, ErrCastInputInvalid
		}
		path, ok := normalizeInputPath(host, input.Caster, input.Path, program.input)
		if !ok {
			return nil, ErrCastInputInvalid
		}
		values["$input.path"] = PathRuntimeValue(path)
		values["$input.start_position"] = PositionRuntimeValue(path[0])
		values["$input.end_position"] = PositionRuntimeValue(path[len(path)-1])
	default:
		return nil, ErrCastInputInvalid
	}
	result := make([]RuntimeValue, len(program.input.slots))
	for index, slot := range program.input.slots {
		value, ok := values[slot.name]
		if !ok {
			return nil, ErrCastInputInvalid
		}
		result[index] = cloneRuntimeValue(value)
	}
	return result, nil
}

func normalizeInputPath(host Host, caster EntityID, source []Position, plan inputProgram) ([]Position, bool) {
	first, ok := resolveInputPosition(host, caster, source[0], plan.clampPolicy)
	if !ok {
		return nil, false
	}
	result := make([]Position, 1, len(source))
	result[0] = first
	total := int64(0)
	for _, requested := range source[1:] {
		position, valid := resolveInputPosition(host, caster, requested, plan.clampPolicy)
		if !valid {
			return nil, false
		}
		previous := result[len(result)-1]
		segment := integerDistance(previous, position)
		if segment < plan.minimumSegmentLength {
			if plan.simplificationPolicy == "reject" {
				return nil, false
			}
			continue
		}
		remaining := saturatingInt64Sub(plan.maximumPathLength, total)
		if distanceExceeds(previous, position, remaining) {
			if plan.clampPolicy == "reject" || remaining <= 0 {
				return nil, false
			}
			position = clampPosition(previous, position, remaining)
			position, valid = resolveInputPosition(host, caster, position, plan.clampPolicy)
			if !valid {
				return nil, false
			}
			segment = integerDistance(previous, position)
			if segment < plan.minimumSegmentLength || distanceExceeds(previous, position, remaining) {
				return nil, false
			}
			result = append(result, position)
			total = saturatingInt64Add(total, segment)
			break
		}
		result = append(result, position)
		total = saturatingInt64Add(total, segment)
	}
	if len(result) < 2 || total > plan.maximumPathLength {
		return nil, false
	}
	return result, true
}

func readInputEntityPosition(host Host, entity EntityID) (Position, error) {
	result, err := host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: host.CurrentRevision()}, Payload: PositionRead{Entity: entity}})
	if err != nil {
		return Position{}, err
	}
	position, ok := result.Value.Position()
	if !ok {
		return Position{}, ErrHostContractViolation
	}
	return position, nil
}

func inputEntityWithinRange(host Host, caster Position, target EntityID, plan inputProgram) bool {
	if !plan.hasMaximumRange {
		return true
	}
	position, err := readInputEntityPosition(host, target)
	if err != nil {
		return false
	}
	return !distanceExceeds(caster, position, plan.maximumRange)
}

func normalizeInputPosition(host Host, caster EntityID, origin, requested Position, plan inputProgram) (Position, bool) {
	position := requested
	if plan.hasMaximumRange && distanceExceeds(origin, position, plan.maximumRange) {
		if plan.clampPolicy == "reject" {
			return Position{}, false
		}
		position = clampPosition(origin, position, plan.maximumRange)
	}
	position, ok := resolveInputPosition(host, caster, position, plan.clampPolicy)
	if !ok {
		return Position{}, false
	}
	if plan.hasMaximumRange && distanceExceeds(origin, position, plan.maximumRange) {
		return Position{}, false
	}
	return position, true
}

func resolveInputPosition(host Host, caster EntityID, position Position, policy string) (Position, bool) {
	resolver, ok := host.(InputPositionResolver)
	if !ok {
		return position, true
	}
	return resolver.ResolveInputPosition(InputPositionRequest{Caster: caster, Position: position, Policy: policy})
}

func zeroDirection(direction Direction) bool { return direction.X == 0 && direction.Y == 0 }

func clampPosition(origin, requested Position, maximum int64) Position {
	length, saturated := integerDistanceSaturated(origin, requested)
	if !saturated && (length == 0 || length <= maximum) {
		return requested
	}
	if saturated ||
		absDeltaUint64(requested.X, origin.X) > uint64(maxInt64Value) ||
		absDeltaUint64(requested.Y, origin.Y) > uint64(maxInt64Value) {
		// Deltas beyond int64 need the exact arbitrary-precision path; this
		// domain is unreachable for real world coordinates.
		return clampPositionBig(origin, requested, maximum)
	}
	dx := saturatingInt64Sub(requested.X, origin.X)
	dy := saturatingInt64Sub(requested.Y, origin.Y)
	result := Position{
		X: saturatingInt64Add(origin.X, mulDivRounded(dx, maximum, length)),
		Y: saturatingInt64Add(origin.Y, mulDivRounded(dy, maximum, length)),
	}
	for distanceExceeds(origin, result, maximum) {
		result = stepPositionToward(origin, result)
	}
	return result
}

// clampPositionBig is the arbitrary-precision clamp kept for deltas beyond
// int64, preserving the historical exact behavior at extreme magnitudes.
func clampPositionBig(origin, requested Position, maximum int64) Position {
	length := new(big.Int)
	{
		dx := new(big.Int).Sub(big.NewInt(requested.X), big.NewInt(origin.X))
		dy := new(big.Int).Sub(big.NewInt(requested.Y), big.NewInt(origin.Y))
		squared := new(big.Int).Add(new(big.Int).Mul(dx, dx), new(big.Int).Mul(dy, dy))
		length.Sqrt(squared)
		remainder := new(big.Int).Sub(squared, new(big.Int).Mul(new(big.Int).Set(length), length))
		if remainder.Cmp(new(big.Int).Add(length, big.NewInt(1))) >= 0 {
			length.Add(length, big.NewInt(1))
		}
	}
	if length.Sign() == 0 || length.Cmp(big.NewInt(maximum)) <= 0 {
		return requested
	}
	dx := new(big.Int).Sub(big.NewInt(requested.X), big.NewInt(origin.X))
	dy := new(big.Int).Sub(big.NewInt(requested.Y), big.NewInt(origin.Y))
	x := new(big.Int).Add(big.NewInt(origin.X), scaleBigRatioRounded(dx, maximum, length))
	y := new(big.Int).Add(big.NewInt(origin.Y), scaleBigRatioRounded(dy, maximum, length))
	result := Position{
		X: bigIntToInt64Saturated(x),
		Y: bigIntToInt64Saturated(y),
	}
	for distanceExceeds(origin, result, maximum) {
		result = stepPositionToward(origin, result)
	}
	return result
}

func scaleBigRatioRounded(value *big.Int, numerator int64, denominator *big.Int) *big.Int {
	if denominator.Sign() == 0 {
		return new(big.Int)
	}
	product := new(big.Int).Mul(value, big.NewInt(numerator))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, denominator, remainder)
	twiceRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
	if twiceRemainder.Cmp(new(big.Int).Abs(denominator)) >= 0 {
		if product.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	return quotient
}

func bigIntToInt64Saturated(value *big.Int) int64 {
	if value.IsInt64() {
		return value.Int64()
	}
	if value.Sign() < 0 {
		return -int64(^uint64(0)>>1) - 1
	}
	return int64(^uint64(0) >> 1)
}

func stepPositionToward(origin, position Position) Position {
	dx := saturatingInt64Sub(position.X, origin.X)
	dy := saturatingInt64Sub(position.Y, origin.Y)
	if absoluteDifference(dx, 0) >= absoluteDifference(dy, 0) && dx != 0 {
		if dx > 0 {
			position.X--
		} else {
			position.X++
		}
		return position
	}
	if dy > 0 {
		position.Y--
	} else if dy < 0 {
		position.Y++
	}
	return position
}

func normalizedDirection(start, end Position, length int64) Direction {
	if length <= 0 {
		return Direction{}
	}
	return Direction{
		X: scaleRatioRounded(saturatingInt64Sub(end.X, start.X), normalizedDirectionScale, length),
		Y: scaleRatioRounded(saturatingInt64Sub(end.Y, start.Y), normalizedDirectionScale, length),
	}
}

// scaleRatioRounded delegates to the allocation-free 128-bit implementation;
// bit-exactness with the historical big.Int version is locked by
// TestMulDivRoundedMatchesReference.
func scaleRatioRounded(value, numerator, denominator int64) int64 {
	return mulDivRounded(value, numerator, denominator)
}
