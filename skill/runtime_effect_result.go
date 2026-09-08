package skill

import (
	"fmt"
)

type runtimeEffectResultValue struct {
	typ     resultType
	outcome ResultOutcome
	fields  []RuntimeValue
}

type EffectResultEvent struct {
	EffectIndex   EffectIndex
	ResultType    string
	FailureReason ExpectedFailureReason
}

func newRuntimeEffectResult(layout resultLayoutProgram, outcome ResultOutcome, resolve func(string) (RuntimeValue, bool)) RuntimeValue {
	fields := make([]RuntimeValue, len(layout.fields))
	for index, field := range layout.fields {
		switch field.name {
		case "succeeded":
			fields[index] = BoolRuntimeValue(outcome.Succeeded)
		case "failure_reason":
			fields[index] = StringRuntimeValue(string(outcome.FailureReason))
		default:
			if !outcome.Succeeded && field.visibility == resultFieldSuccess {
				fields[index] = MissingRuntimeValue(field.typ)
				continue
			}
			if value, found := resolve(field.name); found {
				fields[index] = cloneRuntimeValue(value)
			} else {
				fields[index] = MissingRuntimeValue(field.typ)
			}
		}
	}
	return RuntimeValue{present: true, typ: effectResultReferenceType(layout, resultOutcomeAny), effectResult: runtimeEffectResultValue{typ: layout.typ, outcome: outcome, fields: fields}}
}

func runtimeEffectResultFromHost(layout resultLayoutProgram, payload EffectResultPayload) (RuntimeValue, ResultOutcome, error) {
	payload = normalizeEffectResultPayload(payload)
	outcome, payloadType, found := effectPayloadOutcome(payload)
	if !found || payloadType != layout.typ {
		return RuntimeValue{}, ResultOutcome{}, fmt.Errorf("%w: result payload %T does not match %s", ErrHostContractViolation, payload, layout.typ)
	}
	outcome, err := validateResultOutcome(layout, outcome)
	if err != nil {
		return RuntimeValue{}, ResultOutcome{}, err
	}
	value := newRuntimeEffectResult(layout, outcome, func(name string) (RuntimeValue, bool) {
		return hostResultField(payload, name)
	})
	if err := validateRuntimeEffectResult(layout, value, outcome); err != nil {
		return RuntimeValue{}, ResultOutcome{}, err
	}
	if !outcome.Succeeded && hostPayloadCarriesSuccessData(payload) {
		return RuntimeValue{}, ResultOutcome{}, fmt.Errorf("%w: failed result %s carries success-only data", ErrHostContractViolation, layout.typ)
	}
	if outcome.Succeeded {
		switch typed := payload.(type) {
		case SpawnEffectResult:
			if len(typed.Entities) == 0 || typed.FirstEntity == 0 || typed.Entities[0] != typed.FirstEntity {
				return RuntimeValue{}, ResultOutcome{}, fmt.Errorf("%w: spawn success requires a stable non-empty entity result", ErrHostContractViolation)
			}
		case SnapshotCaptureEffectResult:
			if typed.Token.opaque == 0 {
				return RuntimeValue{}, ResultOutcome{}, fmt.Errorf("%w: snapshot capture success requires a non-zero token", ErrHostContractViolation)
			}
		}
	}
	return value, outcome, nil
}

func validateRuntimeEffectResult(layout resultLayoutProgram, value RuntimeValue, outcome ResultOutcome) error {
	for index, field := range layout.fields {
		if field.name == "succeeded" || field.name == "failure_reason" || (!outcome.Succeeded && field.visibility == resultFieldSuccess) {
			continue
		}
		actual := value.effectResult.fields[index]
		if !actual.Present() {
			if field.typ.Optional {
				continue
			}
			return fmt.Errorf("%w: result %s is missing required field %s", ErrHostContractViolation, layout.typ, field.name)
		}
		if actual.typ.Base != field.typ.Base || actual.typ.Base == valueKindInt && actual.typ.Quantity != field.typ.Quantity {
			return fmt.Errorf("%w: result %s field %s has an incompatible type", ErrHostContractViolation, layout.typ, field.name)
		}
	}
	return nil
}

