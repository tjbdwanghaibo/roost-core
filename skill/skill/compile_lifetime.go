package skill

func runLifetimePass(context *compileContext) {
	context.artifacts.lifetimes = make(map[string]lifecycleFact)
	for _, phase := range context.artifacts.ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) {
			fact := analyzeLifecycle(context, flow)
			context.artifacts.lifetimes[flow.sourceRef().Path] = fact
		})
		if phase.events.enter != nil {
			fact := context.artifacts.lifetimes[phase.events.enter.sourceRef().Path]
			policySuspend := phase.id == context.artifacts.ir.initialPhase && policyAllowsEnterFallthrough(context.artifacts.ir.activation.policy.mode)
			if phase.timeoutTicks == 0 && fact.CanFallthrough && !policySuspend {
				context.addDiagnostic(DiagnosticLifecycleFallthrough, phase.events.enter.sourceRef().Path, "phase enter flow may fall through without a timeout")
			}
		}
	}
	if policyAllowsEnterFallthrough(context.artifacts.ir.activation.policy.mode) {
		initial := context.artifacts.ir.phases[context.artifacts.graph.Index[context.artifacts.ir.initialPhase]]
		if initial.events.release == nil {
			context.addDiagnostic(DiagnosticLifecycleFallthrough, "$.phases", "charge, toggle, and hold policies require an on.release flow")
		} else {
			fact := context.artifacts.lifetimes[initial.events.release.sourceRef().Path]
			if fact.CanFallthrough {
				context.addDiagnostic(DiagnosticLifecycleFallthrough, initial.events.release.sourceRef().Path, "release flow must finish or goto")
			}
		}
	}
}

func policyAllowsEnterFallthrough(mode castMode) bool {
	return mode == castModeCharge || mode == castModeToggle || mode == castModeHold
}

