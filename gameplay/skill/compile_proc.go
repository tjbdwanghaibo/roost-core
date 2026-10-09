package skill

func runEventProcPass(context *compileContext) {
	activation := context.artifacts.ir.activation
	if activation.kind == "active" {
		return
	}
	if activation.procPolicy.maxDepth > context.environment.Limits.MaxProcDepth {
		context.addDiagnostic(DiagnosticProcLimitExceeded, "$.activation.proc_policy.max_depth", "proc depth exceeds environment")
	}
	for _, required := range activation.eventFilter.requiredTags {
		if containsString(activation.eventFilter.excludedTags, required) {
			context.addDiagnostic(DiagnosticEventFilterConflict, "$.activation.event_filter", "required and excluded tags overlap")
		}
	}
	plan := ProcPlan{MaxDepth: activation.procPolicy.maxDepth, AllowSelfTrigger: activation.procPolicy.allowSelfTrigger, OncePerRootEvent: activation.procPolicy.oncePerRootEvent, MaxEventsPerRoot: context.environment.Limits.MaxEventsPerRoot}
	for _, key := range activation.eventFilter.requiredTags {
		if handle, ok := lookupTag(context.environment.Gameplay.Tags, key); ok {
			plan.Filter.RequiredTags = append(plan.Filter.RequiredTags, handle)
		} else {
			context.addDiagnostic(DiagnosticCapabilityUnknown, "$.activation.event_filter.required_tags", "unknown tag")
		}
	}
	for _, key := range activation.eventFilter.excludedTags {
		if handle, ok := lookupTag(context.environment.Gameplay.Tags, key); ok {
			plan.Filter.ExcludedTags = append(plan.Filter.ExcludedTags, handle)
		} else {
			context.addDiagnostic(DiagnosticCapabilityUnknown, "$.activation.event_filter.excluded_tags", "unknown tag")
		}
	}
	for _, key := range activation.eventFilter.elements {
		if handle, ok := lookupElement(context.environment.Gameplay.Elements, key); ok {
			plan.Filter.Elements = append(plan.Filter.Elements, handle)
		} else {
			context.addDiagnostic(DiagnosticCapabilityUnknown, "$.activation.event_filter.elements", "unknown element")
		}
	}
	for _, key := range activation.eventFilter.damageTypes {
		if handle, ok := lookupDamageType(context.environment.Gameplay.DamageTypes, key); ok {
			plan.Filter.DamageTypes = append(plan.Filter.DamageTypes, handle)
		} else {
			context.addDiagnostic(DiagnosticCapabilityUnknown, "$.activation.event_filter.damage_types", "unknown damage type")
		}
	}
	plan.Filter.Results = append([]string(nil), activation.eventFilter.results...)
	context.artifacts.proc.plan = &plan
}

func lookupTag(catalog GameplayTagCatalog, key string) (GameplayTagHandle, bool) {
	for _, entry := range catalog.Entries {
		if entry.Key == key {
			return entry.Handle, true
		}
	}
	return 0, false
}

func lookupElement(catalog ElementCatalog, key string) (ElementHandle, bool) {
	for _, entry := range catalog.Entries {
		if entry.Key == key {
			return entry.Handle, true
		}
	}
	return 0, false
}

func lookupDamageType(catalog DamageTypeCatalog, key string) (DamageTypeHandle, bool) {
	for _, entry := range catalog.Entries {
		if entry.Key == key {
			return entry.Handle, true
		}
	}
	return 0, false
}
