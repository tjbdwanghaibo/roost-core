package skill

import (
	"strings"
	"testing"
)

func TestProcRejectsRequiredExcludedTagConflict(t *testing.T) {
	input := passiveSkillJSON(1, `["spell"]`, `["spell"]`)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticEventFilterConflict)
}

func TestProcRejectsDepthAboveEnvironmentLimit(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Limits.MaxProcDepth = 1
	environment.Digest = authorityDigest(environment)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, passiveSkillJSON(2, `[]`, `[]`)), environment)
	requireDiagnostic(t, diagnostics, DiagnosticProcLimitExceeded)
}

func TestProcResolvesElementAndDamageTypeHandles(t *testing.T) {
	input := passiveSkillJSON(1, `[]`, `[]`)
	input = strings.Replace(input, `"elements":[]`, `"elements":["fire"]`, 1)
	input = strings.Replace(input, `"damage_types":[]`, `"damage_types":["magic"]`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if artifacts.proc.plan == nil || len(artifacts.proc.plan.Filter.Elements) != 1 || artifacts.proc.plan.Filter.Elements[0] != ElementHandle(2) {
		t.Fatalf("element plan = %#v", artifacts.proc.plan)
	}
	if len(artifacts.proc.plan.Filter.DamageTypes) != 1 || artifacts.proc.plan.Filter.DamageTypes[0] != DamageTypeHandle(2) {
		t.Fatalf("damage type plan = %#v", artifacts.proc.plan)
	}
}

func passiveSkillJSON(maxDepth int, required, excluded string) string {
	return `{
  "schema":"roost.skill/v2","id":"skill.test.passive","name":"Passive","description":"Runs on damage.",
  "activation":{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":` + required + `,"excluded_tags":` + excluded + `,"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":` + intString(maxDepth) + `,"allow_self_trigger":false,"once_per_root_event":true}},
  "input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"finish","reason":"done"}}}]
}`
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(rune('0'+value%10)) + result
		value /= 10
	}
	return result
}
