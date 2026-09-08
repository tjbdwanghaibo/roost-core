package skill

import (
	"fmt"
	"strings"
)

type typeScope map[string]valueType

type typeChecker struct {
	context    *compileContext
	attributes map[string]AttributeCatalogEntry
	memory     map[string]valueType
	state      map[string]valueType
}

func runTypeSnapshotPass(context *compileContext) {
	prepareEffectResultLayouts(context)
	runSnapshotPass(context)
	runTypeCheckPass(context)
}

func runTypeCheckPass(context *compileContext) {
	checker := typeChecker{
		context:    context,
		attributes: make(map[string]AttributeCatalogEntry),
		memory:     make(map[string]valueType),
		state:      make(map[string]valueType),
	}
	for _, entry := range context.environment.Gameplay.Attributes.Entries {
		checker.attributes[entry.Key] = entry
	}
	context.artifacts.types.types = make(map[string]valueType)
	for name, declaration := range context.artifacts.ir.memory {
		typ := declaredMemoryType(declaration.declaredType)
		if typ.Base == valueKindInvalid {
			context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".type", "memory has an unsupported declared type")
		}
		if _, nullable := declaration.defaultValue.(*nullValueIR); nullable {
			typ.Optional = true
		} else {
			checker.expect(declaration.defaultValue, checker.baseScope(), typ)
		}
		checker.memory[name] = typ
	}
	for _, name := range sortedStateNames(context.artifacts.ir.persistentState) {
		declaration := context.artifacts.ir.persistentState[name]
		typ := declaredStateType(declaration.declaredType)
		if _, nullable := declaration.defaultValue.(*nullValueIR); nullable {
			if !typ.Optional {
				context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".default", "state type does not allow null default")
			}
		} else {
			checker.expect(declaration.defaultValue, checker.baseScope(), typ)
		}
		checker.state[name] = typ
	}

	root := checker.baseScope()
	window := context.artifacts.ir.activation.castWindow
	if window.hasWindupExpression {
		checker.expect(window.windupExpression, root, quantityType(quantityTicks))
	}
	if window.hasRecoveryExpression {
		checker.expect(window.recoveryExpression, root, quantityType(quantityTicks))
	}
	for _, cost := range context.artifacts.ir.costs {
		checker.expect(cost.amount, root, quantityType(quantityResourceAmount))
	}
	for _, cost := range context.artifacts.ir.activation.policy.sustainCosts {
		checker.expect(cost.amount, root, quantityType(quantityResourceAmount))
	}
	for _, phase := range context.artifacts.ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) { checker.flow(flow, cloneTypeScope(root)) })
	}
}

func declaredMemoryType(name string) valueType {
	switch name {
	case "int":
		return valueType{Base: valueKindInt, Quantity: quantityDimensionless}
	case "bool":
		return valueType{Base: valueKindBool}
	case "string":
		return valueType{Base: valueKindString}
	case "entity":
		return valueType{Base: valueKindEntity}
	case "position":
		return valueType{Base: valueKindPosition}
	case "hit":
		return valueType{Base: valueKindHit}
	case "path":
		return valueType{Base: valueKindPath}
	case "direction":
		return valueType{Base: valueKindDirection}
	default:
		return valueType{Base: valueKindInvalid}
	}
}

func (c *typeChecker) baseScope() typeScope {
	scope := typeScope{
		"$caster":          {Base: valueKindEntity},
		"$caster.position": {Base: valueKindPosition},
		"$primary_target":  {Base: valueKindEntity, Optional: true},
		"$ability.self":    {Base: valueKindAbility},
	}
	for name, typ := range c.context.artifacts.input.Slots {
		scope[name] = typ
	}
	for name, typ := range c.memory {
		scope["$memory."+name] = typ
	}
	policy := c.context.artifacts.ir.activation.policy
	scope["$cast.mode"] = valueType{Base: valueKindString}
	scope["$cast.elapsed_ticks"] = quantityType(quantityTicks)
	switch policy.mode {
	case castModeCharge:
		scope["$cast.charge_bp"] = quantityType(quantityBasisPoints)
		scope["$cast.release_reason"] = valueType{Base: valueKindString}
	case castModeHold, castModeToggle:
		scope["$cast.pulse_index"] = quantityType(quantityCount)
	case castModeAmmo:
		scope["$cast.stock"] = quantityType(quantityCount)
		scope["$cast.max_stock"] = quantityType(quantityCount)
	}
	return scope
}

