package skill

func prepareEffectResultLayouts(context *compileContext) {
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		effectFlow, ok := flow.(*effectFlowIR)
		if !ok {
			return
		}
		layout, found := effectResultLayout(context, effectFlow.effect)
		if !found && effectFlow.result != nil {
			context.addDiagnostic(DiagnosticShapeInvalid, effectFlow.result.source.Path, "effect does not expose an instant typed result")
			return
		}
		if found {
			effectFlow.resultLayout = layout
			effectFlow.hasResultLayout = true
			if effectFlow.result != nil {
				effectFlow.result.layout = layout
			}
		}
	})
}

func runEffectResultScopePass(context *compileContext) {
	resultCount := 0
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		effectFlow, ok := flow.(*effectFlowIR)
		if !ok || effectFlow.result == nil {
			return
		}
		resultCount++
		if effectFlow.callbacks != nil {
			context.addDiagnostic(DiagnosticShapeInvalid, effectFlow.result.source.Path, "process effects cannot declare instant result branches")
		}
		for _, branch := range []flowIR{effectFlow.result.success, effectFlow.result.failure} {
			if effectResultBranchMaySuspend(branch) {
				context.addDiagnostic(DiagnosticShapeInvalid, branch.sourceRef().Path, "effect result branches cannot suspend or start a process")
			}
		}
	})
	if resultCount > context.environment.Limits.MaxEffectResultSlots {
		context.addDiagnostic(DiagnosticBudgetExceeded, "$", "effect result slot budget exceeded")
	}
}

func effectResultBranchMaySuspend(flow flowIR) bool {
	if flow == nil {
		return false
	}
	found := false
	walkFlowTree(flow, func(candidate flowIR) {
		switch typed := candidate.(type) {
		case *waitFlowIR:
			found = true
		case *repeatFlowIR:
			found = found || typed.intervalTicks > 0
		case *effectFlowIR:
			found = found || typed.callbacks != nil
		}
	})
	return found
}

func effectResultLayout(context *compileContext, effect effectIR) (resultLayoutProgram, bool) {
	switch typed := effect.(type) {
	case *captureSnapshotEffectIR:
		return resultLayoutByType(resultTypeSnapshotCapture, valueType{}), true
	case *restoreSnapshotEffectIR:
		return resultLayoutByType(resultTypeSnapshotRestore, valueType{}), true
	case *damageEffectIR:
		return resultLayoutByType(resultTypeDamage, valueType{}), true
	case *healEffectIR:
		return resultLayoutByType(resultTypeHeal, valueType{}), true
	case *shieldEffectIR:
		return resultLayoutByType(resultTypeShield, valueType{}), true
	case *teleportEffectIR:
		return resultLayoutByType(resultTypeTeleport, valueType{}), true
	case *addStatusEffectIR, *removeStatusEffectIR, *modifyStatusInstanceEffectIR:
		return resultLayoutByType(resultTypeStatusOperation, valueType{}), true
	case *attributeModifierEffectIR:
		return resultLayoutByType(resultTypeAttributeModifier, valueType{}), true
	case *modifyStateEffectIR:
		plan, ok := context.artifacts.state.plans[typed.state]
		if !ok {
			return resultLayoutProgram{}, false
		}
		return resultLayoutByType(resultTypeStateChange, plan.typ), true
	case *modifyAbilityStateEffectIR:
		property, ok := context.artifacts.ability.properties[typed.property]
		if !ok {
			return resultLayoutProgram{}, false
		}
		propertyType := valueType{Base: property.policy.ValueType}
		if propertyType.Base == valueKindInt {
			propertyType.Quantity = abilityPropertyQuantity(typed.property)
		}
		return resultLayoutByType(resultTypeAbilityChange, propertyType), true
	case *spawnEffectIR:
		return resultLayoutByType(resultTypeSpawn, valueType{}), true
	case *entityCommandEffectIR:
		return resultLayoutByType(resultTypeEntityCommand, valueType{}), true
	default:
		return resultLayoutProgram{}, false
	}
}
