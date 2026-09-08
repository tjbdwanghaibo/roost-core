package skill

import (
	"fmt"
	"regexp"
)

var visualKeywordPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func runVisualPass(context *compileContext) {
	artifact := visualArtifact{bySourcePath: make(map[string]VisualIndex)}
	referenceCount := 0
	intern := func(visual *visualIR, mount, path string) (VisualIndex, bool) {
		referenceCount++
		if !validateVisual(context, visual, mount, path) {
			return 0, false
		}
		for _, entry := range artifact.entries {
			if entry.category == visual.category && entry.theme == visual.theme && sameVisualElements(entry.elements, visual.elements) {
				return entry.index, true
			}
		}
		if len(artifact.entries) >= context.environment.Visual.Limits.MaxManifestEntries {
			context.addDiagnostic(DiagnosticVisualInvalid, path, "visual manifest exceeds catalog limit")
			return 0, false
		}
		index := VisualIndex(len(artifact.entries))
		artifact.entries = append(artifact.entries, visualProgram{index: index, category: visual.category, theme: visual.theme, elements: append([]string(nil), visual.elements...)})
		return index, true
	}
	if presentation := context.artifacts.ir.presentation; presentation != nil {
		validateIconKeywords(context, presentation.iconKeywords)
		if presentation.cast != nil {
			artifact.castIndex, artifact.hasCast = intern(presentation.cast, "cast", "$.presentation.cast")
		}
	}
	context.artifacts.ir.walkEffects(func(effect effectIR) {
		visual := effectVisual(effect)
		if visual == nil {
			return
		}
		index, ok := intern(visual, visualEffectKind(effect), effect.sourceRef().Path+".visual")
		if ok {
			artifact.bySourcePath[effect.sourceRef().Path] = index
		}
	})
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		effectFlow, ok := flow.(*effectFlowIR)
		if !ok || effectFlow.process == nil || effectFlow.process.visual == nil {
			return
		}
		process := effectFlow.process
		index, valid := intern(process.visual, process.kind, process.source.Path+".visual")
		if valid {
			artifact.bySourcePath[process.source.Path] = index
		}
	})
	if referenceCount > context.environment.Visual.Limits.MaxVisualRefs {
		context.addDiagnostic(DiagnosticVisualInvalid, "$.visual", "visual references exceed catalog limit")
	}
	context.artifacts.visual = artifact
}

func validateVisual(context *compileContext, visual *visualIR, mount, path string) bool {
	limits := context.environment.Visual.Limits
	if visual == nil || visual.category == "" || visual.theme == "" || len(visual.elements) == 0 || len(visual.elements) > limits.MaxElementsPerRef || len(visual.category) > limits.MaxCategoryBytes || len(visual.theme) > limits.MaxThemeBytes {
		context.addDiagnostic(DiagnosticVisualInvalid, path, "visual category, theme, and bounded elements are required")
		return false
	}
	category, found := context.environment.Visual.Categories[visual.category]
	if !found {
		context.addDiagnostic(DiagnosticVisualInvalid, path+".category", "unknown visual category")
		return false
	}
	theme, found := category.Themes[visual.theme]
	if !found {
		context.addDiagnostic(DiagnosticVisualInvalid, path+".theme", "unknown visual theme")
		return false
	}
	if !containsVisualString(theme.AllowedEffects, mount) {
		context.addDiagnostic(DiagnosticVisualInvalid, path, fmt.Sprintf("visual category %q is incompatible with %s", visual.category, mount))
		return false
	}
	if theme.RequiredElements != len(visual.elements) {
		context.addDiagnostic(DiagnosticVisualInvalid, path+".elements", "visual theme requires a different element count")
		return false
	}
	seen := make(map[string]bool, len(visual.elements))
	for _, element := range visual.elements {
		if len(element) == 0 || len(element) > limits.MaxElementBytes || seen[element] || !containsVisualString(theme.AllowedElements, element) {
			context.addDiagnostic(DiagnosticVisualInvalid, path+".elements", "unknown, duplicate, or incompatible visual element")
			return false
		}
		if _, found := context.environment.Visual.Elements[element]; !found {
			context.addDiagnostic(DiagnosticVisualInvalid, path+".elements", "unknown visual element")
			return false
		}
		seen[element] = true
	}
	return true
}

func validateIconKeywords(context *compileContext, values []string) {
	if len(values) == 0 {
		return
	}
	limits := context.environment.Visual.Limits
	if len(values) < 3 || len(values) > limits.MaxIconKeywords {
		context.addDiagnostic(DiagnosticVisualInvalid, "$.presentation.icon_keywords", "icon keywords require 3 to 5 values")
		return
	}
	reserved := map[string]bool{"skill": true, "effect": true, "entity": true, "projectile": true, "spawn": true, "trigger": true, "aoe": true}
	seen := map[string]bool{}
	for _, value := range values {
		if len(value) < 2 || len(value) > limits.MaxKeywordBytes || !visualKeywordPattern.MatchString(value) || reserved[value] || seen[value] {
			context.addDiagnostic(DiagnosticVisualInvalid, "$.presentation.icon_keywords", "invalid icon keyword")
			return
		}
		seen[value] = true
	}
}

func visualEffectKind(effect effectIR) string {
	switch effect.(type) {
	case *damageEffectIR:
		return "damage"
	case *healEffectIR:
		return "heal"
	case *shieldEffectIR:
		return "shield"
	case *addStatusEffectIR, *removeStatusEffectIR:
		return "status"
	case *attributeModifierEffectIR:
		return "attribute_modifier"
	case *resourceEffectIR:
		return "resource"
	case *teleportEffectIR:
		return "teleport"
	case *knockbackEffectIR:
		return "knockback"
	case *pullEffectIR:
		return "pull"
	case *stopMovementEffectIR:
		return "stop_movement"
	default:
		return "forbidden"
	}
}
func containsVisualString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
func sameVisualElements(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
