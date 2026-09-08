package skill

func runAuthorityCapabilityPass(context *compileContext) {
	diagnostics := validateCompileEnvironment(context.environment)
	context.diagnostics = append(context.diagnostics, diagnostics...)
	if !context.hasErrors() {
		authority := authorityArtifact{
			identity:   AuthorityIdentity{Revision: context.environment.Revision, Digest: context.environment.Digest},
			attributes: make(map[string]AttributeHandle), resources: make(map[string]ResourceHandle),
			statuses: make(map[string]StatusHandle), collision: make(map[string]CollisionLayerHandle), tags: make(map[string]GameplayTagHandle), unitTemplates: make(map[string]UnitTemplateHandle),
		}
		for _, entry := range context.environment.Gameplay.Attributes.Entries {
			authority.attributes[entry.Key] = entry.Handle
		}
		for _, entry := range context.environment.Gameplay.Resources.Entries {
			authority.resources[entry.Key] = entry.Handle
		}
		for _, entry := range context.environment.Gameplay.Statuses.Entries {
			authority.statuses[entry.Key] = entry.Handle
		}
		for _, entry := range context.environment.Gameplay.Collision.Entries {
			authority.collision[entry.Key] = entry.Handle
		}
		for _, entry := range context.environment.Gameplay.Tags.Entries {
			authority.tags[entry.Key] = entry.Handle
		}
		for _, entry := range context.environment.Gameplay.UnitTemplates.Entries {
			authority.unitTemplates[entry.Key] = entry.Handle
		}
		context.artifacts.authority = authority
		if context.artifacts.ir != nil {
			for _, key := range context.artifacts.ir.activation.castWindow.interruptTags {
				if _, found := authority.tags[key]; !found {
					context.addDiagnostic(DiagnosticCapabilityUnknown, "$.activation.cast_window.interrupt_tags", "unknown interrupt tag")
				}
			}
			validateTargetFilters(context)
		}
		context.artifacts.processProperties = append([]ProcessPropertyPolicy(nil), context.environment.ProcessProperties.Properties...)
		runOwnedEntityPass(context)
		runStatusInstancePass(context)
	}
}

func validateTargetFilters(context *compileContext) {
	tags := make(map[string]GameplayTagCatalogEntry, len(context.environment.Gameplay.Tags.Entries))
	for _, entry := range context.environment.Gameplay.Tags.Entries {
		tags[entry.Key] = entry
	}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		if selected, ok := flow.(*selectFlowIR); ok {
			validateTargetFilterList(context, tags, selected.selectPlan.filters)
		}
		if effect, ok := flow.(*effectFlowIR); ok && effect.process != nil && effect.process.area != nil {
			validateTargetFilterList(context, tags, effect.process.area.filters)
		}
	})
}

func validateTargetFilterList(context *compileContext, tags map[string]GameplayTagCatalogEntry, filters []filterIR) {
	for _, filter := range filters {
		switch typed := filter.(type) {
		case *gameplayTagFilterIR:
			entry, found := tags[typed.tag]
			if !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".tag", "unknown gameplay tag")
			} else if entry.Classes&GameplayTagTargetQueryable == 0 {
				context.addDiagnostic(DiagnosticGameplayTagPermission, typed.source.Path+".tag", "gameplay tag is not target queryable")
			}
		case *lineOfSightFilterIR:
			if len(typed.collision) == 0 || !uniqueNonEmptyStrings(typed.collision) {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".collision", "line of sight collision layers must be a non-empty unique list")
				continue
			}
			for index, layer := range typed.collision {
				if _, found := context.artifacts.authority.collision[layer]; !found {
					context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".collision["+intToDecimal(index)+"]", "unknown collision layer")
				}
			}
		}
	}
}