func hostPayloadCarriesSuccessData(payload EffectResultPayload) bool {
	switch typed := payload.(type) {
	case DamageEffectResult:
		result := typed.Result
		hasHooks := len(result.CombatHooks) > 0
		return hasHooks || hasDamageResultData(result)
	case HealEffectResult:
		return typed.Result != (HealResult{})
	case ShieldEffectResult:
		return typed.Result != (ShieldResult{})
	case TeleportEffectResult:
		return typed.Position != (Position{})
	case StatusEffectResult:
		result := typed.Result
		hasHooks := len(result.CombatHooks) > 0
		return hasHooks || hasStatusResultData(result)
	case AttributeModifierEffectResult:
		return typed.Result != (AttributeModifierResult{})
	case SpawnEffectResult:
		return len(typed.Entities) != 0 || typed.FirstEntity != 0
	case StateChangeEffectResult:
		return typed.Before.Present() || typed.After.Present() || typed.Applied
	case AbilityChangeEffectResult:
		return typed.Before.Present() || typed.After.Present() || typed.Applied
	case EntityCommandEffectResult:
		return typed.Applied
	case SnapshotCaptureEffectResult:
		return typed.Token.opaque != 0
	case SnapshotRestoreEffectResult:
		return typed.Applied || len(typed.AppliedFields) != 0 || len(typed.SkippedFields) != 0
	default:
		return false
	}
}

func hasDamageResultData(result DamageResult) bool {
	return result.Attempted != 0 || result.Mitigated != 0 || result.Absorbed != 0 || result.HealthDamage != 0 ||
		result.Critical || result.Blocked || result.Dodged || result.Immune || result.Killed || result.Parried
}

func hasStatusResultData(result StatusResult) bool {
	return result.Applied || result.Removed || result.Immune || result.PreviousStacks != 0 ||
		result.CurrentStacks != 0 || result.RemovedStacks != 0 || result.DueTick != 0 || result.PreviousDueTick != 0 ||
		result.Status != (StatusInstanceRef{}) || result.Created != (StatusInstanceRef{})
}

func normalizeEffectResultPayload(payload EffectResultPayload) EffectResultPayload {
	switch typed := payload.(type) {
	case *DamageEffectResult:
		if typed != nil {
			return *typed
		}
	case *HealEffectResult:
		if typed != nil {
			return *typed
		}
	case *ShieldEffectResult:
		if typed != nil {
			return *typed
		}
	case *TeleportEffectResult:
		if typed != nil {
			return *typed
		}
	case *StatusEffectResult:
		if typed != nil {
			return *typed
		}
	case *AttributeModifierEffectResult:
		if typed != nil {
			return *typed
		}
	case *SpawnEffectResult:
		if typed != nil {
			return *typed
		}
	case *StateChangeEffectResult:
		if typed != nil {
			return *typed
		}
	case *AbilityChangeEffectResult:
		if typed != nil {
			return *typed
		}
	case *EntityCommandEffectResult:
		if typed != nil {
			return *typed
		}
	case *SnapshotCaptureEffectResult:
		if typed != nil {
			return *typed
		}
	case *SnapshotRestoreEffectResult:
		if typed != nil {
			return *typed
		}
	}
	return payload
}

func validateResultOutcome(layout resultLayoutProgram, outcome ResultOutcome) (ResultOutcome, error) {
	if outcome.Succeeded && outcome.FailureReason == ExpectedFailureNone {
		return outcome, nil
	}
	if !outcome.Succeeded && outcome.FailureReason != "" && outcome.FailureReason != ExpectedFailureNone && layout.allows(outcome.FailureReason) {
		return outcome, nil
	}
	return ResultOutcome{}, fmt.Errorf("%w: result %s returned inconsistent outcome succeeded=%t reason=%q", ErrHostContractViolation, layout.typ, outcome.Succeeded, outcome.FailureReason)
}

func effectPayloadOutcome(payload EffectResultPayload) (ResultOutcome, resultType, bool) {
	switch typed := payload.(type) {
	case DamageEffectResult:
		return typed.ResultOutcome, resultTypeDamage, true
	case HealEffectResult:
		return typed.ResultOutcome, resultTypeHeal, true
	case ShieldEffectResult:
		return typed.ResultOutcome, resultTypeShield, true
	case TeleportEffectResult:
		return typed.ResultOutcome, resultTypeTeleport, true
	case StatusEffectResult:
		return typed.ResultOutcome, resultTypeStatusOperation, true
	case AttributeModifierEffectResult:
		return typed.ResultOutcome, resultTypeAttributeModifier, true
	case SpawnEffectResult:
		return typed.ResultOutcome, resultTypeSpawn, true
	case StateChangeEffectResult:
		return typed.ResultOutcome, resultTypeStateChange, true
	case AbilityChangeEffectResult:
		return typed.ResultOutcome, resultTypeAbilityChange, true
	case EntityCommandEffectResult:
		return typed.ResultOutcome, resultTypeEntityCommand, true
	case SnapshotCaptureEffectResult:
		return typed.ResultOutcome, resultTypeSnapshotCapture, true
	case SnapshotRestoreEffectResult:
		return typed.ResultOutcome, resultTypeSnapshotRestore, true
	default:
		return ResultOutcome{}, "", false
	}
}

