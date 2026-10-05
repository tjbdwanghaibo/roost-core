package skill

import "strings"

func runOwnedEntityPass(context *compileContext) {
	if context.artifacts.ir == nil {
		return
	}
	templates := make(map[string]UnitTemplateCatalogEntry, len(context.environment.Gameplay.UnitTemplates.Entries))
	for _, template := range context.environment.Gameplay.UnitTemplates.Entries {
		templates[template.Key] = template
	}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		switch typed := flow.(type) {
		case *effectFlowIR:
			switch effect := typed.effect.(type) {
			case *spawnEffectIR:
				template, found := templates[effect.template]
				if !found {
					context.addDiagnostic(DiagnosticCapabilityUnknown, effect.source.Path+".template", "unknown unit template")
					return
				}
				if effect.count <= 0 || effect.count > template.MaximumSpawnCount || effect.count > context.environment.Limits.MaxOwnedEntities {
					context.addDiagnostic(DiagnosticBudgetExceeded, effect.source.Path+".count", "spawn count exceeds the unit template or environment maximum")
				}
				if effect.durationTicks <= 0 || effect.durationTicks > template.MaximumLifetimeTicks || effect.durationTicks > context.environment.Limits.MaxLifetimeTicks {
					context.addDiagnostic(DiagnosticBudgetExceeded, effect.source.Path+".duration_ticks", "spawn lifetime exceeds the unit template or environment maximum")
				}
				if typed.callbacks != nil {
					allowAreaFinish := typed.process != nil && typed.process.kind == "area"
					validateDetachedCallbacks(context, typed.callbacks, allowAreaFinish)
				}
				if typed.process != nil {
					validateDetachedProcessFields(context, typed.process)
				}
				validateSpawnBindings(context, effect, template)
			case *entityCommandEffectIR:
				validateOwnedEntityCommand(context, effect)
			}
		case *selectFlowIR:
			validateOwnedEntitySelect(context, typed)
		}
	})
}

func validateSpawnBindings(context *compileContext, effect *spawnEffectIR, template UnitTemplateCatalogEntry) {
	allowedOverrides := make(map[AttributeHandle]bool, len(template.AllowedAttributeOverrides))
	for _, policy := range template.AllowedAttributeOverrides {
		allowedOverrides[policy.Attribute] = true
	}
	for _, override := range effect.attributeOverrides {
		handle, found := context.artifacts.authority.attributes[override.attribute]
		if !found || !allowedOverrides[handle] {
			context.addDiagnostic(DiagnosticCapabilityUnknown, override.value.sourceRef().Path, "attribute override is not allowed by the unit template")
		}
	}
	allowedParameters := make(map[string]bool, len(template.Parameters))
	for _, policy := range template.Parameters {
		allowedParameters[policy.Name] = true
	}
	bound := make(map[string]bool, len(effect.parameterBindings))
	for _, binding := range effect.parameterBindings {
		bound[binding.name] = true
		if !allowedParameters[binding.name] {
			context.addDiagnostic(DiagnosticCapabilityUnknown, binding.value.sourceRef().Path, "parameter binding is not declared by the unit template")
		}
	}
	hasStart, hasEnd := bound["start_position"], bound["end_position"]
	if hasStart != hasEnd || (hasStart && !template.DynamicCollider) {
		context.addDiagnostic(DiagnosticCapabilityUnknown, effect.source.Path+".parameter_bindings", "two-point bindings require start_position/end_position and DynamicCollider capability")
	}
}

func unitTemplateEntry(context *compileContext, key string) (UnitTemplateCatalogEntry, bool) {
	for _, template := range context.environment.Gameplay.UnitTemplates.Entries {
		if template.Key == key {
			return template, true
		}
	}
	return UnitTemplateCatalogEntry{}, false
}

