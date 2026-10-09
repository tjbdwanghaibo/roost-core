package skill

import "sort"

func runGameplayTagsPass(context *compileContext) {
	tagByKey := map[string]GameplayTagCatalogEntry{}
	for _, entry := range context.environment.Gameplay.Tags.Entries {
		tagByKey[entry.Key] = entry
	}
	damageByKey := map[string]DamageTypeCatalogEntry{}
	for _, entry := range context.environment.Gameplay.DamageTypes.Entries {
		damageByKey[entry.Key] = entry
	}
	elementByKey := map[string]ElementCatalogEntry{}
	for _, entry := range context.environment.Gameplay.Elements.Entries {
		elementByKey[entry.Key] = entry
	}
	context.artifacts.gameplay.damage = make(map[string]resolvedDamageSemantics)
	for index, key := range context.artifacts.ir.gameplayTags {
		entry, ok := tagByKey[key]
		path := "$.gameplay_tags[" + intToDecimal(index) + "]"
		if !ok {
			context.addDiagnostic(DiagnosticCapabilityUnknown, path, "unknown gameplay tag")
			continue
		}
		if entry.Classes&GameplayTagDeclarable == 0 {
			context.addDiagnostic(DiagnosticGameplayTagPermission, path, "tag is not declarable")
			continue
		}
		context.artifacts.gameplay.skillTags = append(context.artifacts.gameplay.skillTags, entry.Handle)
	}
	context.artifacts.gameplay.skillTags = sortedUniqueTagHandles(context.artifacts.gameplay.skillTags)
	context.artifacts.ir.walkEffects(func(effect effectIR) {
		damage, ok := effect.(*damageEffectIR)
		if !ok {
			return
		}
		path := damage.source.Path
		damageEntry, ok := damageByKey[damage.damageType]
		if !ok {
			context.addDiagnostic(DiagnosticCapabilityUnknown, path+".damage_type", "unknown damage type")
			return
		}
		elementKey := damage.element
		if elementKey == "" {
			elementKey = "neutral"
		}
		elementEntry, ok := elementByKey[elementKey]
		if !ok {
			context.addDiagnostic(DiagnosticCapabilityUnknown, path+".element", "unknown element")
			return
		}
		if damage.damageType == "true" && elementKey != "neutral" {
			context.addDiagnostic(DiagnosticGameplayElementInvalid, path+".element", "true damage must use neutral element")
		}
		tags := make([]GameplayTagHandle, 0, len(damage.combatTags))
		for index, key := range damage.combatTags {
			entry, ok := tagByKey[key]
			tagPath := path + ".combat_tags[" + intToDecimal(index) + "]"
			if !ok {
				context.addDiagnostic(DiagnosticCapabilityUnknown, tagPath, "unknown gameplay tag")
				continue
			}
			if entry.Classes&GameplayTagDeclarable == 0 {
				context.addDiagnostic(DiagnosticGameplayTagPermission, tagPath, "combat tag is not declarable")
				continue
			}
			tags = append(tags, entry.Handle)
		}
		context.artifacts.gameplay.damage[path] = resolvedDamageSemantics{DamageType: damageEntry.Handle, Element: elementEntry.Handle, CombatTags: sortedUniqueTagHandles(tags)}
	})
}

func sortedUniqueTagHandles(values []GameplayTagHandle) []GameplayTagHandle {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
func intToDecimal(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}
