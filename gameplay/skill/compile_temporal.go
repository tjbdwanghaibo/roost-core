package skill

func runTemporalPass(context *compileContext) {
	profiles := make(map[string]TemporalSnapshotProfile, len(context.environment.Gameplay.Temporal.Entries))
	for _, profile := range context.environment.Gameplay.Temporal.Entries {
		profiles[profile.Key] = profile
	}
	artifact := temporalArtifact{profiles: make(map[string]resolvedTemporalProfile)}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		effectFlow, ok := flow.(*effectFlowIR)
		if !ok {
			return
		}
		switch effect := effectFlow.effect.(type) {
		case *captureSnapshotEffectIR:
			profile, found := profiles[effect.profile]
			if !found || profile.Handle == 0 {
				context.addDiagnostic(DiagnosticCapabilityUnknown, effect.source.Path+".profile", "unknown temporal snapshot profile")
				return
			}
			artifact.profiles[effect.source.Path] = resolvedTemporalProfile{handle: profile.Handle, fields: append([]string(nil), profile.Fields...), allowRevive: profile.AllowRevive, eventPolicy: profile.EventPolicy, blockedPositionPolicy: profile.BlockedPositionPolicy}
		case *restoreSnapshotEffectIR:
			if effect.onBlocked != "" && !validTemporalBlockedPositionPolicy(effect.onBlocked) {
				context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".on_blocked", "invalid temporal blocked-position policy")
			}
		}
	})
	context.artifacts.temporal = artifact
}
