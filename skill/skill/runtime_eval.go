package skill

import (
	"errors"
	"fmt"
)

var ErrRuntimeDivisionByZero = errors.New("skill: runtime division by zero")

func (runtime *Runtime) evalValue(cast *castInstance, value programValue) (RuntimeValue, error) {
	if value == nil {
		return RuntimeValue{}, ErrProgramInvariant
	}
	switch typed := value.(type) {
	case nullProgramValue:
		return MissingRuntimeValue(typed.typ), nil
	case intProgramValue:
		return IntRuntimeValue(typed.value, typed.typ.Quantity), nil
	case boolProgramValue:
		return BoolRuntimeValue(typed.value), nil
	case stringProgramValue:
		return StringRuntimeValue(typed.value), nil
	case referenceProgramValue:
		return runtime.evalReference(cast, typed)
	case expressionProgramValue:
		return runtime.evalExpression(cast, typed)
	case attributeReadProgramValue:
		if shouldCacheSnapshot(typed.snapshot) {
			if cached, ok := cast.snapshots[typed.snapshotSlot]; ok {
				return cached, nil
			}
		}
		entityValue, err := runtime.evalValue(cast, typed.entity)
		if err != nil {
			return RuntimeValue{}, err
		}
		entity, ok := entityValue.Entity()
		if !ok {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		read, err := runtime.host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: cast.visibleRevision}, Payload: AttributeRead{Entity: entity, Attribute: typed.attribute}})
		if err != nil {
			return RuntimeValue{}, err
		}
		cast.visibleRevision = maxRevision(cast.visibleRevision, read.Meta.Revision)
		if shouldCacheSnapshot(typed.snapshot) {
			cast.snapshots[typed.snapshotSlot] = read.Value
		}
		return read.Value, nil
	case stateReadProgramValue:
		return runtime.evalStateRead(cast, typed)
	case abilityStateReadProgramValue:
		property, found := abilityPropertyByHandle(cast.program, typed.property)
		if !found || property.name != typed.name {
			return RuntimeValue{}, ErrProgramInvariant
		}
		owner, err := runtime.evalEntity(cast, typed.owner)
		if err != nil {
			return RuntimeValue{}, err
		}
		abilityValue, err := runtime.evalValue(cast, typed.ability)
		if err != nil {
			return RuntimeValue{}, err
		}
		ability, ok := abilityValue.Ability()
		if !ok || ability.Owner != owner || !runtime.abilityOwnerAllowed(cast.program, cast.caster, owner) {
			return RuntimeValue{}, ErrCastInputRejected
		}
		return runtime.readAbilityStateLocked(owner, ability.Handle, typed.name)
	default:
		return RuntimeValue{}, fmt.Errorf("%w: unsupported value %T", ErrProgramInvariant, value)
	}
}

func (runtime *Runtime) captureSnapshots(cast *castInstance, point snapshotPoint) error {
	for _, plan := range cast.program.snapshots {
		if plan.point != point {
			continue
		}
		entityValue, err := runtime.evalValue(cast, plan.entity)
		if err != nil {
			return err
		}
		entity, ok := entityValue.Entity()
		if !ok {
			return ErrRuntimeTypeMismatch
		}
		read, err := runtime.host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: cast.visibleRevision}, Payload: AttributeRead{Entity: entity, Attribute: plan.attribute}})
		if err != nil {
			return err
		}
		cast.visibleRevision = maxRevision(cast.visibleRevision, read.Meta.Revision)
		cast.snapshots[plan.slot] = read.Value
	}
	return nil
}

func shouldCacheSnapshot(point snapshotPoint) bool {
	return point == snapshotCastStart || point == snapshotPhaseStart || point == snapshotProcessStart
}

