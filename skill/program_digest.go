package skill

func gameplayProgramDigestPayload(program *Program) any {
	operations := make([]any, len(program.operations))
	for index, operation := range program.operations {
		operations[index] = operationDigestValue(operation)
	}
	selectors := make([]any, len(program.selectors))
	for index, selector := range program.selectors {
		shapeValues := make([]any, len(selector.shapePlan.values))
		for valueIndex, value := range selector.shapePlan.values {
			shapeValues[valueIndex] = programValueDigest(value)
		}
		filters := make([]any, len(selector.filters))
		for filterIndex, filter := range selector.filters {
			filters[filterIndex] = map[string]any{"kind": filter.kind, "relation": filter.relation, "status": filter.status, "attribute": filter.attribute, "operation": filter.operation, "value": programValueDigest(filter.value), "tag": filter.tag, "slot": filter.slot, "text": filter.text, "template": filter.template, "cast": filter.cast, "tick": filter.tick, "collision": filter.collision}
		}
		selectors[index] = map[string]any{
			"index": selector.index, "element": selector.element, "limit": selector.limit,
			"from": programValueDigest(selector.from), "shape": selector.shapePlan.kind,
			"shape_values": shapeValues, "collision": selector.shapePlan.collision,
			"max_targets": selector.shapePlan.maxTargets, "allow_repeat": selector.shapePlan.allowRepeat,
			"hop_interval_ticks": selector.shapePlan.hopIntervalTicks, "filters": filters,
			"order_by": selector.order.by, "order_direction": selector.order.direction,
			"consumer_local": selector.consumerLocal, "random_site": selector.randomSite, "has_random_site": selector.hasRandomSite,
			"consumer_mode": selector.consumerMode, "consumer_root": selector.consumerRoot,
			"has_consumer": selector.hasConsumer, "empty_root": selector.emptyRoot, "has_empty": selector.hasEmpty,
		}
	}
	phases := make([]any, len(program.phases))
	for index, phase := range program.phases {
		phases[index] = map[string]any{"index": phase.index, "timeout_ticks": phase.timeoutTicks, "roots": phase.roots}
	}
	roots := make([]any, len(program.roots))
	for index, root := range program.roots {
		roots[index] = map[string]any{"index": root.index, "phase": root.phase, "event": root.event, "operation": root.operation, "has_operation": root.hasOperation}
	}
	memory := make([]any, len(program.memory))
	for index, slot := range program.memory {
		memory[index] = map[string]any{"index": slot.index, "type": digestValueType(slot.typ), "default": programValueDigest(slot.defaultValue)}
	}
	states := make([]any, len(program.states))
	for index, state := range program.states {
		states[index] = map[string]any{
			"slot": state.slot, "type": digestValueType(state.typ), "scope": state.scope, "default": programValueDigest(state.defaultValue),
			"minimum": state.minimum, "maximum": state.maximum, "enum_values": state.enumValues,
			"duration_ticks": state.durationTicks, "maximum_duration_ticks": state.maximumDurationTicks,
			"on_write": state.onWrite, "clear_on": state.clearOn,
		}
	}
	abilityProperties := make([]any, len(program.abilityProperties))
	for index, property := range program.abilityProperties {
		abilityProperties[index] = map[string]any{"handle": property.handle, "name": property.name, "type": property.typ, "mutable": property.mutable, "minimum": property.minimum, "maximum": property.maximum, "maximum_mutation": property.maximumMutation, "maximum_duration": property.maximumDuration}
	}
	locals := make([]any, len(program.locals))
	for index, slot := range program.locals {
		locals[index] = map[string]any{"index": slot.index, "type": digestValueType(slot.typ)}
	}
	quantities := make([]any, len(program.quantities))
	for index, quantity := range program.quantities {
		quantities[index] = map[string]any{"type": digestValueType(quantity.typ), "minimum": quantity.minimum, "maximum": quantity.maximum, "proved": quantity.proved}
	}
	randomSites := make([]any, len(program.randomSites))
	for index, site := range program.randomSites {
		randomSites[index] = map[string]any{"index": site.index, "kind": site.kind, "invocation_bound": site.invocationBound}
	}
	inputSlots := make([]any, len(program.input.slots))
	for index, slot := range program.input.slots {
		inputSlots[index] = map[string]any{"name": slot.name, "type": digestValueType(slot.typ)}
	}
	costs := digestCosts(program.costs)
	sustainCosts := digestCosts(program.cast.sustainCosts)
	snapshots := make([]any, len(program.snapshots))
	for index, snapshot := range program.snapshots {
		snapshots[index] = map[string]any{"slot": snapshot.slot, "entity": programValueDigest(snapshot.entity), "attribute": snapshot.attribute, "point": snapshot.point}
	}
	processTemplates := make([]any, len(program.processTemplates))
	for index, template := range program.processTemplates {
		callbacks := make([]any, len(template.callbacks))
		for callbackIndex, callback := range template.callbacks {
			callbacks[callbackIndex] = map[string]any{"event": callback.event, "operation": callback.operation}
		}
		numericTracks := make([]any, len(template.numericTracks))
		for trackIndex, track := range template.numericTracks {
			numericTracks[trackIndex] = map[string]any{"property": track.property, "operation": track.operation, "value": programValueDigest(track.value), "over_ticks": track.overTicks}
		}
		processTemplates[index] = map[string]any{"index": template.index, "duration_ticks": template.durationTicks, "interval_ticks": template.intervalTicks, "emit_leave_on_stop": template.emitLeaveOnStop, "visual": template.visual, "has_visual": template.hasVisual, "area": selectorProgramDigest(template.area), "motion": motionProgramDigest(template.motion), "numeric_tracks": numericTracks, "callbacks": callbacks}
	}
	processProperties := make([]any, len(program.processProperties))
	for index, property := range program.processProperties {
		bindings := make([]any, len(property.slotBindings))
		for bindingIndex, binding := range property.slotBindings {
			bindings[bindingIndex] = map[string]any{"stage": binding.stage, "variant": binding.variant, "field": binding.field}
		}
		processProperties[index] = map[string]any{"handle": property.handle, "key": property.key, "minimum": property.minimum, "maximum": property.maximum, "interpolation": property.interpolation, "rounding": property.rounding, "operations": property.allowedOperationsMask, "process_kinds": property.processKinds, "slot_bindings": bindings}
	}
	eventPlans := make([]any, len(program.eventPlans))
	for index, plan := range program.eventPlans {
		eventPlans[index] = map[string]any{
			"required_tags": plan.filter.RequiredTags, "excluded_tags": plan.filter.ExcludedTags,
			"elements": plan.filter.Elements, "damage_types": plan.filter.DamageTypes, "results": plan.filter.Results,
			"max_depth": plan.proc.MaxDepth, "allow_self_trigger": plan.proc.AllowSelfTrigger,
			"once_per_root_event": plan.proc.OncePerRootEvent, "max_events_per_root": plan.proc.MaxEventsPerRoot,
		}
	}
	return map[string]any{
		"id": program.id, "compiler_semantics_revision": program.compilerSemanticsRevision,
		"authority": program.authority, "activation_kind": program.activationKind, "cooldown_scope": program.cooldownScope, "cooldown_ticks": program.cooldownTicks, "global_cooldown_ticks": program.globalCooldownTicks,
		"initial_phase": program.initialPhase, "gameplay_tags": program.gameplayTags,
		"cast": map[string]any{
			"windup_ticks": program.cast.windupTicks, "commit_tick": program.cast.commitTick, "recovery_ticks": program.cast.recoveryTicks,
			"has_windup_expression": program.cast.hasWindupExpression, "windup_expression": programValueDigest(program.cast.windupExpression),
			"windup_ticks_min": program.cast.windupTicksMin, "windup_ticks_max": program.cast.windupTicksMax,
			"has_recovery_expression": program.cast.hasRecoveryExpression, "recovery_expression": programValueDigest(program.cast.recoveryExpression),
			"recovery_ticks_min": program.cast.recoveryTicksMin, "recovery_ticks_max": program.cast.recoveryTicksMax,
			"movement": program.cast.movement, "turning": program.cast.turning, "interrupt_tags": program.cast.interruptTags,
			"refund_before_commit": program.cast.refundBeforeCommit, "concurrent": program.cast.concurrent,
			"mode": program.cast.mode, "pulse_interval_ticks": program.cast.pulseIntervalTicks, "max_duration_ticks": program.cast.maxDurationTicks,
			"max_charge_ticks": program.cast.maxChargeTicks, "min_charge_bp": program.cast.minChargeBP, "auto_release": program.cast.autoRelease,
			"max_stock": program.cast.maxStock, "recharge_ticks": program.cast.rechargeTicks, "initial_stock": program.cast.initialStock,
			"sustain_costs": sustainCosts,
		},
		"costs": costs, "input": map[string]any{
			"kind": program.input.kind, "slots": inputSlots,
			"maximum_range": program.input.maximumRange, "has_maximum_range": program.input.hasMaximumRange,
			"minimum_length": program.input.minimumLength, "maximum_length": program.input.maximumLength,
			"maximum_path_points": program.input.maximumPathPoints, "maximum_path_length": program.input.maximumPathLength,
			"minimum_segment_length": program.input.minimumSegmentLength, "clamp_policy": program.input.clampPolicy,
			"simplification_policy": program.input.simplificationPolicy, "update_ports": program.input.updatePorts,
		},
		"memory": memory, "persistent_state": states, "ability_properties": abilityProperties, "locals": locals, "phases": phases, "roots": roots,
		"ability_control": map[string]any{"selectable_tags": program.abilityControl.selectableTags, "owner_relations": program.abilityControl.ownerRelations},
		"operations":      operations, "selectors": selectors, "process_templates": processTemplates, "process_properties": processProperties,
		"snapshots": snapshots, "quantities": quantities, "random_sites": randomSites,
		"event_plans": eventPlans, "limits": program.limits,
	}
}

