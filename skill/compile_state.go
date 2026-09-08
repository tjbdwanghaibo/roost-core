package skill

import "sort"

type resolvedStatePlan struct {
	name                 string
	shared               SharedStateHandle
	slot                 StateSlot
	typ                  valueType
	scope                StateScope
	defaultValue         valueIR
	minimum              int64
	maximum              int64
	durationTicks        Tick
	maximumDurationTicks Tick
	onWrite              string
	clearOn              []string
	enumValues           []string
}

func runStatePass(context *compileContext) {
	context.artifacts.state.slots = make(map[string]StateSlot)
	context.artifacts.state.plans = make(map[string]resolvedStatePlan)
	index := StateSlot(0)
	for _, name := range sortedStateNames(context.artifacts.ir.persistentState) {
		declaration := context.artifacts.ir.persistentState[name]
		if declaration.scope != StateScopeOwner && declaration.scope != StateScopeOwnerTarget && declaration.scope != StateScopeTeam && declaration.scope != StateScopeMatch {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".scope", "unsupported persistent state scope")
		}
		if declaration.durationTicks <= 0 || declaration.maximumDurationTicks <= 0 || declaration.durationTicks > declaration.maximumDurationTicks || declaration.maximumDurationTicks > context.environment.Limits.MaxLifetimeTicks {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".lifetime", "persistent state lifetime must be positive and bounded")
		}
		if declaration.onWrite != "refresh" && declaration.onWrite != "keep" && declaration.onWrite != "extend" {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".lifetime.on_write", "unsupported state expiry policy")
		}
		if !validStateClearPolicies(declaration.clearOn) {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".lifetime.clear_on", "unsupported state clear policy")
		}
		typ := declaredStateType(declaration.declaredType)
		if typ.Base == valueKindInvalid {
			context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".type", "unsupported persistent state type")
		}
		if declaration.declaredType == "enum" && !validEnumValues(declaration.enumValues) {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".values", "enum values must be non-empty and unique")
		}
		if declaration.declaredType == "enum" {
			if literal, ok := declaration.defaultValue.(*stringValueIR); !ok || !containsString(declaration.enumValues, literal.value) {
				context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".default", "enum default must be a declared value")
			}
		}
		if declaration.scope != StateScopeOwnerTarget && containsString(declaration.clearOn, "target_death") {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".lifetime.clear_on", "target_death requires owner_target scope")
		}
		if declaration.declaredType == "snapshot_token" && declaration.scope != StateScopeOwner {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path+".scope", "snapshot_token state requires owner scope")
		}
		if declaration.declaredType == "int" && (declaration.minimum == nil || declaration.maximum == nil || *declaration.minimum > *declaration.maximum) {
			context.addDiagnostic(DiagnosticShapeInvalid, declaration.source.Path, "int state requires valid minimum and maximum")
		}
		if declaration.declaredType == "int" && declaration.minimum != nil && declaration.maximum != nil {
			if literal, ok := declaration.defaultValue.(*intValueIR); !ok || literal.value < *declaration.minimum || literal.value > *declaration.maximum {
				context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".default", "int state default is outside declared bounds")
			}
		}
		context.artifacts.state.slots[name] = index
		minimum, maximum := int64(0), int64(0)
		if declaration.minimum != nil {
			minimum = *declaration.minimum
		}
		if declaration.maximum != nil {
			maximum = *declaration.maximum
		}
		context.artifacts.state.plans[name] = resolvedStatePlan{name: name, slot: index, typ: typ, scope: declaration.scope, defaultValue: declaration.defaultValue, minimum: minimum, maximum: maximum, durationTicks: declaration.durationTicks, maximumDurationTicks: declaration.maximumDurationTicks, onWrite: declaration.onWrite, clearOn: append([]string(nil), declaration.clearOn...), enumValues: append([]string(nil), declaration.enumValues...)}
		index++
	}
	for _, shared := range context.environment.Gameplay.SharedStates.Entries {
		typ := valueType{Base: shared.ValueType}
		if shared.ValueType == valueKindInt {
			typ.Quantity = quantityDimensionless
		}
		context.artifacts.state.plans[shared.Key] = resolvedStatePlan{name: shared.Key, shared: shared.Handle, typ: typ, scope: StateScope(shared.Scope), defaultValue: defaultStateValueIR(shared.ValueType), minimum: shared.Minimum, maximum: shared.Maximum, durationTicks: shared.MaximumDurationTicks, maximumDurationTicks: shared.MaximumDurationTicks, onWrite: "refresh"}
	}
	context.artifacts.ir.walkValues(func(value valueIR) {
		if read, ok := value.(*stateReadValueIR); ok {
			plan, found := context.artifacts.state.plans[read.state]
			if !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, read.source.Path+".read_state.state", "unknown persistent/shared state")
				return
			}
			read.resolvedType = plan.typ
			validateStateIRBinding(context, read.source.Path+".read_state", plan.scope, read.owner, read.subject, read.teamOf)
		}
	})
	context.artifacts.ir.walkEffects(func(effect effectIR) {
		mutation, ok := effect.(*modifyStateEffectIR)
		if !ok {
			return
		}
		plan, found := context.artifacts.state.plans[mutation.state]
		if !found {
			context.addDiagnostic(DiagnosticCapabilityUnknown, mutation.source.Path+".state", "unknown persistent/shared state")
			return
		}
		if mutation.durationTicks == 0 {
			mutation.durationTicks = plan.durationTicks
		}
		if mutation.expiryPolicy == "" {
			mutation.expiryPolicy = plan.onWrite
		}
		validateStateIRBinding(context, mutation.source.Path, plan.scope, mutation.owner, mutation.subject, mutation.teamOf)
		if !stateOperationAllowed(plan.typ.Base, mutation.operation) {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".operation", "state operation is not allowed for its type")
		}
		if mutation.operation != "clear" && mutation.value == nil {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".value", "state mutation value is required")
		}
		if len(plan.enumValues) > 0 && mutation.operation == "set" {
			if literal, ok := mutation.value.(*stringValueIR); ok && !containsString(plan.enumValues, literal.value) {
				context.addDiagnostic(DiagnosticTypeMismatch, mutation.source.Path+".value", "enum state value is not declared")
			}
		}
		if mutation.operation == "clear" && mutation.value != nil {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".value", "clear state mutation cannot include a value")
		}
		if mutation.operation != "clear" && (mutation.durationTicks <= 0 || mutation.durationTicks > plan.maximumDurationTicks) {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".duration_ticks", "state mutation duration is out of bounds")
		}
		if mutation.expiryPolicy != "refresh" && mutation.expiryPolicy != "keep" && mutation.expiryPolicy != "extend" {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".expiry_policy", "invalid state expiry policy")
		}
	})
}