func cloneTypeScope(scope typeScope) typeScope {
	copy := make(typeScope, len(scope)+1)
	for name, typ := range scope {
		copy[name] = typ
	}
	return copy
}

func (c *typeChecker) flow(flow flowIR, scope typeScope) {
	if flow == nil {
		return
	}
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		for _, child := range typed.steps {
			c.flow(child, scope)
		}
	case *parallelFlowIR:
		for _, branch := range typed.branches {
			c.flow(branch, cloneTypeScope(scope))
		}
	case *ifFlowIR:
		c.expect(typed.condition, scope, valueType{Base: valueKindBool})
		thenScope := cloneTypeScope(scope)
		if reference, ok := directlyGuardedReference(typed.condition); ok {
			if current, found := thenScope[reference]; found {
				thenScope[reference] = withoutOptional(current)
			}
		}
		c.flow(typed.thenFlow, thenScope)
		c.flow(typed.elseFlow, cloneTypeScope(scope))
	case *repeatFlowIR:
		c.expect(typed.times, scope, quantityType(quantityCount))
		child := cloneTypeScope(scope)
		child["$local."+typed.index.Name] = quantityType(quantityCount)
		c.flow(typed.body, child)
	case *waitFlowIR:
		c.flow(typed.then, cloneTypeScope(scope))
	case *selectFlowIR:
		c.selectPlan(&typed.selectPlan, scope)
		element := selectionValueType(typed.selectPlan.elementType)
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			child := cloneTypeScope(scope)
			child["$local."+consume.local.Name] = element
			c.flow(consume.then, child)
		case *selectEachConsumeIR:
			child := cloneTypeScope(scope)
			child["$local."+consume.local.Name] = element
			c.flow(consume.body, child)
		}
		c.flow(typed.onEmpty, cloneTypeScope(scope))
	case *effectFlowIR:
		c.effect(typed.effect, scope)
		c.process(typed.process, scope)
		if typed.result != nil {
			successScope, failureScope := cloneTypeScope(scope), cloneTypeScope(scope)
			if typed.result.local != nil {
				successScope["$local."+typed.result.local.Name] = effectResultReferenceType(typed.result.layout, resultOutcomeSuccess)
				failureScope["$local."+typed.result.local.Name] = effectResultReferenceType(typed.result.layout, resultOutcomeFailure)
			}
			c.flow(typed.result.success, successScope)
			c.flow(typed.result.failure, failureScope)
		}
		if typed.callbacks != nil {
			callbackScope := typeScope{
				"$owner": {Base: valueKindEntity}, "$owner.position": {Base: valueKindPosition},
				"$lifecycle_entity": {Base: valueKindEntity}, "$process": {Base: valueKindProcess},
				"$event.source": {Base: valueKindEntity}, "$event.owner": {Base: valueKindEntity}, "$event.target": {Base: valueKindEntity}, "$event.tick": quantityType(quantityTicks),
			}
			if typed.process != nil {
				if typed.process.kind == "area" {
					callbackScope["$event.membership_ticks"] = quantityType(quantityTicks)
					callbackScope["$event.enter_count"] = quantityType(quantityCount)
				}
				for _, policy := range c.context.environment.ProcessProperties.Properties {
					if containsString(policy.ProcessKinds, typed.process.kind) && processPropertyBindingCount(typed.process.motion, policy) == 1 {
						callbackScope["#process_property:"+policy.Key] = valueType{Base: valueKindInt}
					}
				}
			}
			walkPhaseFlows(phaseEventsIR{enter: typed.callbacks.enter, cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition, release: typed.callbacks.targetLost, pulse: typed.callbacks.tick}, func(child flowIR) { c.flow(child, cloneTypeScope(callbackScope)) })
			c.flow(typed.callbacks.leave, cloneTypeScope(callbackScope))
		}
	case *gotoFlowIR, *finishFlowIR:
		return
	}
}

