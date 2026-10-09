package skill

import "fmt"

func runBudgetPass(context *compileContext) {
	computed := ComputedLimits{}
	computed.RandomSites = len(context.artifacts.identity.RandomSites)
	computed.InputPathPoints = context.artifacts.input.MaximumPathPoints
	computed.InputPathLength = context.artifacts.input.MaximumPathLength
	if context.artifacts.proc.plan != nil {
		computed.EventsPerRoot = context.artifacts.proc.plan.MaxEventsPerRoot
		computed.PassiveActivationsPerTick = 1
	}
	context.artifacts.ir.walkValues(func(valueIR) {
		computed.ValueNodes = saturatingAdd(computed.ValueNodes, 1)
	})
	for _, phase := range context.artifacts.ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) {
			measureFlowBudget(flow, 1, 1, &computed)
			fact := context.artifacts.lifetimes[flow.sourceRef().Path]
			computed.LifetimeTicks = maxTick(computed.LifetimeTicks, fact.MaxLifetime)
			computed.Schedules = maxInt(computed.Schedules, fact.MaxSchedules)
			computed.Spawns = maxInt(computed.Spawns, fact.MaxSpawns)
		})
	}
	context.artifacts.limits = computed
	limits := context.environment.Limits
	checkBudget(context, "phases", len(context.artifacts.ir.phases), limits.MaxPhases)
	checkBudget(context, "flow_nodes", computed.FlowNodes, limits.MaxFlowNodes)
	checkBudget(context, "flow_depth", computed.FlowDepth, limits.MaxFlowDepth)
	checkBudget(context, "value_nodes", computed.ValueNodes, limits.MaxValueNodes)
	checkBudget(context, "repeat", computed.Repeat, limits.MaxRepeat)
	checkBudget(context, "targets", computed.Targets, limits.MaxTargets)
	checkBudget(context, "spawns", computed.Spawns, limits.MaxSpawns)
	checkBudget(context, "schedules", computed.Schedules, limits.MaxSchedules)
	checkBudget(context, "mutations", computed.Mutations, limits.MaxMutations)
	checkBudget(context, "area_members", computed.AreaMembers, limits.MaxAreaMembers)
	checkBudget(context, "ability_mutations", computed.AbilityMutations, limits.MaxAbilityMutations)
	checkBudget(context, "owned_entities", computed.OwnedEntities, limits.MaxOwnedEntities)
	checkBudget(context, "owned_spawns", computed.OwnedSpawns, limits.MaxOwnedSpawns)
	checkBudget(context, "status_mutations", computed.StatusMutations, limits.MaxStatusMutations)
	checkBudget(context, "temporal_snapshots", computed.TemporalSnapshots, limits.MaxTemporalSnapshots)
	checkBudget(context, "random_sites", computed.RandomSites, limits.MaxRandomSites)
	checkBudget(context, "local_frames", computed.LocalFrames, limits.MaxLocalFrames)
	checkBudget(context, "input_path_points", computed.InputPathPoints, limits.MaxInputPathPoints)
	if computed.InputPathLength > limits.MaxInputPathLength {
		context.addDiagnostic(DiagnosticBudgetExceeded, "$.input_schema.maximum_total_length", fmt.Sprintf("input_path_length budget %d exceeds limit %d", computed.InputPathLength, limits.MaxInputPathLength))
	}
	if computed.LifetimeTicks > limits.MaxLifetimeTicks {
		context.addDiagnostic(DiagnosticBudgetExceeded, "$", fmt.Sprintf("lifetime_ticks budget %d exceeds limit %d", computed.LifetimeTicks, limits.MaxLifetimeTicks))
	}
}