func analyzeLifecycle(context *compileContext, flow flowIR) lifecycleFact {
	if flow == nil {
		return lifecycleFact{CanFallthrough: true}
	}
	var fact lifecycleFact
	switch typed := flow.(type) {
	case *finishFlowIR, *gotoFlowIR:
		fact = lifecycleFact{MustTerminate: true}
	case *effectFlowIR:
		fact = lifecycleFact{CanFallthrough: true}
		if typed.process != nil && typed.process.kind == "area" {
			processes := 1
			if spawn, ok := typed.effect.(*spawnEffectIR); ok {
				processes = spawn.count
			}
			fact.MaxLifetime = typed.process.durationTicks
			fact.MaxSchedules = saturatingMul(processes, areaStepBound(typed.process.durationTicks, typed.process.intervalTicks))
			fact.MaxProcesses = processes
		}
		if typed.result != nil {
			resultFact := mergeAlternativeLifetimes(analyzeLifecycle(context, typed.result.success), analyzeLifecycle(context, typed.result.failure))
			resultFact.MaxLifetime = maxTick(resultFact.MaxLifetime, fact.MaxLifetime)
			resultFact.MaxSchedules = saturatingAdd(resultFact.MaxSchedules, fact.MaxSchedules)
			resultFact.MaxProcesses = saturatingAdd(resultFact.MaxProcesses, fact.MaxProcesses)
			fact = resultFact
		}
		if typed.callbacks != nil {
			if typed.process == nil || typed.process.kind != "area" {
				fact.MaxProcesses = saturatingAdd(fact.MaxProcesses, 1)
			}
		}
	case *sequenceFlowIR:
		fact = lifecycleFact{CanFallthrough: true}
		for _, child := range typed.steps {
			if !fact.CanFallthrough {
				break
			}
			childFact := analyzeLifecycle(context, child)
			fact.MaxLifetime = saturatingTickAdd(fact.MaxLifetime, childFact.MaxLifetime)
			fact.MaxSchedules = saturatingAdd(fact.MaxSchedules, childFact.MaxSchedules)
			fact.MaxProcesses = saturatingAdd(fact.MaxProcesses, childFact.MaxProcesses)
			fact.MaySuspend = fact.MaySuspend || childFact.MaySuspend
			fact.CanFallthrough = childFact.CanFallthrough
			fact.MustTerminate = childFact.MustTerminate
		}
	case *parallelFlowIR:
		if containsLifecycleControl(flow) {
			context.addDiagnostic(DiagnosticLifecycleControlConflict, typed.source.Path, "goto and finish are not allowed inside parallel branches")
		}
		fact = lifecycleFact{CanFallthrough: true}
		for _, branch := range typed.branches {
			branchFact := analyzeLifecycle(context, branch)
			fact.CanFallthrough = fact.CanFallthrough && branchFact.CanFallthrough
			fact.MustTerminate = fact.MustTerminate || branchFact.MustTerminate
			fact.MaySuspend = fact.MaySuspend || branchFact.MaySuspend
			fact.MaxLifetime = maxTick(fact.MaxLifetime, branchFact.MaxLifetime)
			fact.MaxSchedules = saturatingAdd(fact.MaxSchedules, branchFact.MaxSchedules)
			fact.MaxProcesses = saturatingAdd(fact.MaxProcesses, branchFact.MaxProcesses)
		}
	case *ifFlowIR:
		fact = mergeAlternativeLifetimes(analyzeLifecycle(context, typed.thenFlow), analyzeLifecycle(context, typed.elseFlow))
	case *repeatFlowIR:
		body := analyzeLifecycle(context, typed.body)
		times := literalRepeat(typed.times)
		fact = body
		fact.CanFallthrough = true
		fact.MustTerminate = false
		fact.MaxLifetime = Tick(saturatingMul(int(body.MaxLifetime), times))
		fact.MaxSchedules = saturatingMul(body.MaxSchedules, times)
		fact.MaxProcesses = saturatingMul(body.MaxProcesses, times)
		if typed.intervalTicks > 0 && times > 1 {
			fact.MaySuspend = true
			fact.MaxSchedules = saturatingAdd(fact.MaxSchedules, times-1)
			fact.MaxLifetime = saturatingTickAdd(fact.MaxLifetime, Tick(saturatingMul(int(typed.intervalTicks), times-1)))
		}
	case *waitFlowIR:
		fact = analyzeLifecycle(context, typed.then)
		fact.MaySuspend = true
		fact.MaxSchedules = saturatingAdd(fact.MaxSchedules, 1)
		fact.MaxLifetime = saturatingTickAdd(typed.ticks, fact.MaxLifetime)
	case *selectFlowIR:
		var consumed lifecycleFact
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			consumed = analyzeLifecycle(context, consume.then)
		case *selectEachConsumeIR:
			consumed = analyzeLifecycle(context, consume.body)
			consumed.MaxSchedules = saturatingMul(consumed.MaxSchedules, typed.selectPlan.limit)
			consumed.MaxProcesses = saturatingMul(consumed.MaxProcesses, typed.selectPlan.limit)
		}
		fact = mergeAlternativeLifetimes(consumed, analyzeLifecycle(context, typed.onEmpty))
	default:
		fact = lifecycleFact{CanFallthrough: true}
	}
	context.artifacts.lifetimes[flow.sourceRef().Path] = fact
	return fact
}

func mergeAlternativeLifetimes(left, right lifecycleFact) lifecycleFact {
	return lifecycleFact{
		CanFallthrough: left.CanFallthrough || right.CanFallthrough,
		MustTerminate:  left.MustTerminate && right.MustTerminate,
		MaySuspend:     left.MaySuspend || right.MaySuspend,
		MaxLifetime:    maxTick(left.MaxLifetime, right.MaxLifetime),
		MaxSchedules:   maxInt(left.MaxSchedules, right.MaxSchedules),
		MaxProcesses:   maxInt(left.MaxProcesses, right.MaxProcesses),
	}
}

func containsLifecycleControl(flow flowIR) bool {
	found := false
	walkFlowTree(flow, func(candidate flowIR) {
		switch candidate.(type) {
		case *gotoFlowIR, *finishFlowIR:
			found = true
		}
	})
	return found
}

func literalRepeat(value valueIR) int {
	if literal, ok := value.(*intValueIR); ok && literal.value > 0 {
		if literal.value > int64(maxIntValue()) {
			return maxIntValue()
		}
		return int(literal.value)
	}
	return 1
}

func maxTick(left, right Tick) Tick {
	if left > right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
