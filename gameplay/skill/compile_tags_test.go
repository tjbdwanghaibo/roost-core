package skill

import (
	"strings"
	"testing"
)

func TestGameplayTagsResolveDeclarableHandles(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"activation"`, `"gameplay_tags":["spell","spell"],"activation"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if len(artifacts.gameplay.skillTags) != 1 || artifacts.gameplay.skillTags[0] != GameplayTagHandle(1) {
		t.Fatalf("resolved tags = %v, want [1]", artifacts.gameplay.skillTags)
	}
}

func TestGameplayTagsRejectCompilerAndRuntimeOnlyDeclarations(t *testing.T) {
	for _, tag := range []string{"projectile", "critical"} {
		t.Run(tag, func(t *testing.T) {
			input := strings.Replace(minimalSkillJSON, `"activation"`, `"gameplay_tags":["`+tag+`"],"activation"`, 1)
			_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticGameplayTagPermission)
		})
	}
}

func TestGameplayTagsDamageDefaultsToNeutralElement(t *testing.T) {
	artifacts, diagnostics := compileToArtifacts(mustParseFixture(t, "simple_damage.json"), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	damage := artifacts.gameplay.damage["$.phases[0].on.enter.steps[0].effect"]
	if damage.DamageType != DamageTypeHandle(1) || damage.Element != ElementHandle(1) {
		t.Fatalf("resolved damage = %#v, want physical/neutral handles", damage)
	}
}

func TestGameplayTagsRejectTrueDamageWithElement(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"damage_type": "physical"`, `"damage_type": "true", "element": "fire"`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticGameplayElementInvalid)
}
