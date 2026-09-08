package skill

func runIdentityRandomPass(context *compileContext) {
	artifact := identityArtifact{}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		artifact.Operations = append(artifact.Operations, operationIdentity{
			Path:  flow.sourceRef().Path,
			Index: len(artifact.Operations),
		})
	})
	for _, phase := range context.artifacts.ir.phases {
		walkPhaseFlows(phase.events, func(flow flowIR) {
			collectRandomSites(flow, 1, &artifact)
		})
	}
	context.artifacts.identity = artifact
}
