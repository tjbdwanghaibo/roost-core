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
			validateCatalogReferences(context, authority)
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
		case *statusFilterIR:
			// has_status / missing_status：lower 用 lookupStatusHandle 的 map 零值，未知名字
			// 会变成 handle 0，filter 静默按不存在的状态过滤（RR-20261005-NC-214）。
			if _, found := context.artifacts.authority.statuses[typed.status]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".status", "unknown status")
			}
		case *attributeCompareFilterIR:
			if _, found := context.artifacts.authority.attributes[typed.attribute]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".attribute", "unknown attribute")
			}
			// Host 对未知比较运算一律判不匹配，filter 静默全部落空（RR-20261005-NC-215）。
			if !validCompareOperation(typed.op) {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".op", "attribute comparison must be eq, ne, lt, lte, gt, or gte")
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

// validateCatalogReferences 检查 effect 与 cost 里按名字引用的 status / attribute /
// resource 都在 catalog 里（RR-20261005-NC-214）。lower 的 lookupStatusHandle /
// lookupAttributeHandle / lookupResourceHandle 对未知名字返回 map 零值 handle 0：
// add_status、resource、cost 每次施法被 Host 拒绝，remove_status、attribute_modifier
// 静默作用在 handle 0 上。damage_type、element、gameplay tag、collision、unit template
// 已在各自的 pass 里检查；filter 里的名字由 validateTargetFilterList 检查。
//
// attribute_modifier 的 operation 同时要在该属性 catalog 的 ModifierOperations 里，
// 并且是 Host 实现的 add / mul_bp（RR-20261005-NC-215）：两个参考 Host 对其它取值
// 一律返回错误，ModifierOperations 此前没有读取方。
func validateCatalogReferences(context *compileContext, authority authorityArtifact) {
	ir := context.artifacts.ir
	checkResource := func(name, path string) {
		if _, found := authority.resources[name]; !found {
			context.addDiagnostic(DiagnosticCapabilityUnknown, path, "unknown resource")
		}
	}
	for _, cost := range ir.costs {
		checkResource(cost.resource, cost.source.Path+".resource")
	}
	for _, cost := range ir.activation.policy.sustainCosts {
		checkResource(cost.resource, cost.source.Path+".resource")
	}
	attributes := make(map[string]AttributeCatalogEntry, len(context.environment.Gameplay.Attributes.Entries))
	for _, entry := range context.environment.Gameplay.Attributes.Entries {
		attributes[entry.Key] = entry
	}
	ir.walkEffects(func(effect effectIR) {
		switch typed := effect.(type) {
		case *addStatusEffectIR:
			if _, found := authority.statuses[typed.status]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".status", "unknown status")
			}
		case *removeStatusEffectIR:
			if _, found := authority.statuses[typed.status]; !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".status", "unknown status")
			}
		case *resourceEffectIR:
			checkResource(typed.resource, typed.source.Path+".resource")
		case *attributeModifierEffectIR:
			entry, found := attributes[typed.attribute]
			if !found {
				context.addDiagnostic(DiagnosticCapabilityUnknown, typed.source.Path+".attribute", "unknown attribute")
				return
			}
			if (typed.operation != "add" && typed.operation != "mul_bp") || !containsString(entry.ModifierOperations, typed.operation) {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".operation", "modifier operation must be add or mul_bp and allowed by the attribute catalog")
			}
		}
	})
}
