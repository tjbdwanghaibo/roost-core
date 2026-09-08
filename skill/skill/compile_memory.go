package skill

import "strings"

type memoryState map[string]bool

func runMemoryPass(context *compileContext) {
	initial := make(memoryState, len(context.artifacts.ir.memory))
	for name, declaration := range context.artifacts.ir.memory {
		_, nullable := declaration.defaultValue.(*nullValueIR)
		initial[name] = !nullable
	}
	context.artifacts.memory.initializedAtEntry = cloneMemoryState(initial)
	for _, phase := range context.artifacts.ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) {
			analyzeMemoryFlow(context, flow, cloneMemoryState(initial), nil)
		})
	}
}

func analyzeMemoryFlow(context *compileContext, flow flowIR, state memoryState, guarded map[string]bool) memoryState {
	if flow == nil {
		return state
	}
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		for _, child := range typed.steps {
			state = analyzeMemoryFlow(context, child, state, guarded)
		}
		return state
	case *parallelFlowIR:
		outputs := make([]memoryState, 0, len(typed.branches))
		for _, branch := range typed.branches {
			outputs = append(outputs, analyzeMemoryFlow(context, branch, cloneMemoryState(state), guarded))
		}
		return intersectMemoryStates(state, outputs...)
	case *ifFlowIR:
		guardedName, hasGuard := directlyGuardedReference(typed.condition)
		conditionGuards := cloneStringSet(guarded)
		if hasGuard {
			if name, ok := memoryName(guardedName); ok {
				conditionGuards[name] = true
			}
		}
		checkMemoryValue(context, typed.condition, state, conditionGuards)
		thenState := cloneMemoryState(state)
		thenGuards := cloneStringSet(guarded)
		if hasGuard {
			if name, ok := memoryName(guardedName); ok {
				thenState[name] = true
				thenGuards[name] = true
			}
		}
		thenState = analyzeMemoryFlow(context, typed.thenFlow, thenState, thenGuards)
		elseState := analyzeMemoryFlow(context, typed.elseFlow, cloneMemoryState(state), guarded)
		return intersectMemoryStates(state, thenState, elseState)
	case *repeatFlowIR:
		checkMemoryValue(context, typed.times, state, guarded)
		body := analyzeMemoryFlow(context, typed.body, cloneMemoryState(state), guarded)
		return intersectMemoryStates(state, state, body)
	case *waitFlowIR:
		return analyzeMemoryFlow(context, typed.then, state, nil)
	case *selectFlowIR:
		checkMemoryValue(context, typed.selectPlan.from, state, guarded)
		typed.selectPlan.shape.walkValues(func(value valueIR) { checkMemoryReference(context, value, state, guarded) })
		for _, filter := range typed.selectPlan.filters {
			filter.walkValues(func(value valueIR) { checkMemoryReference(context, value, state, guarded) })
		}
		outputs := []memoryState{cloneMemoryState(state)}
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			outputs = append(outputs, analyzeMemoryFlow(context, consume.then, cloneMemoryState(state), guarded))
		case *selectEachConsumeIR:
			outputs = append(outputs, analyzeMemoryFlow(context, consume.body, cloneMemoryState(state), guarded))
		}
		outputs = append(outputs, analyzeMemoryFlow(context, typed.onEmpty, cloneMemoryState(state), guarded))
		return intersectMemoryStates(state, outputs...)
	case *effectFlowIR:
		typed.effect.walkValues(func(value valueIR) { checkMemoryReference(context, value, state, guarded) })
		if typed.process != nil {
			typed.process.walkValues(func(value valueIR) { checkMemoryReference(context, value, state, guarded) })
		}
		switch effect := typed.effect.(type) {
		case *setMemoryEffectIR:
			if _, exists := state[effect.name]; exists {
				state[effect.name] = true
			}
		case *addMemoryEffectIR:
			if initialized, exists := state[effect.name]; exists && !initialized {
				context.addDiagnostic(DiagnosticMemoryMaybeUninitialized, effect.source.Path, "memory must be initialized before add_memory")
			}
		case *clearMemoryEffectIR:
			if _, exists := state[effect.name]; exists {
				state[effect.name] = false
			}
		}
		if typed.result != nil {
			analyzeMemoryFlow(context, typed.result.success, cloneMemoryState(state), nil)
			analyzeMemoryFlow(context, typed.result.failure, cloneMemoryState(state), nil)
		}
		if typed.callbacks != nil {
			walkPhaseFlows(phaseEventsIR{enter: typed.callbacks.enter, cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition, release: typed.callbacks.targetLost, pulse: typed.callbacks.tick}, func(child flowIR) {
				analyzeMemoryFlow(context, child, cloneMemoryState(state), nil)
			})
			analyzeMemoryFlow(context, typed.callbacks.leave, cloneMemoryState(state), nil)
		}
		return state
	default:
		return state
	}
}

func checkMemoryValue(context *compileContext, value valueIR, state memoryState, guarded map[string]bool) {
	walkValue(value, func(candidate valueIR) { checkMemoryReference(context, candidate, state, guarded) })
}

func checkMemoryReference(context *compileContext, candidate valueIR, state memoryState, guarded map[string]bool) {
	reference, ok := candidate.(*referenceValueIR)
	if !ok {
		return
	}
	name, ok := memoryName(reference.reference)
	if !ok || state[name] || guarded[name] {
		return
	}
	context.addDiagnostic(DiagnosticMemoryMaybeUninitialized, reference.source.Path, "memory may be uninitialized on this path")
}

func memoryName(reference string) (string, bool) {
	const prefix = "$memory."
	if !strings.HasPrefix(reference, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(reference, prefix)
	if index := strings.IndexByte(name, '.'); index >= 0 {
		name = name[:index]
	}
	return name, name != ""
}

func cloneMemoryState(state memoryState) memoryState {
	copy := make(memoryState, len(state))
	for name, initialized := range state {
		copy[name] = initialized
	}
	return copy
}

func cloneStringSet(values map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(values)+1)
	for value, present := range values {
		copy[value] = present
	}
	return copy
}

func intersectMemoryStates(fallback memoryState, states ...memoryState) memoryState {
	result := cloneMemoryState(fallback)
	if len(states) == 0 {
		return result
	}
	for name := range result {
		initialized := true
		for _, state := range states {
			initialized = initialized && state[name]
		}
		result[name] = initialized
	}
	return result
}
