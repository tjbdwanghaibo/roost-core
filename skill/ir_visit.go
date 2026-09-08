package skill

type flowVisitor func(flowIR)
type referenceVisitor func(*referenceValueIR)
type visualVisitor func(*visualIR)

func (ir *skillIR) walkValues(visitor valueVisitor) {
	for _, cost := range ir.costs {
		walkValue(cost.amount, visitor)
	}
	if ir.activation.castWindow.hasWindupExpression {
		walkValue(ir.activation.castWindow.windupExpression, visitor)
	}
	if ir.activation.castWindow.hasRecoveryExpression {
		walkValue(ir.activation.castWindow.recoveryExpression, visitor)
	}
	for _, cost := range ir.activation.policy.sustainCosts {
		walkValue(cost.amount, visitor)
	}
	for _, declaration := range ir.memory {
		walkValue(declaration.defaultValue, visitor)
	}
	for _, declaration := range ir.persistentState {
		walkValue(declaration.defaultValue, visitor)
	}
	for _, phase := range ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) { flow.walkValues(visitor) })
	}
}

func (ir *skillIR) walkReferences(visitor referenceVisitor) {
	ir.walkValues(func(value valueIR) {
		if reference, ok := value.(*referenceValueIR); ok {
			visitor(reference)
		}
	})
}

func (ir *skillIR) walkFlows(visitor flowVisitor) {
	for _, phase := range ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) { walkFlowTree(flow, visitor) })
	}
}

func (ir *skillIR) walkEffects(visitor effectVisitor) {
	ir.walkFlows(func(flow flowIR) {
		if effectFlow, ok := flow.(*effectFlowIR); ok {
			visitor(effectFlow.effect)
		}
	})
}

func (ir *skillIR) walkProcesses(visitor effectVisitor) {
	ir.walkEffects(func(effect effectIR) {
		// Process variants are introduced as sealed effect IR types in Task 10.
		_ = effect
	})
	_ = visitor
}

func (ir *skillIR) walkVisualRefs(visitor visualVisitor) {
	if ir.presentation != nil && ir.presentation.cast != nil {
		visitor(ir.presentation.cast)
	}
	ir.walkEffects(func(effect effectIR) {
		if visual := effectVisual(effect); visual != nil {
			visitor(visual)
		}
	})
	ir.walkFlows(func(flow flowIR) {
		if effectFlow, ok := flow.(*effectFlowIR); ok && effectFlow.process != nil && effectFlow.process.visual != nil {
			visitor(effectFlow.process.visual)
		}
	})
}

func walkValue(value valueIR, visitor valueVisitor) {
	if value == nil {
		return
	}
	visitor(value)
	switch typed := value.(type) {
	case *expressionValueIR:
		for _, argument := range typed.args {
			walkValue(argument, visitor)
		}
	case *attributeReadValueIR:
		walkValue(typed.entity, visitor)
	case *stateReadValueIR:
		walkValue(typed.owner, visitor)
		walkValue(typed.subject, visitor)
		walkValue(typed.teamOf, visitor)
	case *abilityStateReadValueIR:
		walkValue(typed.owner, visitor)
		walkValue(typed.ability, visitor)
	}
}

func walkPhaseFlows(events phaseEventsIR, visit func(flowIR)) {
	for _, flow := range []flowIR{events.enter, events.recast, events.cancel, events.directionChanged, events.targetChanged, events.timeout, events.release, events.pulse} {
		if flow != nil {
			visit(flow)
		}
	}
}

func walkFlowTree(flow flowIR, visitor flowVisitor) {
	if flow == nil {
		return
	}
	visitor(flow)
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		for _, child := range typed.steps {
			walkFlowTree(child, visitor)
		}
	case *parallelFlowIR:
		for _, child := range typed.branches {
			walkFlowTree(child, visitor)
		}
	case *ifFlowIR:
		walkFlowTree(typed.thenFlow, visitor)
		walkFlowTree(typed.elseFlow, visitor)
	case *repeatFlowIR:
		walkFlowTree(typed.body, visitor)
	case *waitFlowIR:
		walkFlowTree(typed.then, visitor)
	case *selectFlowIR:
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			walkFlowTree(consume.then, visitor)
		case *selectEachConsumeIR:
			walkFlowTree(consume.body, visitor)
		}
		walkFlowTree(typed.onEmpty, visitor)
	case *effectFlowIR:
		if typed.result != nil {
			walkFlowTree(typed.result.success, visitor)
			walkFlowTree(typed.result.failure, visitor)
		}
		if typed.callbacks != nil {
			walkPhaseFlows(phaseEventsIR{enter: typed.callbacks.enter, cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition, release: typed.callbacks.targetLost, pulse: typed.callbacks.tick}, func(child flowIR) { walkFlowTree(child, visitor) })
			walkFlowTree(typed.callbacks.leave, visitor)
		}
	}
}

func effectVisual(effect effectIR) *visualIR {
	switch typed := effect.(type) {
	case *damageEffectIR:
		return typed.visual
	case *healEffectIR:
		return typed.visual
	case *shieldEffectIR:
		return typed.visual
	case *addStatusEffectIR:
		return typed.visual
	case *removeStatusEffectIR:
		return typed.visual
	case *attributeModifierEffectIR:
		return typed.visual
	case *resourceEffectIR:
		return typed.visual
	case *teleportEffectIR:
		return typed.visual
	case *knockbackEffectIR:
		return typed.visual
	case *pullEffectIR:
		return typed.visual
	case *stopMovementEffectIR:
		return typed.visual
	default:
		return nil
	}
}