func (c *typeChecker) process(process *processIR, scope typeScope) {
	if process == nil {
		return
	}
	if process.area != nil {
		c.selectPlan(process.area, scope)
	}
	seen := make(map[string]bool)
	for _, track := range process.numericTracks {
		path := track.source.Path
		policy, found := lookupProcessPropertyPolicy(c.context.environment.ProcessProperties, track.property)
		if !found {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "property is not mutable in the process property catalog")
		} else {
			if !containsString(policy.ProcessKinds, process.kind) || processPropertyBindingCount(process.motion, policy) != 1 {
				c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "property must bind exactly one Motion slot for this process")
			}
			if !containsString(policy.Operations, track.operation) {
				c.context.addDiagnostic(DiagnosticShapeInvalid, path+".operation", "operation is not allowed by the process property policy")
			}
		}
		if seen[track.property] {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "initial numeric property must not be duplicated")
		}
		seen[track.property] = true
		if track.overTicks < 0 {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".over_ticks", "over_ticks must be non-negative")
		}
		c.expect(track.value, scope, valueType{Base: valueKindInt})
	}
	if process.motion == nil {
		return
	}
	motion, ok := process.motion.(*canonicalMotionIR)
	if !ok {
		return
	}
	entity := valueType{Base: valueKindEntity}
	position := valueType{Base: valueKindPosition}
	distance := quantityType(quantityWorldDistance)
	angle := quantityType(quantityAngleMDeg)
	if frame, ok := motion.frame.(followFrameIR); ok {
		c.expect(frame.target, scope, entity)
	}
	if steering, ok := motion.steering.(trackingSteeringIR); ok {
		c.expect(steering.target, scope, entity)
	}
	switch trajectory := motion.trajectory.(type) {
	case linearTrajectoryIR:
		c.expect(trajectory.speed, scope, distance)
	case pathTrajectoryIR:
		c.expect(trajectory.points, scope, valueType{Base: valueKindPath})
		c.expect(trajectory.speed, scope, distance)
	case orbitTrajectoryIR:
		c.expect(trajectory.anchor, scope, entity)
		c.expect(trajectory.radius, scope, distance)
		c.expect(trajectory.angularSpeed, scope, angle)
	case parabolaTrajectoryIR:
		c.expect(trajectory.destination, scope, position)
		c.expect(trajectory.height, scope, distance)
	}
	for _, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagOffsetIR:
			c.expect(typed.amplitude, scope, distance)
		case circularOffsetIR:
			c.expect(typed.radius, scope, distance)
			c.expect(typed.angularSpeed, scope, angle)
		}
	}
	if motion.carry != nil {
		c.expect(motion.carry.target, scope, entity)
	}
}

func directlyGuardedReference(value valueIR) (string, bool) {
	expression, ok := value.(*expressionValueIR)
	if !ok || expression.op != "exists" || len(expression.args) != 1 {
		return "", false
	}
	reference, ok := expression.args[0].(*referenceValueIR)
	if !ok {
		return "", false
	}
	return reference.reference, true
}