func unitTemplateOverrideType(context *compileContext, template UnitTemplateCatalogEntry, key string) (valueType, bool) {
	handle, found := context.artifacts.authority.attributes[key]
	if !found {
		return valueType{}, false
	}
	allowed := false
	for _, policy := range template.AllowedAttributeOverrides {
		allowed = allowed || policy.Attribute == handle
	}
	for _, attribute := range context.environment.Gameplay.Attributes.Entries {
		if allowed && attribute.Handle == handle {
			return valueType{Base: attribute.ValueType, Quantity: attribute.Quantity}, true
		}
	}
	return valueType{}, false
}

func unitTemplateParameterType(template UnitTemplateCatalogEntry, name string) (valueType, bool) {
	for _, parameter := range template.Parameters {
		if parameter.Name == name {
			return valueType{Base: parameter.ValueType, Quantity: parameter.Quantity}, true
		}
	}
	return valueType{}, false
}

func validateDetachedCallbacks(context *compileContext, callbacks *processCallbacksIR, allowAreaFinish bool) {
	roots := []struct {
		event string
		flow  flowIR
	}{
		{"tick", callbacks.tick}, {"hit", callbacks.hit}, {"collision", callbacks.collision},
		{"end", callbacks.end}, {"cancel", callbacks.cancel}, {"transition", callbacks.transition},
		{"target_lost", callbacks.targetLost}, {"enter", callbacks.enter}, {"leave", callbacks.leave},
	}
	for _, callback := range roots {
		event, root := callback.event, callback.flow
		if root == nil {
			continue
		}
		locals := make(map[string]bool)
		walkFlowTree(root, func(flow flowIR) {
			switch typed := flow.(type) {
			case *selectFlowIR:
				switch consume := typed.consume.(type) {
				case *selectOneConsumeIR:
					locals[consume.local.Name] = true
				case *selectEachConsumeIR:
					locals[consume.local.Name] = true
				}
			case *repeatFlowIR:
				locals[typed.index.Name] = true
			case *effectFlowIR:
				if typed.result != nil && typed.result.local != nil {
					locals[typed.result.local.Name] = true
				}
			}
		})
		root.walkValues(func(value valueIR) {
			switch typed := value.(type) {
			case *referenceValueIR:
				if name, local := detachedLocalName(typed.reference); local {
					if !locals[name] {
						context.addDiagnostic(DiagnosticReferenceUnknown, typed.source.Path, "entity-scoped process callback cannot capture a cast local")
					}
					return
				}
				if !detachedReferenceAllowed(typed.reference) {
					context.addDiagnostic(DiagnosticInputUnavailable, typed.source.Path, "entity-scoped process callback reference is not available after cast handoff")
				}
			case *attributeReadValueIR:
				if typed.snapshot == "cast_start" || typed.snapshot == "phase_start" {
					context.addDiagnostic(DiagnosticInputUnavailable, typed.source.Path, "entity-scoped process callback cannot read a cast-local snapshot")
				}
			}
		})
		walkFlowTree(root, func(flow flowIR) {
			switch typed := flow.(type) {
			case *finishFlowIR:
				if allowAreaFinish && event != "cancel" {
					break
				}
				context.addDiagnostic(DiagnosticLifecycleControlConflict, flow.sourceRef().Path, "entity-scoped process callback cannot control its finished cast")
			case *gotoFlowIR:
				context.addDiagnostic(DiagnosticLifecycleControlConflict, flow.sourceRef().Path, "entity-scoped process callback cannot control its finished cast")
			case *waitFlowIR:
				context.addDiagnostic(DiagnosticLifecycleControlConflict, typed.source.Path, "entity-scoped process callback cannot suspend")
			case *repeatFlowIR:
				if typed.intervalTicks > 0 {
					context.addDiagnostic(DiagnosticLifecycleControlConflict, typed.source.Path, "entity-scoped process callback cannot schedule asynchronous repetition")
				}
			case *effectFlowIR:
				if typed.callbacks != nil {
					context.addDiagnostic(DiagnosticBudgetExceeded, typed.source.Path, "entity-scoped process callback cannot recursively create a process")
				}
				switch effect := typed.effect.(type) {
				case *setMemoryEffectIR, *addMemoryEffectIR, *clearMemoryEffectIR:
					context.addDiagnostic(DiagnosticInputUnavailable, typed.source.Path, "entity-scoped process callback cannot mutate cast memory")
				case *modifyStateEffectIR:
					if valueReferencesProcess(effect.value) {
						context.addDiagnostic(DiagnosticReferenceUnknown, effect.source.Path, "process references cannot be stored in persistent state")
					}
				}
			}
		})
	}
}