func hostResultField(payload EffectResultPayload, name string) (RuntimeValue, bool) {
	switch typed := payload.(type) {
	case DamageEffectResult:
		result := typed.Result
		switch name {
		case "attempted":
			return IntRuntimeValue(result.Attempted, quantityCombatAmount), true
		case "mitigated":
			return IntRuntimeValue(result.Mitigated, quantityCombatAmount), true
		case "absorbed":
			return IntRuntimeValue(result.Absorbed, quantityCombatAmount), true
		case "health_damage":
			return IntRuntimeValue(result.HealthDamage, quantityCombatAmount), true
		case "critical":
			return BoolRuntimeValue(result.Critical), true
		case "blocked":
			return BoolRuntimeValue(result.Blocked), true
		case "dodged":
			return BoolRuntimeValue(result.Dodged), true
		case "immune":
			return BoolRuntimeValue(result.Immune), true
		case "parried":
			return BoolRuntimeValue(result.Parried), true
		case "killed":
			return BoolRuntimeValue(result.Killed), true
		}
	case HealEffectResult:
		switch name {
		case "attempted":
			return IntRuntimeValue(typed.Result.Attempted, quantityCombatAmount), true
		case "effective":
			return IntRuntimeValue(typed.Result.Effective, quantityCombatAmount), true
		}
	case ShieldEffectResult:
		if name == "added" {
			return IntRuntimeValue(typed.Result.Added, quantityCombatAmount), true
		}
	case TeleportEffectResult:
		if name == "position" {
			return PositionRuntimeValue(typed.Position), true
		}
	case StatusEffectResult:
		switch name {
		case "applied":
			return BoolRuntimeValue(typed.Result.Applied), true
		case "removed":
			return BoolRuntimeValue(typed.Result.Removed), true
		case "immune":
			return BoolRuntimeValue(typed.Result.Immune), true
		case "previous_stacks":
			return IntRuntimeValue(int64(typed.Result.PreviousStacks), quantityCount), true
		case "current_stacks":
			return IntRuntimeValue(int64(typed.Result.CurrentStacks), quantityCount), true
		case "removed_stacks":
			return IntRuntimeValue(int64(typed.Result.RemovedStacks), quantityCount), true
		case "due_tick":
			return IntRuntimeValue(int64(typed.Result.DueTick), quantityTicks), true
		}
	case AttributeModifierEffectResult:
		switch name {
		case "applied":
			return BoolRuntimeValue(typed.Result.Applied), true
		case "due_tick":
			return IntRuntimeValue(int64(typed.Result.DueTick), quantityTicks), true
		}
	case SpawnEffectResult:
		switch name {
		case "entities":
			return EntityListRuntimeValue(typed.Entities), true
		case "first_entity":
			return EntityRuntimeValue(typed.FirstEntity), true
		}
	case StateChangeEffectResult:
		return stateChangeResultField(typed.Before, typed.After, typed.Applied, name)
	case AbilityChangeEffectResult:
		return stateChangeResultField(typed.Before, typed.After, typed.Applied, name)
	case EntityCommandEffectResult:
		if name == "applied" {
			return BoolRuntimeValue(typed.Applied), true
		}
	case SnapshotCaptureEffectResult:
		if name == "token" {
			return SnapshotTokenRuntimeValue(typed.Token), true
		}
	case SnapshotRestoreEffectResult:
		switch name {
		case "applied":
			return BoolRuntimeValue(typed.Applied), true
		case "applied_fields":
			return StringListRuntimeValue(typed.AppliedFields), true
		case "skipped_fields":
			return StringListRuntimeValue(typed.SkippedFields), true
		}
	}
	return RuntimeValue{}, false
}

func stateChangeResultField(before, after RuntimeValue, applied bool, name string) (RuntimeValue, bool) {
	switch name {
	case "before":
		return before, true
	case "after":
		return after, true
	case "applied":
		return BoolRuntimeValue(applied), true
	default:
		return RuntimeValue{}, false
	}
}