func (c *typeChecker) selectPlan(plan *selectIR, scope typeScope) {
	c.infer(plan.from, scope, nil)
	switch shape := plan.shape.(type) {
	case *statusSetShapeIR:
		c.expect(plan.from, scope, valueType{Base: valueKindEntity})
	case *circleShapeIR:
		c.expect(shape.radius, scope, quantityType(quantityWorldDistance))
	case *ringShapeIR:
		c.expect(shape.innerRadius, scope, quantityType(quantityWorldDistance))
		c.expect(shape.outerRadius, scope, quantityType(quantityWorldDistance))
	case *coneShapeIR:
		c.expect(shape.rangeValue, scope, quantityType(quantityWorldDistance))
		c.expect(shape.angleDeg, scope, quantityType(quantityAngleMDeg))
		c.expect(shape.direction, scope, valueType{Base: valueKindDirection})
	case *lineShapeIR:
		c.expect(shape.length, scope, quantityType(quantityWorldDistance))
		c.expect(shape.width, scope, quantityType(quantityWorldDistance))
		c.expect(shape.direction, scope, valueType{Base: valueKindDirection})
	case *rectangleShapeIR:
		c.expect(shape.length, scope, quantityType(quantityWorldDistance))
		c.expect(shape.width, scope, quantityType(quantityWorldDistance))
		c.expect(shape.direction, scope, valueType{Base: valueKindDirection})
	case *raycastShapeIR:
		c.expect(shape.length, scope, quantityType(quantityWorldDistance))
		c.expect(shape.direction, scope, valueType{Base: valueKindDirection})
	case *chainShapeIR:
		c.expect(shape.hopRange, scope, quantityType(quantityWorldDistance))
	case *pathShapeIR:
		c.expect(shape.points, scope, valueType{Base: valueKindPath})
	case *nearestValidShapeIR:
		c.expect(shape.searchRadius, scope, quantityType(quantityWorldDistance))
	}
	for _, filter := range plan.filters {
		if compare, ok := filter.(*attributeCompareFilterIR); ok {
			if attribute, found := c.attributes[compare.attribute]; found {
				c.expect(compare.value, scope, valueType{Base: attribute.ValueType, Quantity: attribute.Quantity})
			}
		}
		if status, ok := filter.(*statusInstanceFilterIR); ok && status.value != nil {
			expected := valueType{Base: valueKindEntity}
			if status.kind == "status_stack_compare" {
				expected = quantityType(quantityCount)
			}
			if status.kind == "status_duration_compare" {
				expected = quantityType(quantityTicks)
			}
			c.expect(status.value, scope, expected)
		}
	}
}

func selectionValueType(element selectionElementType) valueType {
	switch element {
	case selectionEntity:
		return valueType{Base: valueKindEntity}
	case selectionPosition:
		return valueType{Base: valueKindPosition}
	case selectionHit:
		return valueType{Base: valueKindHit}
	case selectionPath:
		return valueType{Base: valueKindPath}
	case selectionAbility:
		return valueType{Base: valueKindAbility}
	case selectionStatusInstance:
		return valueType{Base: valueKindStatusInstance}
	default:
		return valueType{Base: valueKindInvalid}
	}
}