// validateDetachedProcessFields 检查 spawn 进程里每一步都要重新求值的字段（RR-20261005-NC-224）。
// 进程启动那一步用施法本身求值，之后每一步用 detachedProcessCast（process_owned.go
// advanceOwnedProcesses）：那里没有施法的输入、memory 与局部变量（`$input.*` / `$memory.*`
// 越界即 ErrProgramInvariant，局部变量全是 missing）。旧实现按施法作用域检查这些字段，于是
// `area.from: "$input.target"` 这类定义能编译，Activate 正常，下一 tick 起 Advance 返回
// ErrProgramInvariant。
//
// 每一步求值的是：area 选择（from / shape / filters）、motion 的 follow / tracking / carry
// 目标、path 点、orbit 锚点、parabola 目的地，以及没有绑定到进程数值属性的数值字段。
// numeric track 的值和绑定到属性的数值字段只在启动时（initializeProcessNumeric，用施法）求值
// 一次，不在此限。`$caster` 系列在两个上下文里都是施法者，仍然允许。
func validateDetachedProcessFields(context *compileContext, process *processIR) {
	visit := func(value valueIR) {
		reference, ok := value.(*referenceValueIR)
		if !ok {
			return
		}
		for _, prefix := range []string{"$input.", "$memory.", "$local."} {
			if strings.HasPrefix(reference.reference, prefix) {
				context.addDiagnostic(DiagnosticInputUnavailable, reference.source.Path, "an owned entity process re-evaluates this field on every step after the cast hands it off; cast input, memory and locals are not available there")
				return
			}
		}
	}
	if process.area != nil {
		process.area.walkValues(visit)
	}
	motion, ok := process.motion.(*canonicalMotionIR)
	if !ok || motion == nil {
		return
	}
	perStepNumeric := func(value valueIR, stage, variant, field string) {
		if !processNumericFieldBound(context.environment, process.kind, stage, variant, field) {
			walkValue(value, visit)
		}
	}
	motion.frame.walkValues(visit)
	if motion.steering != nil {
		motion.steering.walkValues(visit)
	}
	if motion.carry != nil {
		motion.carry.walkValues(visit)
	}
	switch trajectory := motion.trajectory.(type) {
	case linearTrajectoryIR:
		perStepNumeric(trajectory.speed, "trajectory", "linear", "speed")
	case pathTrajectoryIR:
		walkValue(trajectory.points, visit)
		perStepNumeric(trajectory.speed, "trajectory", "path", "speed")
	case orbitTrajectoryIR:
		walkValue(trajectory.anchor, visit)
		perStepNumeric(trajectory.radius, "trajectory", "orbit", "radius")
		perStepNumeric(trajectory.angularSpeed, "trajectory", "orbit", "angular_speed")
	case parabolaTrajectoryIR:
		walkValue(trajectory.destination, visit)
		perStepNumeric(trajectory.height, "trajectory", "parabola", "height")
	}
	for _, offset := range motion.offsets {
		switch typed := offset.(type) {
		case zigzagOffsetIR:
			perStepNumeric(typed.amplitude, "offset", "zigzag", "amplitude")
		case circularOffsetIR:
			perStepNumeric(typed.radius, "offset", "circular", "radius")
			perStepNumeric(typed.angularSpeed, "offset", "circular", "angular_speed")
		}
	}
}

