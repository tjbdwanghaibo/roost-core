package skill

import (
	"fmt"
	"sort"
	"strings"
)

// typeScope 是一个值位点可见的名字与类型，连同位点所在的求值上下文（eval_contexts.go）。
// 名字集合由 scopeFor 按求值上下文表生成，流程再往里加局部变量。
type typeScope struct {
	context evalContext
	values  map[string]valueType
}

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
		}
		checker.memory[name] = typ
	}
	// memory 默认值在 Activate 里按槽位顺序求值，读不到别的 memory（表的 memory_default 列）。
	// 此前在填 checker.memory 的同一个 map 循环里按施法作用域检查，能不能看见另一个 memory
	// 取决于 map 遍历顺序；看见了就编译通过，槽位排在后面时每次 Activate 类型不匹配
	// （RR-20261005-NC-280）。
	memoryDefaultScope := checker.scopeFor(evalMemoryDefault, "")
	for _, name := range sortedMemoryNames(context.artifacts.ir.memory) {
		declaration := context.artifacts.ir.memory[name]
		if _, nullable := declaration.defaultValue.(*nullValueIR); !nullable {
			checker.expect(declaration.defaultValue, memoryDefaultScope, checker.memory[name])
		}
	}
	for _, name := range sortedStateNames(context.artifacts.ir.persistentState) {
		declaration := context.artifacts.ir.persistentState[name]
		typ := declaredStateType(declaration.declaredType)
		if _, nullable := declaration.defaultValue.(*nullValueIR); nullable {
			if !typ.Optional {
				context.addDiagnostic(DiagnosticTypeMismatch, declaration.source.Path+".default", "state type does not allow null default")
			}
		} else {
			// 状态默认值在读 / 写处求值，那里可能是衍生物回调或衍生物字段（表的 state_default 列，
			// RR-20261005-NC-281）。
			checker.expect(declaration.defaultValue, checker.scopeFor(evalStateDefault, ""), typ)
		}
		checker.state[name] = typ
	}

	root := checker.scopeFor(evalCastFlow, "")
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

// scopeFor 按求值上下文表生成一个上下文的作用域：表里在该上下文可用、
// 且在这个 Program 形状里存在（施法模式、area 衍生物）的行。输入槽位来自输入布局，memory
// 来自声明；局部变量由流程加入。spawnKind 只对衍生物回调有意义（area 专有的事件字段）。
func (c *typeChecker) scopeFor(context evalContext, spawnKind string) typeScope {
	scope := typeScope{context: context, values: make(map[string]valueType)}
	mode := c.context.artifacts.ir.activation.policy.mode
	for index, row := range evalReferenceTable {
		if row.name == "" || !row.cells[context].usable() || !row.presentIn(mode, spawnKind) {
			continue
		}
		switch evalReferenceRowIndex(index) {
		case evalRowInput:
			for name, typ := range c.context.artifacts.input.Slots {
				scope.values[name] = typ
			}
		case evalRowMemory:
			for name, typ := range c.memory {
				scope.values["$memory."+name] = typ
			}
		case evalRowLocal:
		default:
			scope.values[row.name] = row.typ
		}
	}
	return scope
}