func selectorProgramDigest(selector *selectorProgram) any {
	if selector == nil {
		return nil
	}
	shapeValues := make([]any, len(selector.shapePlan.values))
	for index, value := range selector.shapePlan.values {
		shapeValues[index] = programValueDigest(value)
	}
	filters := make([]any, len(selector.filters))
	for index, filter := range selector.filters {
		filters[index] = map[string]any{"kind": filter.kind, "relation": filter.relation, "status": filter.status, "attribute": filter.attribute, "operation": filter.operation, "value": programValueDigest(filter.value), "tag": filter.tag, "collision": filter.collision}
	}
	return map[string]any{"element": selector.element, "limit": selector.limit, "from": programValueDigest(selector.from), "shape": selector.shapePlan.kind, "shape_values": shapeValues, "shape_collision": selector.shapePlan.collision, "filters": filters, "order_by": selector.order.by, "order_direction": selector.order.direction}
}

func motionProgramDigest(motion *motionProgram) any {
	if motion == nil {
		return nil
	}
	result := map[string]any{}
	switch frame := motion.frame.(type) {
	case worldMotionFrameProgram:
		result["frame"] = map[string]any{"kind": "world"}
	case followMotionFrameProgram:
		result["frame"] = map[string]any{"kind": "follow", "target": programValueDigest(frame.target)}
	default:
		panic("skill: unsupported motion frame in digest")
	}
	switch steering := motion.steering.(type) {
	case fixedMotionSteeringProgram:
		result["steering"] = map[string]any{"kind": "fixed"}
	case trackingMotionSteeringProgram:
		result["steering"] = map[string]any{"kind": "tracking", "target": programValueDigest(steering.target), "duration_ticks": steering.durationTicks}
	default:
		panic("skill: unsupported motion steering in digest")
	}
	switch trajectory := motion.trajectory.(type) {
	case stationaryMotionTrajectoryProgram:
		result["trajectory"] = map[string]any{"kind": "stationary"}
	case linearMotionTrajectoryProgram:
		result["trajectory"] = map[string]any{"kind": "linear", "speed": programValueDigest(trajectory.speed)}
	case pathMotionTrajectoryProgram:
		result["trajectory"] = map[string]any{"kind": "path", "points": programValueDigest(trajectory.points), "speed": programValueDigest(trajectory.speed)}
	case orbitMotionTrajectoryProgram:
		result["trajectory"] = map[string]any{"kind": "orbit", "anchor": programValueDigest(trajectory.anchor), "radius": programValueDigest(trajectory.radius), "angular_speed": programValueDigest(trajectory.angularSpeed)}
	case parabolaMotionTrajectoryProgram:
		result["trajectory"] = map[string]any{"kind": "parabola", "destination": programValueDigest(trajectory.destination), "height": programValueDigest(trajectory.height), "duration_ticks": trajectory.durationTicks}
	default:
		panic("skill: unsupported motion trajectory in digest")
	}
	offsets := make([]any, len(motion.offsets))
	for index, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagMotionOffsetProgram:
			offsets[index] = map[string]any{"kind": "zigzag", "amplitude": programValueDigest(typed.amplitude), "period_ticks": typed.periodTicks}
		case circularMotionOffsetProgram:
			offsets[index] = map[string]any{"kind": "circular", "radius": programValueDigest(typed.radius), "angular_speed": programValueDigest(typed.angularSpeed)}
		default:
			panic("skill: unsupported motion offset in digest")
		}
	}
	result["offsets"] = offsets
	if motion.collision != nil {
		result["collision"] = map[string]any{"layers": append([]CollisionLayerHandle(nil), motion.collision.layers...), "response": motion.collision.response, "max_reflects": motion.collision.maxReflects, "max_pierces": motion.collision.maxPierces}
	}
	if motion.carry != nil {
		result["carry"] = map[string]any{"target": programValueDigest(motion.carry.target)}
	}
	switch completion := motion.completion.(type) {
	case endMotionCompletionProgram:
		result["completion"] = map[string]any{"kind": "end"}
	case pauseThenEndMotionCompletionProgram:
		result["completion"] = map[string]any{"kind": "pause_then_end", "pause_ticks": completion.pauseTicks}
	case boomerangMotionCompletionProgram:
		result["completion"] = map[string]any{"kind": "boomerang", "max_return_ticks": completion.maxReturnTicks}
	default:
		panic("skill: unsupported motion completion in digest")
	}
	return result
}