// processNumericFieldBound 报告环境里是否有适用于该进程种类的数值属性绑定到这个 motion 槽位；
// 绑定的槽位由进程数值状态在启动时取值（resolveProcessNumeric 先查绑定）。
func processNumericFieldBound(environment CompileEnvironment, kind, stage, variant, field string) bool {
	for _, policy := range environment.ProcessProperties.Properties {
		if !containsString(policy.ProcessKinds, kind) {
			continue
		}
		for _, binding := range policy.SlotBindings {
			if binding.Stage == stage && binding.Variant == variant && binding.Field == field {
				return true
			}
		}
	}
	return false
}

func detachedReferenceAllowed(reference string) bool {
	return reference == "$owner" || strings.HasPrefix(reference, "$owner.") || reference == "$lifecycle_entity" || strings.HasPrefix(reference, "$lifecycle_entity.") || reference == "$process" || strings.HasPrefix(reference, "$event.")
}

func detachedLocalName(reference string) (string, bool) {
	if !strings.HasPrefix(reference, "$local.") {
		return "", false
	}
	name := strings.TrimPrefix(reference, "$local.")
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		name = name[:dot]
	}
	return name, name != ""
}

func valueReferencesProcess(value valueIR) bool {
	found := false
	walkValue(value, func(candidate valueIR) {
		if reference, ok := candidate.(*referenceValueIR); ok && reference.reference == "$process" {
			found = true
		}
	})
	return found
}

func validateOwnedEntityCommand(context *compileContext, effect *entityCommandEffectIR) {
	valid := false
	switch effect.command {
	case "move_to":
		valid = effect.position != nil && effect.targetEntity == nil && effect.behavior == ""
	case "follow", "attack_target":
		valid = effect.position == nil && effect.targetEntity != nil && effect.behavior == ""
	case "invoke_behavior":
		valid = effect.position == nil && effect.targetEntity == nil && effect.behavior != ""
	case "hold_position", "return_to_owner", "stop", "despawn":
		valid = effect.position == nil && effect.targetEntity == nil && effect.behavior == ""
	}
	if !valid {
		context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path, "entity command and arguments do not match the closed command schema")
	}
}

func validateOwnedEntitySelect(context *compileContext, flow *selectFlowIR) {
	_, owned := flow.selectPlan.shape.(*ownedEntitiesShapeIR)
	if !owned {
		return
	}
	path := flow.source.Path + ".select"
	if flow.selectPlan.elementType != selectionEntity {
		context.addDiagnostic(DiagnosticShapeInvalid, path+".kind", "owned_entities selects entity elements")
	}
	if flow.selectPlan.limit > context.environment.Limits.MaxOwnedEntities {
		context.addDiagnostic(DiagnosticBudgetExceeded, path+".limit", "owned entity select limit exceeds the environment maximum")
	}
	for index, filter := range flow.selectPlan.filters {
		switch typed := filter.(type) {
		case *ownedSourceSkillFilterIR:
			if typed.skill == "" {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "source_skill requires a skill id")
			}
		case *ownedSourceCastFilterIR:
			if typed.cast == 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "source_cast requires a non-zero cast id")
			}
		case *ownedSpawnTickFilterIR:
			if typed.tick < 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "spawn tick filter requires a non-negative tick")
			}
		case *ownedUnitTemplateFilterIR:
			if _, found := context.artifacts.authority.unitTemplates[typed.template]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, filter.sourceRef().Path, "unknown unit template")
			}
		case *ownedEntityTagFilterIR:
			if _, found := context.artifacts.authority.tags[typed.tag]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, filter.sourceRef().Path, "unknown owned entity tag")
			}
		default:
			context.addDiagnostic(DiagnosticShapeInvalid, path+".filters", "owned_entities only accepts source, template, and entity tag filters")
			_ = index
		}
	}
	if flow.selectPlan.order == nil {
		return
	}
	switch flow.selectPlan.order.by {
	case "stable_id", "entity_id", "spawn_tick", "spawn_sequence", "distance_to_owner", "remaining_lifetime":
	default:
		context.addDiagnostic(DiagnosticShapeInvalid, path+".order.by", "owned_entities requires a stable owned entity order")
	}
}