func cloneTypeScope(scope typeScope) typeScope {
	copy := typeScope{context: scope.context, values: make(map[string]valueType, len(scope.values)+1)}
	for name, typ := range scope.values {
		copy.values[name] = typ
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
			if current, found := thenScope.values[reference]; found {
				thenScope.values[reference] = withoutOptional(current)
			}
		}
		c.flow(typed.thenFlow, thenScope)
		c.flow(typed.elseFlow, cloneTypeScope(scope))
	case *repeatFlowIR:
		c.expect(typed.times, scope, quantityType(quantityCount))
		child := cloneTypeScope(scope)
		child.values["$local."+typed.index.Name] = quantityType(quantityCount)
		c.flow(typed.body, child)
	case *waitFlowIR:
		c.flow(typed.then, cloneTypeScope(scope))
	case *selectFlowIR:
		c.selectPlan(&typed.selectPlan, scope)
		element := selectionValueType(typed.selectPlan.elementType)
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			child := cloneTypeScope(scope)
			child.values["$local."+consume.local.Name] = element
			c.flow(consume.then, child)
		case *selectEachConsumeIR:
			child := cloneTypeScope(scope)
			child.values["$local."+consume.local.Name] = element
			c.flow(consume.body, child)
		}
		c.flow(typed.onEmpty, cloneTypeScope(scope))
	case *effectFlowIR:
		c.effect(typed.effect, scope)
		c.spawn(typed.spawn, scope)
		if typed.result != nil {
			successScope, failureScope := cloneTypeScope(scope), cloneTypeScope(scope)
			if typed.result.local != nil {
				successScope.values["$local."+typed.result.local.Name] = effectResultReferenceType(typed.result.layout, resultOutcomeSuccess)
				failureScope.values["$local."+typed.result.local.Name] = effectResultReferenceType(typed.result.layout, resultOutcomeFailure)
			}
			c.flow(typed.result.success, successScope)
			c.flow(typed.result.failure, failureScope)
		}
		if typed.callbacks != nil {
			spawnKind := ""
			if typed.spawn != nil {
				spawnKind = typed.spawn.kind
			}
			// 回调的作用域只来自表的 spawn_callback 列：施法的输入、memory、局部变量与
			// `$caster` / `$cast.*` 都不在里面（此前 owned entity pass 另写一份 detachedReferenceAllowed）。
			callbackScope := c.scopeFor(evalSpawnCallback, spawnKind)
			if typed.spawn != nil {
				for _, policy := range c.context.environment.SpawnProperties.Properties {
					if containsString(policy.SpawnKinds, typed.spawn.kind) && spawnPropertyBindingCount(typed.spawn.motion, policy) == 1 {
						callbackScope.values["#spawn_property:"+policy.Key] = valueType{Base: valueKindInt}
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

// spawn 检查 spawn 衍生物的字段。每一步重新求值的字段（area 选择、motion 的目标 / 点 / 锚点 /
// 目的地、未绑定到衍生物数值属性的数值字段）用表的 spawn_step 列；numeric track 的值与绑定到
// 数值属性的字段只在启动时用施法求一次（initializeSpawnNumeric），用施法作用域 castScope。
// 此前全部按施法作用域检查，再由 owned entity pass 另写一份前缀黑名单（RR-20261005-NC-224）。
func (c *typeChecker) spawn(spawn *spawnIR, castScope typeScope) {
	if spawn == nil {
		return
	}
	stepScope := c.scopeFor(evalSpawnStep, "")
	numeric := func(stage, variant, field string) typeScope {
		if spawnNumericFieldBound(c.context.environment, spawn.kind, stage, variant, field) {
			return castScope
		}
		return stepScope
	}
	scope := stepScope
	if spawn.area != nil {
		c.selectPlan(spawn.area, scope)
	}
	seen := make(map[string]bool)
	for _, track := range spawn.numericTracks {
		path := track.source.Path
		policy, found := lookupSpawnPropertyPolicy(c.context.environment.SpawnProperties, track.property)
		if !found {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "property is not mutable in the spawn property catalog")
		} else {
			if !containsString(policy.SpawnKinds, spawn.kind) || spawnPropertyBindingCount(spawn.motion, policy) != 1 {
				c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "property must bind exactly one Motion slot for this spawn")
			}
			if !containsString(policy.Operations, track.operation) {
				c.context.addDiagnostic(DiagnosticShapeInvalid, path+".operation", "operation is not allowed by the spawn property policy")
			}
		}
		if seen[track.property] {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".property", "initial numeric property must not be duplicated")
		}
		seen[track.property] = true
		if track.overTicks < 0 {
			c.context.addDiagnostic(DiagnosticShapeInvalid, path+".over_ticks", "over_ticks must be non-negative")
		}
		c.expect(track.value, castScope, valueType{Base: valueKindInt})
	}
	if spawn.motion == nil {
		return
	}
	motion, ok := spawn.motion.(*canonicalMotionIR)
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
		c.expect(trajectory.speed, numeric("trajectory", "linear", "speed"), distance)
	case pathTrajectoryIR:
		c.expect(trajectory.points, scope, valueType{Base: valueKindPath})
		c.expect(trajectory.speed, numeric("trajectory", "path", "speed"), distance)
	case orbitTrajectoryIR:
		c.expect(trajectory.anchor, scope, entity)
		c.expect(trajectory.radius, numeric("trajectory", "orbit", "radius"), distance)
		c.expect(trajectory.angularSpeed, numeric("trajectory", "orbit", "angular_speed"), angle)
	case parabolaTrajectoryIR:
		c.expect(trajectory.destination, scope, position)
		c.expect(trajectory.height, numeric("trajectory", "parabola", "height"), distance)
	}
	for _, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagOffsetIR:
			c.expect(typed.amplitude, numeric("offset", "zigzag", "amplitude"), distance)
		case circularOffsetIR:
			c.expect(typed.radius, numeric("offset", "circular", "radius"), distance)
			c.expect(typed.angularSpeed, numeric("offset", "circular", "angular_speed"), angle)
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
		if memoryType, found := c.declaredMemory(typed.name, typed.source.Path); found {
			c.expect(typed.value, scope, withoutOptional(memoryType))
		}
	case *addMemoryEffectIR:
		if memoryType, found := c.declaredMemory(typed.name, typed.source.Path); found {
			if memoryType.Base != valueKindInt {
				c.context.addDiagnostic(DiagnosticTypeMismatch, typed.source.Path+".name", "add_memory requires an int memory")
			} else {
				c.expect(typed.value, scope, withoutOptional(memoryType))
			}
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
	case *modifySpawnEffectIR:
		c.expect(typed.spawn, scope, valueType{Base: valueKindSpawn})
		spawnReference, isSpawnReference := typed.spawn.(*referenceValueIR)
		if !isSpawnReference || spawnReference.reference != "$spawn" {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".spawn", "modify_spawn requires the current callback $spawn")
		}
		policy, found := lookupSpawnPropertyPolicy(c.context.environment.SpawnProperties, typed.property)
		if !found || scope.values["#spawn_property:"+typed.property].Base == valueKindInvalid {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".property", "property is not bound to the current spawn Motion")
		} else if !containsString(policy.Operations, typed.operation) {
			c.context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".operation", "operation is not allowed by the spawn property policy")
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
		c.declaredMemory(typed.name, typed.source.Path)
	}
}