func digestCosts(costs []costProgram) []any {
	result := make([]any, len(costs))
	for index, cost := range costs {
		result[index] = map[string]any{"resource": cost.resource, "amount": programValueDigest(cost.amount)}
	}
	return result
}

func operationDigestValue(operation operation) any {
	base := map[string]any{"index": operation.header().index, "kind": operationKind(operation)}
	switch typed := operation.(type) {
	case sequenceOperation:
		base["children"] = typed.children
	case parallelOperation:
		base["branches"] = typed.branches
	case branchOperation:
		base["condition"], base["then"], base["else"], base["has_else"] = programValueDigest(typed.condition), typed.thenOperation, typed.elseOperation, typed.hasElse
	case repeatOperation:
		base["times"], base["interval_ticks"], base["local"], base["body"] = programValueDigest(typed.times), typed.intervalTicks, typed.indexLocal, typed.body
	case waitOperation:
		base["ticks"], base["then"] = typed.ticks, typed.then
	case queryOperation:
		base["selector"] = typed.selector
	case captureSnapshotOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["profile"] = programValueDigest(typed.target), typed.profile
	case restoreSnapshotOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["snapshot"], base["on_blocked"] = programValueDigest(typed.target), programValueDigest(typed.snapshot), typed.onBlocked
	case damageOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["amount"], base["damage_type"], base["element"], base["combat_tags"], base["can_critical"] = programValueDigest(typed.target), programValueDigest(typed.amount), typed.damageType, typed.element, typed.combatTags, typed.canCritical
	case healOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["amount"] = programValueDigest(typed.target), programValueDigest(typed.amount)
	case shieldOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["amount"], base["duration_ticks"] = programValueDigest(typed.target), programValueDigest(typed.amount), typed.durationTicks
	case statusOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["status"], base["remove"], base["duration_ticks"], base["stacks"], base["max_stacks"] = programValueDigest(typed.target), typed.status, typed.remove, typed.durationTicks, typed.stacks, typed.maxStacks
	case modifyStatusInstanceOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["status"], base["operation"], base["value"], base["has_value"] = programValueDigest(typed.status), typed.operation, programValueDigest(typed.value), typed.hasValue
		base["target"], base["has_target"], base["ownership_policy"] = programValueDigest(typed.target), typed.hasTarget, typed.ownershipPolicy
	case attributeModifierOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["attribute"], base["operation"], base["value"], base["duration_ticks"], base["stack_policy"], base["max_stacks"] = programValueDigest(typed.target), typed.attribute, typed.operation, programValueDigest(typed.value), typed.durationTicks, typed.stackPolicy, typed.maxStacks
	case resourceOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["amount"], base["resource"], base["operation"] = programValueDigest(typed.target), programValueDigest(typed.amount), typed.resource, typed.operation
	case memoryOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["memory"], base["operation"], base["value"] = typed.memory, typed.operation, programValueDigest(typed.value)
	case stateOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["state"], base["binding"] = stateReferenceDigest(typed.state), stateBindingDigest(typed.binding)
		base["operation"], base["value"], base["has_value"], base["duration_ticks"], base["expiry_policy"] = typed.operation, programValueDigest(typed.value), typed.hasValue, typed.durationTicks, typed.expiryPolicy
	case abilityStateOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["owner"], base["ability"], base["property"], base["property_handle"] = programValueDigest(typed.owner), programValueDigest(typed.ability), typed.propertyName, typed.property
		base["operation"], base["value"], base["duration_ticks"] = typed.operation, programValueDigest(typed.value), typed.durationTicks
	case modifyProcessOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["process"], base["property"], base["operation"], base["value"], base["over_ticks"] = programValueDigest(typed.process), typed.property, typed.operation, programValueDigest(typed.value), typed.overTicks
	case spawnOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["template"], base["position"], base["count"], base["duration_ticks"] = typed.template, programValueDigest(typed.position), typed.count, typed.durationTicks
		overrides := make([]any, len(typed.attributeOverrides))
		for index, override := range typed.attributeOverrides {
			overrides[index] = map[string]any{"attribute": override.attribute, "value": programValueDigest(override.value)}
		}
		parameters := make([]any, len(typed.parameterBindings))
		for index, binding := range typed.parameterBindings {
			parameters[index] = map[string]any{"name": binding.name, "value": programValueDigest(binding.value)}
		}
		base["attribute_overrides"], base["parameter_bindings"] = overrides, parameters
	case entityCommandOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["command"], base["position"], base["has_position"] = programValueDigest(typed.target), typed.command, programValueDigest(typed.position), typed.hasPosition
		base["target_entity"], base["has_target_entity"], base["behavior"] = programValueDigest(typed.targetEntity), typed.hasTargetEntity, typed.behavior
	case teleportOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"], base["destination"], base["on_blocked"] = programValueDigest(typed.target), programValueDigest(typed.destination), typed.onBlocked
	case motionImpulseOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["motion_kind"], base["target"], base["origin"], base["distance"] = typed.kind, programValueDigest(typed.target), programValueDigest(typed.origin), programValueDigest(typed.distance)
	case stopMovementOperation:
		addEffectDigest(base, typed.effectIndex, typed.effectContinuations)
		base["target"] = programValueDigest(typed.target)
	case gotoOperation:
		base["phase"] = typed.phase
	case finishOperation:
		base["reason"] = typed.reason
	}
	return base
}