func defaultStateValueIR(kind valueKind) valueIR {
	switch kind {
	case valueKindBool:
		return &boolValueIR{value: false}
	case valueKindEntity, valueKindSnapshotToken:
		return &nullValueIR{}
	case valueKindPosition:
		return &nullValueIR{}
	default:
		return &intValueIR{value: 0, quantity: quantityDimensionless}
	}
}

func validateStateIRBinding(context *compileContext, path string, scope StateScope, owner, subject, teamOf valueIR) {
	valid := false
	switch scope {
	case StateScopeOwner:
		valid = owner != nil && subject == nil && teamOf == nil
	case StateScopeOwnerTarget:
		valid = owner != nil && subject != nil && teamOf == nil
	case StateScopeTeam:
		valid = owner == nil && subject == nil && teamOf != nil
	case StateScopeMatch:
		valid = owner == nil && subject == nil && teamOf == nil
	}
	if !valid {
		context.addDiagnostic(DiagnosticShapeInvalid, path, "state scope binding is missing or contains extra fields")
	}
}

func stateOperationAllowed(kind valueKind, operation string) bool {
	if operation == "set" || operation == "clear" {
		return true
	}
	return kind == valueKindInt && (operation == "add" || operation == "mul_bp" || operation == "min" || operation == "max")
}

func sortedStateNames(values map[string]stateDeclarationIR) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func validEnumValues(values []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validStateClearPolicies(values []string) bool {
	for _, value := range values {
		if value != "owner_death" && value != "target_death" && value != "skill_removed" {
			return false
		}
	}
	return true
}

func declaredStateType(name string) valueType {
	switch name {
	case "int":
		return valueType{Base: valueKindInt, Quantity: quantityDimensionless}
	case "bool":
		return valueType{Base: valueKindBool}
	case "entity":
		return valueType{Base: valueKindEntity, Optional: true}
	case "position":
		return valueType{Base: valueKindPosition}
	case "enum":
		return valueType{Base: valueKindString}
	case "snapshot_token":
		return valueType{Base: valueKindSnapshotToken, Optional: true}
	default:
		return valueType{Base: valueKindInvalid}
	}
}