// declaredMemory 是 set / add / clear_memory 共用的名字检查（RR-20261005-NC-210）。
// lower 按名字取槽位时用的是 map 零值：此前未声明的名字在这里被直接跳过，lower 把它
// 落到槽位 0——有别的 memory 时静默改写那个槽位（值的类型也没查过），没有 memory 时
// executeMemory 返回 ErrProgramInvariant。
func (c *typeChecker) declaredMemory(name, effectPath string) (valueType, bool) {
	memoryType, found := c.memory[name]
	if !found {
		c.context.addDiagnostic(DiagnosticReferenceUnknown, effectPath+".name", "memory is not declared")
	}
	return memoryType, found
}

func lookupSpawnPropertyPolicy(catalog SpawnPropertyCatalog, key string) (SpawnPropertyPolicy, bool) {
	for _, policy := range catalog.Properties {
		if policy.Key == key {
			return policy, true
		}
	}
	return SpawnPropertyPolicy{}, false
}

func spawnPropertyBindingCount(value motionIR, policy SpawnPropertyPolicy) int {
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
		c.checkCachedRead(typed, scope)
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
	if typ, found := scope.values[reference.reference]; found {
		return typ
	}
	if typ, field, found := projectedReferenceType(reference.reference, scope); found {
		reference.resultField = field
		return typ
	}
	// 表里有这一行、但在这个求值上下文不可用：诊断指向表项，说明那里为什么没有。
	if row, _, known := evalReferenceRowFor(reference.reference); known {
		if entry := evalReferenceTable[row]; !entry.cells[scope.context].usable() {
			c.context.addDiagnostic(DiagnosticInputUnavailable, reference.source.Path, fmt.Sprintf("reference %q is not available in evaluation context %s: %s (evaluation context table row %s)", reference.reference, scope.context, entry.cells[scope.context].semantics, entry.name))
			return valueType{Base: valueKindInvalid}
		}
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
	for name, root := range scope.values {
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
	rootType, found := scope.values[root]
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

// checkCachedRead 按求值上下文表检查缓存型快照读取（cast_start / phase_start / spawn_start）：
// 这些读取的整个求值（包括实体）发生在采样点，之后读缓存（runtime_eval.go captureSnapshots）。
//   - 快照点在读取所在的上下文里要可用（evalSnapshotTable）：spawn_start 只在衍生物回调里，
//     cast_start / phase_start 不在衍生物回调里（RR-20261005-NC-220）；
//   - 实体里的每个引用在采样上下文里要可用（引用表的采样列：采样点上没有局部变量，
//     spawn_start 采样在衍生物上下文里），且不能是可缺省的——采样时没有读取处的 exists
//     守卫，缺省即 Activate / 进 phase 失败（RR-20261005-NC-282）。
//
// current / each_tick / on_hit / on_event 在读取处求值，不受这两条限制。
func (c *typeChecker) checkCachedRead(read *attributeReadValueIR, scope typeScope) {
	point := snapshotPoint(read.snapshot)
	captureContext, cached := snapshotCaptureContext(point)
	if !cached {
		return
	}
	if cell := evalSnapshotTable[point][scope.context]; !cell.usable() {
		c.context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.snapshot", fmt.Sprintf("a %s snapshot is not available in evaluation context %s: %s (evaluation context snapshot table row %s)", point, scope.context, cell.semantics, point))
		return
	}
	captureScope := c.scopeFor(captureContext, "")
	reported := false
	walkValue(read.entity, func(value valueIR) {
		reference, ok := value.(*referenceValueIR)
		if !ok || reported {
			return
		}
		typ, found := captureScope.values[reference.reference]
		if !found {
			typ, _, found = projectedReferenceType(reference.reference, captureScope)
		}
		switch {
		case !found:
			reason := "it is not visible there"
			if row, _, known := evalReferenceRowFor(reference.reference); known {
				reason = fmt.Sprintf("%s (evaluation context table row %s)", evalReferenceTable[row].cells[captureContext].semantics, evalReferenceTable[row].name)
			}
			c.context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.entity", fmt.Sprintf("a %s snapshot evaluates its entity in evaluation context %s; %s is not available there: %s; read it with snapshot current", point, captureContext, reference.reference, reason))
			reported = true
		case typ.Optional:
			c.context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_attribute.entity", fmt.Sprintf("a %s snapshot evaluates its entity in evaluation context %s, where %s may be missing and no exists guard applies; read it with snapshot current", point, captureContext, reference.reference))
			reported = true
		}
	})
}

func sortedMemoryNames(values map[string]memoryDeclarationIR) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