func addEffectDigest(target map[string]any, index EffectIndex, continuations effectContinuations) {
	target["effect_index"] = index
	target["success"], target["failure"] = continuations.success, continuations.failure
	target["has_success"], target["has_failure"] = continuations.hasSuccess, continuations.hasFailure
	target["process_template"], target["has_process"] = continuations.processTemplate, continuations.hasProcess
	fields := make([]any, len(continuations.result.fields))
	for index, field := range continuations.result.fields {
		fields[index] = map[string]any{"handle": field.handle, "name": field.name, "type": digestValueType(field.typ), "visibility": field.visibility}
	}
	target["result"] = map[string]any{"type": continuations.result.typ, "fields": fields, "allowed_failures": continuations.result.allowedFailures, "local": continuations.resultLocal, "has_local": continuations.hasResultLocal}
}

func programValueDigest(value programValue) any {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case nullProgramValue:
		return map[string]any{"kind": "null", "type": digestValueType(typed.typ)}
	case intProgramValue:
		return map[string]any{"kind": "int", "value": typed.value, "type": digestValueType(typed.typ)}
	case boolProgramValue:
		return map[string]any{"kind": "bool", "value": typed.value}
	case stringProgramValue:
		return map[string]any{"kind": "string", "value": typed.value}
	case referenceProgramValue:
		return map[string]any{"kind": "reference", "reference_kind": typed.kind, "builtin": typed.builtin, "index": typed.index, "field": typed.field, "result_field": typed.resultField, "type": digestValueType(typed.typ)}
	case expressionProgramValue:
		args := make([]any, len(typed.args))
		for index, argument := range typed.args {
			args[index] = programValueDigest(argument)
		}
		return map[string]any{"kind": "expression", "op": typed.op, "args": args, "type": digestValueType(typed.typ)}
	case attributeReadProgramValue:
		return map[string]any{"kind": "attribute_read", "entity": programValueDigest(typed.entity), "attribute": typed.attribute, "snapshot": typed.snapshot, "snapshot_slot": typed.snapshotSlot, "type": digestValueType(typed.typ)}
	case stateReadProgramValue:
		return map[string]any{"kind": "state_read", "state": stateReferenceDigest(typed.state), "binding": stateBindingDigest(typed.binding), "snapshot": typed.snapshot, "type": digestValueType(typed.typ)}
	case abilityStateReadProgramValue:
		return map[string]any{"kind": "ability_state_read", "owner": programValueDigest(typed.owner), "ability": programValueDigest(typed.ability), "property": typed.name, "property_handle": typed.property, "snapshot": typed.snapshot, "type": digestValueType(typed.typ)}
	default:
		panic("skill: unsupported program value in digest")
	}
}

func stateReferenceDigest(state stateReferenceProgram) any {
	return map[string]any{"shared": state.shared, "slot": state.slot, "type": digestValueType(state.typ), "scope": state.scope, "default": programValueDigest(state.defaultValue), "minimum": state.minimum, "maximum": state.maximum, "duration_ticks": state.durationTicks, "maximum_duration_ticks": state.maximumDurationTicks, "on_write": state.onWrite, "clear_on": state.clearOn}
}

func stateBindingDigest(binding stateBindingProgram) any {
	return map[string]any{"owner": programValueDigest(binding.owner), "subject": programValueDigest(binding.subject), "team_of": programValueDigest(binding.teamOf), "has_owner": binding.hasOwner, "has_subject": binding.hasSubject, "has_team_of": binding.hasTeamOf}
}

func digestValueType(typ valueType) any {
	return map[string]any{"base": typ.Base, "optional": typ.Optional, "quantity": typ.Quantity, "result": typ.Result, "outcome": typ.Outcome, "result_value_base": typ.ResultValueBase, "result_value_quantity": typ.ResultValueQuantity, "result_value_optional": typ.ResultValueOptional}
}
