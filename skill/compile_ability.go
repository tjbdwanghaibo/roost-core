package skill

import "sort"

type AbilityPropertyHandle uint16

type resolvedAbilityProperty struct {
	handle AbilityPropertyHandle
	policy AbilityPropertyPolicy
}

type AbilityReadPlan struct {
	Owner, Ability valueIR
	Property       AbilityPropertyHandle
	Snapshot       snapshotPoint
	Type           valueType
}

func runAbilityPass(context *compileContext) {
	context.artifacts.ability.properties = make(map[string]resolvedAbilityProperty)
	context.artifacts.ability.reads = make(map[string]AbilityReadPlan)
	context.artifacts.ability.selectableTags = normalizeGameplayTagHandles(context.environment.Gameplay.Abilities.SelectableTags)
	context.artifacts.ability.ownerRelations = normalizeAbilityRelations(context.environment.Gameplay.Abilities.OwnerRelations)
	for index, policy := range context.environment.Gameplay.Abilities.Properties {
		context.artifacts.ability.properties[policy.Property] = resolvedAbilityProperty{handle: AbilityPropertyHandle(index + 1), policy: policy}
	}
	context.artifacts.ir.walkValues(func(value valueIR) {
		read, ok := value.(*abilityStateReadValueIR)
		if !ok {
			return
		}
		property, found := context.artifacts.ability.properties[read.property]
		if !found {
			context.addDiagnostic(DiagnosticCapabilityUnknown, read.source.Path+".read_ability_state.property", "unknown ability property")
			return
		}
		read.resolvedType = valueType{Base: property.policy.ValueType}
		if property.policy.ValueType == valueKindInt {
			read.resolvedType.Quantity = abilityPropertyQuantity(read.property)
		}
		point := snapshotPoint(read.snapshot)
		if point == "" {
			point = snapshotCurrent
		}
		if point != snapshotCurrent {
			context.addDiagnostic(DiagnosticAttributeSnapshotInvalid, read.source.Path+".read_ability_state.snapshot", "ability state only supports current snapshots")
			return
		}
		context.artifacts.ability.reads[read.source.Path] = AbilityReadPlan{Owner: read.owner, Ability: read.ability, Property: property.handle, Snapshot: point, Type: read.resolvedType}
	})
	context.artifacts.ir.walkEffects(func(effect effectIR) {
		mutation, ok := effect.(*modifyAbilityStateEffectIR)
		if !ok {
			return
		}
		property, found := context.artifacts.ability.properties[mutation.property]
		if !found {
			context.addDiagnostic(DiagnosticCapabilityUnknown, mutation.source.Path+".property", "unknown ability property")
			return
		}
		if !property.policy.Mutable {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".property", "ability property is read-only")
		}
		if !abilityOperationAllowed(mutation.property, mutation.operation) {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".operation", "ability operation is not allowed")
		}
		if mutation.property == "enabled" {
			if literal, ok := mutation.value.(*boolValueIR); !ok || literal.value || mutation.durationTicks <= 0 || mutation.durationTicks > property.policy.MaximumDurationTicks {
				context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path, "enabled mutation must be a bounded set=false overlay")
			}
		} else if mutation.durationTicks != 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, mutation.source.Path+".duration_ticks", "duration is only valid for enabled overlays")
		}
	})
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		selectFlow, ok := flow.(*selectFlowIR)
		if !ok {
			return
		}
		plan := &selectFlow.selectPlan
		_, abilityShape := plan.shape.(*abilitySetShapeIR)
		if plan.elementType == selectionAbility && !abilityShape || abilityShape && plan.elementType != selectionAbility {
			context.addDiagnostic(DiagnosticShapeInvalid, plan.source.Path, "ability selects require kind=ability and shape=ability_set")
		}
		for _, filter := range plan.filters {
			_, abilityTag := filter.(*abilityTagFilterIR)
			_, abilitySlot := filter.(*abilitySlotFilterIR)
			flag, flagFilter := filter.(*flagFilterIR)
			abilityFlag := flagFilter && (flag.kind == "self_ability" || flag.kind == "not_self_ability" || flag.kind == "ability_enabled" || flag.kind == "ability_on_cooldown" || flag.kind == "ability_has_ammo")
			if plan.elementType == selectionAbility && !abilityTag && !abilitySlot && !abilityFlag {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "filter is not valid for an ability select")
			}
			if plan.elementType != selectionAbility && (abilityTag || abilitySlot || abilityFlag) {
				context.addDiagnostic(DiagnosticShapeInvalid, filter.sourceRef().Path, "ability filter requires an ability select")
			}
			switch typed := filter.(type) {
			case *abilityTagFilterIR:
				handle := context.artifacts.authority.tags[typed.tag]
				if handle == 0 {
					context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".tag", "unknown ability gameplay tag")
				} else if !containsGameplayTag(context.artifacts.ability.selectableTags, handle) {
					context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".tag", "gameplay tag is not selectable for abilities")
				}
			case *abilitySlotFilterIR:
				if typed.slot < 0 {
					context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".slot", "ability slot must be non-negative")
				}
			}
			if abilityFlag {
				requiredProperty := ""
				switch flag.kind {
				case "ability_enabled":
					requiredProperty = "enabled"
				case "ability_on_cooldown":
					requiredProperty = "cooldown_remaining_ticks"
				case "ability_has_ammo":
					requiredProperty = "ammo_stock"
				}
				if requiredProperty != "" {
					if _, found := context.artifacts.ability.properties[requiredProperty]; !found {
						context.addDiagnostic(DiagnosticCapabilityUnknown, filter.sourceRef().Path, "ability filter property is not available in the catalog")
					}
				}
			}
		}
		if plan.elementType == selectionAbility && plan.order != nil {
			if plan.order.by != "ability_slot" && plan.order.by != "stable_id" {
				context.addDiagnostic(DiagnosticShapeInvalid, plan.source.Path+".order.by", "ability select order must be ability_slot or stable_id")
			}
			if plan.order.direction != "asc" && plan.order.direction != "desc" {
				context.addDiagnostic(DiagnosticShapeInvalid, plan.source.Path+".order.direction", "order direction must be asc or desc")
			}
		}
	})
}

func normalizeAbilityRelations(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func abilityPropertyQuantity(property string) quantityKind {
	switch property {
	case "cooldown_remaining_ticks", "cooldown_total_ticks", "last_commit_tick", "last_finish_tick":
		return quantityTicks
	default:
		return quantityCount
	}
}

func abilityPropertyValueKind(property string) (valueKind, bool) {
	switch property {
	case "cooldown_remaining_ticks", "cooldown_total_ticks", "ammo_stock", "ammo_max_stock", "last_commit_tick", "last_finish_tick":
		return valueKindInt, true
	case "enabled", "cast_active":
		return valueKindBool, true
	default:
		return valueKindInvalid, false
	}
}

func abilityOperationAllowed(property, operation string) bool {
	switch property {
	case "cooldown_remaining_ticks":
		return operation == "add" || operation == "set" || operation == "mul_bp" || operation == "min" || operation == "max"
	case "ammo_stock":
		return operation == "add" || operation == "set" || operation == "min" || operation == "max"
	case "enabled":
		return operation == "set"
	default:
		return false
	}
}