func (runtime *Runtime) evalReference(cast *castInstance, reference referenceProgramValue) (RuntimeValue, error) {
	var value RuntimeValue
	switch reference.kind {
	case referenceInput:
		if int(reference.index) >= len(cast.inputs) {
			return RuntimeValue{}, ErrProgramInvariant
		}
		value = cast.inputs[reference.index]
	case referenceMemory:
		if int(reference.index) >= len(cast.memory) {
			return RuntimeValue{}, ErrProgramInvariant
		}
		value = cast.memory[reference.index]
	case referenceLocal:
		if int(reference.index) >= len(cast.locals) {
			return RuntimeValue{}, ErrProgramInvariant
		}
		value = cast.locals[reference.index]
	case referenceBuiltin:
		switch reference.builtin {
		case "$caster":
			value = EntityRuntimeValue(cast.caster)
		case "$caster.position":
			return runtime.readEntityPosition(cast, cast.caster)
		case "$primary_target":
			if cast.primaryTarget == 0 {
				value = MissingRuntimeValue(reference.typ)
			} else {
				value = EntityRuntimeValue(cast.primaryTarget)
			}
		case "$cast.mode":
			value = StringRuntimeValue(string(cast.program.cast.mode))
		case "$cast.elapsed_ticks":
			value = IntRuntimeValue(int64(runtime.currentTick-cast.startTick), quantityTicks)
		case "$cast.charge_bp":
			value = IntRuntimeValue(runtime.castChargeBP(cast), quantityBasisPoints)
		case "$cast.pulse_index":
			value = IntRuntimeValue(cast.pulseIndex, quantityCount)
		case "$cast.release_reason":
			value = StringRuntimeValue(cast.releaseReason)
		case "$cast.stock":
			if cast.program.cast.mode == castModeAmmo {
				cast.stock = runtime.ammoState(cast).stock
			}
			value = IntRuntimeValue(cast.stock, quantityCount)
		case "$cast.max_stock":
			value = IntRuntimeValue(cast.maxStock, quantityCount)
		case "$ability.self":
			value = AbilityRuntimeValue(AbilityRef{Owner: cast.caster, Handle: cast.ability})
		case "$owner":
			if cast.detachedProcess == nil {
				return RuntimeValue{}, ErrProgramInvariant
			}
			value = EntityRuntimeValue(cast.detachedProcess.Owner)
		case "$owner.position":
			if cast.detachedProcess == nil {
				return RuntimeValue{}, ErrProgramInvariant
			}
			return runtime.readEntityPosition(cast, cast.detachedProcess.Owner)
		case "$lifecycle_entity":
			if cast.detachedProcess == nil {
				return RuntimeValue{}, ErrProgramInvariant
			}
			value = EntityRuntimeValue(cast.detachedProcess.LifecycleEntity)
		case "$process":
			if cast.detachedProcess == nil {
				return RuntimeValue{}, ErrProgramInvariant
			}
			value = ProcessRuntimeValue(cast.detachedProcess.ID)
		case "$event.source":
			value = EntityRuntimeValue(cast.detachedEvent.Source)
		case "$event.owner":
			value = EntityRuntimeValue(cast.detachedEvent.Owner)
		case "$event.target":
			value = EntityRuntimeValue(cast.detachedEvent.Target)
		case "$event.tick":
			value = IntRuntimeValue(int64(cast.detachedEvent.Tick), quantityTicks)
		case "$event.membership_ticks":
			value = IntRuntimeValue(cast.detachedEvent.MembershipTicks, quantityTicks)
		case "$event.enter_count":
			value = IntRuntimeValue(cast.detachedEvent.EnterCount, quantityCount)
		default:
			return RuntimeValue{}, fmt.Errorf("%w: builtin %q", ErrProgramInvariant, reference.builtin)
		}
	default:
		return RuntimeValue{}, ErrProgramInvariant
	}
	if reference.field == "" || !value.Present() {
		return value, nil
	}
	if reference.resultField != 0 {
		field, found := value.effectResultField(reference.resultField)
		if !found {
			return RuntimeValue{}, ErrProgramInvariant
		}
		return field, nil
	}
	switch reference.field {
	case "position":
		entity, ok := value.Entity()
		if !ok {
			if hit, hitOK := value.Hit(); hitOK {
				return PositionRuntimeValue(hit.Position), nil
			}
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		return runtime.readEntityPosition(cast, entity)
	case "entity", "target":
		hit, ok := value.Hit()
		if !ok {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		return EntityRuntimeValue(hit.Entity), nil
	default:
		return RuntimeValue{}, fmt.Errorf("%w: reference field %q", ErrProgramInvariant, reference.field)
	}
}

func (runtime *Runtime) readEntityPosition(cast *castInstance, entity EntityID) (RuntimeValue, error) {
	read, err := runtime.host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: cast.visibleRevision}, Payload: PositionRead{Entity: entity}})
	if err != nil {
		return RuntimeValue{}, err
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, read.Meta.Revision)
	return read.Value, nil
}