func (c *typeChecker) effect(effect effectIR, scope typeScope) {
	entity := valueType{Base: valueKindEntity}
	combat := quantityType(quantityCombatAmount)
	distance := quantityType(quantityWorldDistance)
	switch typed := effect.(type) {
	case *captureSnapshotEffectIR:
		c.expect(typed.target, scope, entity)
	case *restoreSnapshotEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.snapshot, scope, valueType{Base: valueKindSnapshotToken})
	case *damageEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.amount, scope, combat)
	case *healEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.amount, scope, combat)
	case *shieldEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.amount, scope, combat)
	case *addStatusEffectIR:
		c.expect(typed.target, scope, entity)
	case *removeStatusEffectIR:
		c.expect(typed.target, scope, entity)
	case *modifyStatusInstanceEffectIR:
		c.expect(typed.status, scope, valueType{Base: valueKindStatusInstance})
		if typed.value != nil {
			quantity := quantityCount
			if typed.operation == "add_duration" || typed.operation == "set_duration" {
				quantity = quantityTicks
			}
			if typed.operation == "mul_duration_bp" {
				quantity = quantityBasisPoints
			}
			c.expect(typed.value, scope, quantityType(quantity))
		}
		if typed.target != nil {
			c.expect(typed.target, scope, entity)
		}
	case *attributeModifierEffectIR:
		c.expect(typed.target, scope, entity)
		if attribute, found := c.attributes[typed.attribute]; found {
			c.expect(typed.value, scope, valueType{Base: attribute.ValueType, Quantity: attribute.Quantity})
		}
	case *resourceEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.amount, scope, quantityType(quantityResourceAmount))
	case *setMemoryEffectIR:
		if memoryType, found := c.memory[typed.name]; found {
			c.expect(typed.value, scope, withoutOptional(memoryType))
		}
	case *addMemoryEffectIR:
		if memoryType, found := c.memory[typed.name]; found {
			c.expect(typed.value, scope, withoutOptional(memoryType))
		}
	case *teleportEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.destination, scope, valueType{Base: valueKindPosition})
	case *knockbackEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.from, scope, valueType{Base: valueKindPosition})
		c.expect(typed.distance, scope, distance)
	case *pullEffectIR:
		c.expect(typed.target, scope, entity)
		c.expect(typed.toward, scope, valueType{Base: valueKindPosition})
		c.expect(typed.distance, scope, distance)
	case *stopMovementEffectIR:
		c.expect(typed.target, scope, entity)
	case *modifyStateEffectIR:
		plan, found := c.context.artifacts.state.plans[typed.state]
		if found {
			c.expectStateBinding(typed.owner, typed.subject, typed.teamOf, scope)
			if typed.operation != "clear" {
				expected := withoutOptional(plan.typ)
				if typed.operation == "mul_bp" {
					expected = quantityType(quantityBasisPoints)
				}
				c.expect(typed.value, scope, expected)
			}
		}
	case *modifyAbilityStateEffectIR:
		c.expect(typed.owner, scope, entity)
		c.expect(typed.ability, scope, valueType{Base: valueKindAbility})
		if property, found := c.context.artifacts.ability.properties[typed.property]; found {
			expected := valueType{Base: property.policy.ValueType}
			if expected.Base == valueKindInt {
				expected.Quantity = abilityPropertyQuantity(typed.property)
			}
			if typed.operation == "mul_bp" {
				expected = quantityType(quantityBasisPoints)
			}
			c.expect(typed.value, scope, expected)
		}
	case *modifyProcessEffectIR:
		c.expect(typed.process, scope, valueType{Base: valueKindProcess})
		processReference, isProcessReference := typed.process.(*referenceValueIR)
		if !isProcessReference || processReference.reference != "$process" {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".process", "modify_process requires the current callback $process")
		}
		policy, found := lookupProcessPropertyPolicy(c.context.environment.ProcessProperties, typed.property)
		if !found || scope["#process_property:"+typed.property].Base == valueKindInvalid {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".property", "property is not bound to the current process Motion")
		} else if !containsString(policy.Operations, typed.operation) {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".operation", "operation is not allowed by the process property policy")
		}
		if typed.overTicks < 0 {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".over_ticks", "over_ticks must be non-negative")
		}
		c.expect(typed.value, scope, valueType{Base: valueKindInt})
	case *spawnEffectIR:
		c.expect(typed.position, scope, valueType{Base: valueKindPosition})
		if template, found := unitTemplateEntry(c.context, typed.template); found {
			for _, override := range typed.attributeOverrides {
				if expected, allowed := unitTemplateOverrideType(c.context, template, override.attribute); allowed {
					c.expect(override.value, scope, expected)
				}
			}
			for _, binding := range typed.parameterBindings {
				if expected, allowed := unitTemplateParameterType(template, binding.name); allowed {
					c.expect(binding.value, scope, expected)
				}
			}
		}
	case *entityCommandEffectIR:
		c.expect(typed.target, scope, entity)
		if typed.position != nil {
			c.expect(typed.position, scope, valueType{Base: valueKindPosition})
		}
		if typed.targetEntity != nil {
			c.expect(typed.targetEntity, scope, entity)
		}
	case *clearMemoryEffectIR:
		return
	}
}