func measureFlowBudget(flow flowIR, depth, invocationBound int, computed *ComputedLimits) {
	if flow == nil {
		return
	}
	computed.FlowNodes = saturatingAdd(computed.FlowNodes, invocationBound)
	computed.FlowDepth = maxInt(computed.FlowDepth, depth)
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		for _, child := range typed.steps {
			measureFlowBudget(child, depth+1, invocationBound, computed)
		}
	case *parallelFlowIR:
		for _, branch := range typed.branches {
			measureFlowBudget(branch, depth+1, invocationBound, computed)
		}
	case *ifFlowIR:
		measureFlowBudget(typed.thenFlow, depth+1, invocationBound, computed)
		measureFlowBudget(typed.elseFlow, depth+1, invocationBound, computed)
	case *repeatFlowIR:
		times := literalRepeat(typed.times)
		computed.Repeat = maxInt(computed.Repeat, times)
		computed.LocalFrames = saturatingAdd(computed.LocalFrames, saturatingMul(invocationBound, times))
		measureFlowBudget(typed.body, depth+1, saturatingMul(invocationBound, times), computed)
	case *waitFlowIR:
		measureFlowBudget(typed.then, depth+1, invocationBound, computed)
	case *selectFlowIR:
		computed.Targets = maxInt(computed.Targets, saturatingMul(invocationBound, typed.selectPlan.limit))
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			computed.LocalFrames = saturatingAdd(computed.LocalFrames, invocationBound)
			measureFlowBudget(consume.then, depth+1, invocationBound, computed)
		case *selectEachConsumeIR:
			consumerBound := saturatingMul(invocationBound, typed.selectPlan.limit)
			computed.LocalFrames = saturatingAdd(computed.LocalFrames, consumerBound)
			measureFlowBudget(consume.body, depth+1, consumerBound, computed)
		}
		measureFlowBudget(typed.onEmpty, depth+1, invocationBound, computed)
	case *effectFlowIR:
		computed.Mutations = saturatingAdd(computed.Mutations, invocationBound)
		spawnInvocationBound := invocationBound
		if summon, ok := typed.effect.(*summonEffectIR); ok && typed.spawn != nil && typed.spawn.kind == "area" {
			spawnInvocationBound = saturatingMul(spawnInvocationBound, summon.count)
		}
		if typed.spawn != nil && len(typed.spawn.numericTracks) != 0 {
			computed.Mutations = saturatingAdd(computed.Mutations, saturatingMul(spawnInvocationBound, len(typed.spawn.numericTracks)))
		}
		if typed.spawn != nil && typed.spawn.kind == "area" && typed.spawn.area != nil {
			computed.AreaMembers = maxInt(computed.AreaMembers, saturatingMul(spawnInvocationBound, typed.spawn.area.limit))
		}
		if summon, ok := typed.effect.(*summonEffectIR); ok {
			summoned := saturatingMul(invocationBound, summon.count)
			computed.OwnedEntities = saturatingAdd(computed.OwnedEntities, summoned)
			if typed.callbacks != nil {
				computed.OwnedSpawns = saturatingAdd(computed.OwnedSpawns, summoned)
			}
		}
		if _, ok := typed.effect.(*modifyAbilityStateEffectIR); ok {
			computed.AbilityMutations = saturatingAdd(computed.AbilityMutations, invocationBound)
		}
		if _, ok := typed.effect.(*modifyStatusInstanceEffectIR); ok {
			computed.StatusMutations = saturatingAdd(computed.StatusMutations, invocationBound)
		}
		if _, ok := typed.effect.(*captureSnapshotEffectIR); ok {
			computed.TemporalSnapshots = saturatingAdd(computed.TemporalSnapshots, invocationBound)
		}
		if typed.result != nil {
			measureFlowBudget(typed.result.success, depth+1, invocationBound, computed)
			measureFlowBudget(typed.result.failure, depth+1, invocationBound, computed)
		}
		if typed.callbacks != nil {
			computed.Spawns = saturatingAdd(computed.Spawns, spawnInvocationBound)
			if typed.spawn != nil && typed.spawn.kind == "area" && typed.spawn.area != nil {
				members := saturatingMul(spawnInvocationBound, typed.spawn.area.limit)
				steps := areaStepBound(typed.spawn.durationTicks, typed.spawn.intervalTicks)
				callbackBound := saturatingMul(members, steps)
				for _, callback := range []flowIR{typed.callbacks.leave, typed.callbacks.enter, typed.callbacks.tick} {
					measureFlowBudget(callback, depth+1, callbackBound, computed)
				}
				walkPhaseFlows(phaseEventsIR{cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition}, func(child flowIR) {
					measureFlowBudget(child, depth+1, spawnInvocationBound, computed)
				})
			} else {
				walkPhaseFlows(phaseEventsIR{enter: typed.callbacks.enter, cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition, release: typed.callbacks.targetLost, pulse: typed.callbacks.tick}, func(child flowIR) {
					measureFlowBudget(child, depth+1, invocationBound, computed)
				})
				measureFlowBudget(typed.callbacks.leave, depth+1, invocationBound, computed)
			}
		}
	}
}

func areaStepBound(duration, interval Tick) int {
	if duration <= 0 || interval <= 0 {
		return maxIntValue()
	}
	steps := duration / interval
	if steps > Tick(maxIntValue()) {
		return maxIntValue()
	}
	return saturatingAdd(int(steps), boolInt(duration%interval != 0))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func checkBudget(context *compileContext, name string, actual, maximum int) {
	if maximum >= 0 && actual > maximum {
		context.addDiagnostic(DiagnosticBudgetExceeded, "$", fmt.Sprintf("%s budget %d exceeds limit %d", name, actual, maximum))
	}
}

func saturatingAdd(left, right int) int {
	maximum := maxIntValue()
	if left < 0 || right < 0 || left > maximum-right {
		return maximum
	}
	return left + right
}

func saturatingMul(left, right int) int {
	maximum := maxIntValue()
	if left < 0 || right < 0 {
		return maximum
	}
	if left == 0 || right == 0 {
		return 0
	}
	if left > maximum/right {
		return maximum
	}
	return left * right
}

func saturatingTickAdd(left, right Tick) Tick {
	const maximum = Tick(int64(^uint64(0) >> 1))
	if left < 0 || right < 0 || left > maximum-right {
		return maximum
	}
	return left + right
}

func maxIntValue() int { return int(^uint(0) >> 1) }
