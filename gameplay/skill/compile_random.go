package skill

func collectRandomSites(flow flowIR, invocationBound int, artifact *identityArtifact) {
	if flow == nil {
		return
	}
	switch typed := flow.(type) {
	case *sequenceFlowIR:
		for _, child := range typed.steps {
			collectRandomSites(child, invocationBound, artifact)
		}
	case *parallelFlowIR:
		for _, branch := range typed.branches {
			collectRandomSites(branch, invocationBound, artifact)
		}
	case *ifFlowIR:
		collectRandomSites(typed.thenFlow, invocationBound, artifact)
		collectRandomSites(typed.elseFlow, invocationBound, artifact)
	case *repeatFlowIR:
		collectRandomSites(typed.body, saturatingMul(invocationBound, literalRepeat(typed.times)), artifact)
	case *waitFlowIR:
		collectRandomSites(typed.then, invocationBound, artifact)
	case *selectFlowIR:
		if typed.selectPlan.order != nil && typed.selectPlan.order.by == "random" {
			artifact.RandomSites = append(artifact.RandomSites, RandomSite{
				ID:              len(artifact.RandomSites),
				Path:            typed.selectPlan.source.Path + ".order",
				InvocationBound: invocationBound,
			})
		}
		consumerBound := saturatingMul(invocationBound, maxInt(1, typed.selectPlan.limit))
		switch consume := typed.consume.(type) {
		case *selectOneConsumeIR:
			collectRandomSites(consume.then, invocationBound, artifact)
		case *selectEachConsumeIR:
			collectRandomSites(consume.body, consumerBound, artifact)
		}
		collectRandomSites(typed.onEmpty, invocationBound, artifact)
	case *effectFlowIR:
		if typed.result != nil {
			collectRandomSites(typed.result.success, invocationBound, artifact)
			collectRandomSites(typed.result.failure, invocationBound, artifact)
		}
		if typed.callbacks != nil {
			walkPhaseFlows(phaseEventsIR{enter: typed.callbacks.enter, cancel: typed.callbacks.cancel, timeout: typed.callbacks.end, recast: typed.callbacks.hit, directionChanged: typed.callbacks.collision, targetChanged: typed.callbacks.transition, release: typed.callbacks.targetLost, pulse: typed.callbacks.tick}, func(child flowIR) {
				collectRandomSites(child, invocationBound, artifact)
			})
			collectRandomSites(typed.callbacks.leave, invocationBound, artifact)
		}
	}
}