func lookupProcessPropertyPolicy(catalog ProcessPropertyCatalog, key string) (ProcessPropertyPolicy, bool) {
	for _, policy := range catalog.Properties {
		if policy.Key == key {
			return policy, true
		}
	}
	return ProcessPropertyPolicy{}, false
}

func processPropertyBindingCount(value motionIR, policy ProcessPropertyPolicy) int {
	motion, ok := value.(*canonicalMotionIR)
	if !ok || motion == nil {
		return 0
	}
	count := 0
	for _, binding := range policy.SlotBindings {
		switch binding.Stage {
		case "trajectory":
			variant, field := "", ""
			switch motion.trajectory.(type) {
			case linearTrajectoryIR:
				variant, field = "linear", "speed"
			case pathTrajectoryIR:
				variant, field = "path", "speed"
			case orbitTrajectoryIR:
				variant = "orbit"
				if binding.Field == "radius" || binding.Field == "angular_speed" {
					field = binding.Field
				}
			case parabolaTrajectoryIR:
				variant = "parabola"
				if binding.Field == "height" || binding.Field == "speed" {
					field = binding.Field
				}
			}
			if binding.Variant == variant && binding.Field == field {
				count++
			}
		case "steering":
			if _, tracking := motion.steering.(trackingSteeringIR); tracking && binding.Variant == "tracking" && binding.Field == "turn_rate_mdeg_per_tick" {
				count++
			}
		case "offset":
			for _, offset := range motion.offsets {
				switch offset.(type) {
				case zigzagOffsetIR:
					if binding.Variant == "zigzag" && binding.Field == "amplitude" {
						count++
					}
				case circularOffsetIR:
					if binding.Variant == "circular" && (binding.Field == "radius" || binding.Field == "angular_speed") {
						count++
					}
				}
			}
		case "completion":
			if _, boomerang := motion.completion.(boomerangCompletionIR); boomerang && binding.Variant == "boomerang" && binding.Field == "return_speed_bp" {
				count++
			}
		case "collision":
			if motion.collision != nil && binding.Variant == "present" && binding.Field == "force" {
				count++
			}
		}
	}
	return count
}

func (c *typeChecker) expect(value valueIR, scope typeScope, expected valueType) valueType {
	actual := c.infer(value, scope, &expected)
	if actual.Base != valueKindInvalid && expected.Base != valueKindInvalid && actual.Base != expected.Base {
		c.context.addDiagnostic(DiagnosticTypeMismatch, value.sourceRef().Path, fmt.Sprintf("value has type %d, expected %d", actual.Base, expected.Base))
		return actual
	}
	if actual.Base == valueKindInt && !quantitiesCompatible(actual.Quantity, expected.Quantity) {
		c.context.addDiagnostic(DiagnosticQuantityMismatch, value.sourceRef().Path, "numeric quantity is incompatible with its use")
	}
	if !optionalCompatible(actual, expected) && !isMemoryReference(value) {
		c.context.addDiagnostic(DiagnosticOptionalInvalid, value.sourceRef().Path, "optional value requires an exists guard")
	}
	return actual
}

func isMemoryReference(value valueIR) bool {
	reference, ok := value.(*referenceValueIR)
	return ok && strings.HasPrefix(reference.reference, "$memory.")
}