func (runtime *Runtime) resolveEffectResult(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, value RuntimeValue, outcome ResultOutcome) (flowControl, error) {
	if !outcome.Succeeded {
		event := RuntimeEvent{Tick: runtime.currentTick, Kind: "effect_expected_failure", Entity: cast.caster, Context: runtime.effectEventContext(cast, effectIndex), Result: &EffectResultEvent{EffectIndex: effectIndex, ResultType: string(continuations.result.typ), FailureReason: outcome.FailureReason}}
		runtime.appendRuntimeEvent(event)
		runtime.appendCastEvent(cast, event)
	}
	root, hasRoot := continuations.success, continuations.hasSuccess
	if !outcome.Succeeded {
		root, hasRoot = continuations.failure, continuations.hasFailure
	}
	if !hasRoot {
		return flowControl{kind: flowContinue}, nil
	}
	if !continuations.hasResultLocal {
		return runtime.executeEffectResultRoot(cast, root)
	}
	if int(continuations.resultLocal) >= len(cast.locals) {
		return flowControl{}, ErrProgramInvariant
	}
	previous := cast.locals[continuations.resultLocal]
	cast.locals[continuations.resultLocal] = cloneRuntimeValue(value)
	control, err := runtime.executeEffectResultRoot(cast, root)
	cast.locals[continuations.resultLocal] = previous
	return control, err
}

func (runtime *Runtime) resolveHostEffectExecution(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, result EffectResult, err error) (flowControl, error) {
	if err != nil {
		return flowControl{}, err
	}
	if continuations.result.typ == "" {
		return flowControl{kind: flowContinue}, nil
	}
	value, outcome, err := runtimeEffectResultFromHost(continuations.result, result.Payload)
	if err != nil {
		return flowControl{}, err
	}
	return runtime.resolveEffectResult(cast, continuations, effectIndex, value, outcome)
}

func (runtime *Runtime) resolveStateEffectExecution(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, result StateMutationResult, err error) (flowControl, error) {
	if err != nil {
		return flowControl{}, err
	}
	if !result.Succeeded && (result.Before.Present() || result.After.Present()) {
		return flowControl{}, fmt.Errorf("%w: failed state result carries success-only data", ErrHostContractViolation)
	}
	applied := result.Succeeded
	return runtime.resolveDirectEffectExecution(cast, continuations, effectIndex, result.ResultOutcome, func(name string) (RuntimeValue, bool) {
		switch name {
		case "before":
			return result.Before, true
		case "after":
			return result.After, true
		case "applied":
			return BoolRuntimeValue(applied), true
		default:
			return RuntimeValue{}, false
		}
	})
}

func (runtime *Runtime) resolveAbilityEffectExecution(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, result AbilityChangeResult, err error) (flowControl, error) {
	if err != nil {
		return flowControl{}, err
	}
	applied := result.Succeeded
	return runtime.resolveDirectEffectExecution(cast, continuations, effectIndex, result.ResultOutcome, func(name string) (RuntimeValue, bool) {
		switch name {
		case "before":
			return result.Before, true
		case "after":
			return result.After, true
		case "applied":
			return BoolRuntimeValue(applied), true
		default:
			return RuntimeValue{}, false
		}
	})
}

func (runtime *Runtime) resolveDirectEffectExecution(cast *castInstance, continuations effectContinuations, effectIndex EffectIndex, outcome ResultOutcome, resolve func(string) (RuntimeValue, bool)) (flowControl, error) {
	if continuations.result.typ == "" {
		return flowControl{kind: flowContinue}, nil
	}
	outcome, err := validateResultOutcome(continuations.result, outcome)
	if err != nil {
		return flowControl{}, err
	}
	value := newRuntimeEffectResult(continuations.result, outcome, resolve)
	if err := validateRuntimeEffectResult(continuations.result, value, outcome); err != nil {
		return flowControl{}, err
	}
	return runtime.resolveEffectResult(cast, continuations, effectIndex, value, outcome)
}

func (runtime *Runtime) executeEffectResultRoot(cast *castInstance, root OperationIndex) (flowControl, error) {
	control, err := runtime.executeOperation(cast, root)
	if err == nil && control.kind == flowSuspend {
		return flowControl{}, ErrProgramInvariant
	}
	return control, err
}

func cloneRuntimeValue(value RuntimeValue) RuntimeValue {
	result := value
	result.path = append([]Position(nil), value.path...)
	result.entities = append([]EntityID(nil), value.entities...)
	result.strings = append([]string(nil), value.strings...)
	result.effectResult.fields = make([]RuntimeValue, len(value.effectResult.fields))
	for index, field := range value.effectResult.fields {
		result.effectResult.fields[index] = cloneRuntimeValue(field)
	}
	return result
}