func (runtime *Runtime) evalExpression(cast *castInstance, expression expressionProgramValue) (RuntimeValue, error) {
	if expression.op == "exists" {
		if len(expression.args) != 1 {
			return RuntimeValue{}, ErrProgramInvariant
		}
		value, err := runtime.evalValue(cast, expression.args[0])
		if err != nil {
			return RuntimeValue{}, err
		}
		return BoolRuntimeValue(value.Present()), nil
	}
	args := make([]RuntimeValue, len(expression.args))
	for index, argument := range expression.args {
		value, err := runtime.evalValue(cast, argument)
		if err != nil {
			return RuntimeValue{}, err
		}
		if !value.Present() {
			return RuntimeValue{}, ErrRuntimeValueMissing
		}
		args[index] = value
	}
	switch expression.op {
	case "add":
		return CheckedAddRuntimeValues(args[0], args[1])
	case "sub":
		left, right, err := runtimeIntegers(args)
		if err != nil {
			return RuntimeValue{}, err
		}
		result, ok := checkedInt64Sub(left, right)
		if !ok {
			return RuntimeValue{}, ErrRuntimeArithmeticOverflow
		}
		return IntRuntimeValue(result, expression.typ.Quantity), nil
	case "mul":
		left, right, err := runtimeIntegers(args)
		if err != nil {
			return RuntimeValue{}, err
		}
		result, ok := checkedInt64Mul(left, right)
		if !ok {
			return RuntimeValue{}, ErrRuntimeArithmeticOverflow
		}
		return IntRuntimeValue(result, expression.typ.Quantity), nil
	case "div":
		left, right, err := runtimeIntegers(args)
		if err != nil {
			return RuntimeValue{}, err
		}
		if right == 0 {
			return RuntimeValue{}, ErrRuntimeDivisionByZero
		}
		if left == -int64(^uint64(0)>>1)-1 && right == -1 {
			return RuntimeValue{}, ErrRuntimeArithmeticOverflow
		}
		return IntRuntimeValue(left/right, expression.typ.Quantity), nil
	case "scale_bp":
		return CheckedScaleBPRuntimeValue(args[0], args[1])
	case "min", "max":
		left, right, err := runtimeIntegers(args)
		if err != nil {
			return RuntimeValue{}, err
		}
		if expression.op == "min" {
			left = minInt64(left, right)
		} else {
			left = maxInt64(left, right)
		}
		return IntRuntimeValue(left, expression.typ.Quantity), nil
	case "clamp":
		if len(args) != 3 {
			return RuntimeValue{}, ErrProgramInvariant
		}
		value, minimum, err := runtimeIntegers(args[:2])
		if err != nil {
			return RuntimeValue{}, err
		}
		maximum, ok := args[2].Int()
		if !ok {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		return IntRuntimeValue(maxInt64(minimum, minInt64(maximum, value)), expression.typ.Quantity), nil
	case "and", "or":
		left, leftOK := args[0].Bool()
		right, rightOK := args[1].Bool()
		if !leftOK || !rightOK {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		if expression.op == "and" {
			return BoolRuntimeValue(left && right), nil
		}
		return BoolRuntimeValue(left || right), nil
	case "not":
		value, ok := args[0].Bool()
		if !ok {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		return BoolRuntimeValue(!value), nil
	case "eq", "ne", "lt", "lte", "gt", "gte":
		comparison, err := compareRuntimeValues(args[0], args[1])
		if err != nil {
			return RuntimeValue{}, err
		}
		result := comparison == 0
		switch expression.op {
		case "ne":
			result = comparison != 0
		case "lt":
			result = comparison < 0
		case "lte":
			result = comparison <= 0
		case "gt":
			result = comparison > 0
		case "gte":
			result = comparison >= 0
		}
		return BoolRuntimeValue(result), nil
	default:
		return RuntimeValue{}, fmt.Errorf("%w: expression %q", ErrProgramInvariant, expression.op)
	}
}

func checkedInt64Sub(left, right int64) (int64, bool) {
	maximum := int64(^uint64(0) >> 1)
	minimum := -maximum - 1
	if (right > 0 && left < minimum+right) || (right < 0 && left > maximum+right) {
		return 0, false
	}
	return left - right, true
}

func runtimeIntegers(values []RuntimeValue) (int64, int64, error) {
	if len(values) < 2 {
		return 0, 0, ErrProgramInvariant
	}
	left, leftOK := values[0].Int()
	right, rightOK := values[1].Int()
	if !leftOK || !rightOK {
		return 0, 0, ErrRuntimeTypeMismatch
	}
	return left, right, nil
}

func compareRuntimeValues(left, right RuntimeValue) (int, error) {
	if left.typ.Base != right.typ.Base {
		return 0, ErrRuntimeTypeMismatch
	}
	switch left.typ.Base {
	case valueKindInt:
		leftValue, _ := left.Int()
		rightValue, _ := right.Int()
		if leftValue < rightValue {
			return -1, nil
		}
		if leftValue > rightValue {
			return 1, nil
		}
		return 0, nil
	case valueKindBool:
		leftValue, _ := left.Bool()
		rightValue, _ := right.Bool()
		if leftValue == rightValue {
			return 0, nil
		}
		if !leftValue {
			return -1, nil
		}
		return 1, nil
	case valueKindString:
		leftValue, _ := left.String()
		rightValue, _ := right.String()
		if leftValue < rightValue {
			return -1, nil
		}
		if leftValue > rightValue {
			return 1, nil
		}
		return 0, nil
	case valueKindEntity:
		leftValue, _ := left.Entity()
		rightValue, _ := right.Entity()
		if leftValue < rightValue {
			return -1, nil
		}
		if leftValue > rightValue {
			return 1, nil
		}
		return 0, nil
	case valueKindAbility:
		leftValue, _ := left.Ability()
		rightValue, _ := right.Ability()
		if leftValue.Owner < rightValue.Owner || leftValue.Owner == rightValue.Owner && leftValue.Handle < rightValue.Handle {
			return -1, nil
		}
		if leftValue.Owner > rightValue.Owner || leftValue.Owner == rightValue.Owner && leftValue.Handle > rightValue.Handle {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, ErrRuntimeTypeMismatch
	}
}

func maxRevision(left, right WorldRevision) WorldRevision {
	if left > right {
		return left
	}
	return right
}