func (c *typeChecker) infer(value valueIR, scope typeScope, expected *valueType) valueType {
	if value == nil {
		return valueType{Base: valueKindInvalid}
	}
	var result valueType
	switch typed := value.(type) {
	case *nullValueIR:
		if expected != nil {
			result = *expected
			result.Optional = true
		} else {
			result = typed.valueType()
		}
	case *intValueIR:
		result = typed.valueType()
		if expected != nil && expected.Base == valueKindInt && result.Quantity == quantityUnknown {
			result.Quantity = expected.Quantity
			typed.quantity = expected.Quantity
		}
		if result.Quantity == quantityUnknown {
			result.Quantity = quantityDimensionless
			typed.quantity = quantityDimensionless
		}
	case *boolValueIR, *stringValueIR:
		result = value.valueType()
	case *referenceValueIR:
		result = c.referenceType(typed, scope)
		typed.resolvedType = result
	case *attributeReadValueIR:
		c.expect(typed.entity, scope, valueType{Base: valueKindEntity})
		if attribute, found := c.attributes[typed.attribute]; found {
			result = valueType{Base: attribute.ValueType, Quantity: attribute.Quantity}
		} else {
			result = valueType{Base: valueKindInvalid}
		}
		typed.resolvedType = result
	case *stateReadValueIR:
		c.expectStateBinding(typed.owner, typed.subject, typed.teamOf, scope)
		result = typed.resolvedType
	case *abilityStateReadValueIR:
		c.expect(typed.owner, scope, valueType{Base: valueKindEntity})
		c.expect(typed.ability, scope, valueType{Base: valueKindAbility})
		result = typed.resolvedType
	case *expressionValueIR:
		result = c.expressionType(typed, scope, expected)
		typed.resolvedType = result
	default:
		result = valueType{Base: valueKindInvalid}
	}
	c.context.artifacts.types.types[value.sourceRef().Path] = result
	return result
}

func (c *typeChecker) expectStateBinding(owner, subject, teamOf valueIR, scope typeScope) {
	entity := valueType{Base: valueKindEntity}
	if owner != nil {
		c.expect(owner, scope, entity)
	}
	if subject != nil {
		c.expect(subject, scope, entity)
	}
	if teamOf != nil {
		c.expect(teamOf, scope, entity)
	}
}

func (c *typeChecker) referenceType(reference *referenceValueIR, scope typeScope) valueType {
	if typ, found := scope[reference.reference]; found {
		return typ
	}
	if typ, field, found := projectedReferenceType(reference.reference, scope); found {
		reference.resultField = field
		return typ
	}
	code := DiagnosticReferenceUnknown
	if strings.HasPrefix(reference.reference, "$input.") {
		code = DiagnosticInputUnavailable
	}
	c.context.addDiagnostic(code, reference.source.Path, fmt.Sprintf("reference %q is not visible in this scope", reference.reference))
	return valueType{Base: valueKindInvalid}
}

func projectedReferenceType(reference string, scope typeScope) (valueType, ResultFieldHandle, bool) {
	rootName := ""
	rootType := valueType{}
	for name, root := range scope {
		if strings.HasPrefix(reference, name+".") && len(name) > len(rootName) {
			rootName = name
			rootType = root
		}
	}
	if rootName == "" {
		return valueType{}, 0, false
	}
	field := strings.TrimPrefix(reference, rootName+".")
	result := valueType{Base: valueKindInvalid, Optional: rootType.Optional}
	switch rootType.Base {
	case valueKindEntity:
		if field == "position" {
			result.Base = valueKindPosition
		}
	case valueKindHit:
		switch field {
		case "entity", "target":
			result.Base = valueKindEntity
		case "position":
			result.Base = valueKindPosition
		}
	case valueKindEffectResult:
		dynamic := valueType{Base: rootType.ResultValueBase, Quantity: rootType.ResultValueQuantity, Optional: rootType.ResultValueOptional}
		layout := resultLayoutByType(rootType.Result, dynamic)
		if projected, found := layout.field(field, rootType.Outcome); found {
			projected.typ.Optional = projected.typ.Optional || rootType.Optional
			return projected.typ, projected.handle, true
		}
	}
	return result, 0, result.Base != valueKindInvalid
}

func effectResultReferenceType(layout resultLayoutProgram, outcome resultOutcomeScope) valueType {
	result := valueType{Base: valueKindEffectResult, Result: layout.typ, Outcome: outcome}
	for _, field := range layout.fields {
		if field.name == "before" || field.name == "after" {
			result.ResultValueBase = field.typ.Base
			result.ResultValueQuantity = field.typ.Quantity
			result.ResultValueOptional = field.typ.Optional
			break
		}
	}
	return result
}

func (c *typeChecker) expressionType(expression *expressionValueIR, scope typeScope, expected *valueType) valueType {
	args := expression.args
	badArity := func(want int) bool {
		if len(args) == want {
			return false
		}
		c.context.addDiagnostic(DiagnosticTypeMismatch, expression.source.Path, fmt.Sprintf("%s expects %d arguments", expression.op, want))
		return true
	}
	switch expression.op {
	case "exists":
		if badArity(1) {
			return valueType{Base: valueKindBool}
		}
		argument := c.infer(args[0], scope, nil)
		if !argument.Optional {
			c.context.addDiagnostic(DiagnosticOptionalInvalid, args[0].sourceRef().Path, "exists requires an optional value")
		}
		return valueType{Base: valueKindBool}
	case "and", "or":
		if badArity(2) {
			return valueType{Base: valueKindBool}
		}
		c.expect(args[0], scope, valueType{Base: valueKindBool})
		c.expect(args[1], scope, valueType{Base: valueKindBool})
		return valueType{Base: valueKindBool}
	case "not":
		if badArity(1) {
			return valueType{Base: valueKindBool}
		}
		c.expect(args[0], scope, valueType{Base: valueKindBool})
		return valueType{Base: valueKindBool}
	case "eq", "ne", "lt", "lte", "gt", "gte":
		if badArity(2) {
			return valueType{Base: valueKindBool}
		}
		left := c.infer(args[0], scope, nil)
		if left.Optional {
			c.context.addDiagnostic(DiagnosticOptionalInvalid, args[0].sourceRef().Path, "optional value requires an exists guard")
		}
		c.expect(args[1], scope, withoutOptional(left))
		if expression.op == "eq" || expression.op == "ne" {
			c.validateExpectedFailureLiteral(args[0], args[1], scope)
			c.validateExpectedFailureLiteral(args[1], args[0], scope)
		}
		return valueType{Base: valueKindBool}
	case "scale_bp":
		if badArity(2) {
			return valueType{Base: valueKindInvalid}
		}
		left := c.infer(args[0], scope, expected)
		c.expect(args[1], scope, quantityType(quantityBasisPoints))
		return left
	case "add", "sub", "mul", "div", "min", "max":
		if badArity(2) {
			return valueType{Base: valueKindInvalid}
		}
		left := c.infer(args[0], scope, expected)
		c.expect(args[1], scope, withoutOptional(left))
		return left
	case "clamp":
		if badArity(3) {
			return valueType{Base: valueKindInvalid}
		}
		valueType := c.infer(args[0], scope, expected)
		c.expect(args[1], scope, withoutOptional(valueType))
		c.expect(args[2], scope, withoutOptional(valueType))
		return valueType
	default:
		c.context.addDiagnostic(DiagnosticTypeMismatch, expression.source.Path, fmt.Sprintf("unsupported expression %q", expression.op))
		return valueType{Base: valueKindInvalid}
	}
}

func (c *typeChecker) validateExpectedFailureLiteral(referenceValue, literalValue valueIR, scope typeScope) {
	reference, isReference := referenceValue.(*referenceValueIR)
	literal, isLiteral := literalValue.(*stringValueIR)
	if !isReference || !isLiteral || !strings.HasSuffix(reference.reference, ".failure_reason") {
		return
	}
	root := strings.TrimSuffix(reference.reference, ".failure_reason")
	rootType, found := scope[root]
	if !found || rootType.Base != valueKindEffectResult {
		return
	}
	dynamic := valueType{Base: rootType.ResultValueBase, Quantity: rootType.ResultValueQuantity, Optional: rootType.ResultValueOptional}
	layout := resultLayoutByType(rootType.Result, dynamic)
	reason := ExpectedFailureReason(literal.value)
	if reason != ExpectedFailureNone && !layout.allows(reason) {
		c.context.addDiagnostic(DiagnosticShapeInvalid, literal.source.Path, fmt.Sprintf("failure reason %q is not valid for %s", literal.value, layout.typ))
	}
}
