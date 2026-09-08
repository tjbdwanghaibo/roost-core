package skill

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompilePassOrder(t *testing.T) {
	artifacts, diagnostics := compileToArtifacts(mustParseFixture(t, "simple_damage.json"), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	want := []string{"normalize", "shape", "authority_capability", "gameplay_tags", "input_state", "temporal", "type_snapshot", "optional_quantity", "effect_result_scope", "graph", "memory", "lifetime_ownership", "motion", "event_proc", "identity_random", "budget", "visual", "lower"}
	if !reflect.DeepEqual(artifacts.passOrder, want) {
		t.Fatalf("pass order = %v, want %v", artifacts.passOrder, want)
	}
	if !artifacts.lowerReady {
		t.Fatal("successful pipeline did not reach lower")
	}
}

func TestCompilePassOrderStopsAfterUpstreamError(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Revision = ""
	artifacts, diagnostics := compileToArtifactsInternal(mustParseFixture(t, "simple_damage.json"), environment)
	requireDiagnostic(t, diagnostics, DiagnosticEnvironmentInvalid)
	want := []string{"normalize", "shape", "authority_capability"}
	if !reflect.DeepEqual(artifacts.passOrder, want) {
		t.Fatalf("pass order = %v, want %v", artifacts.passOrder, want)
	}
	if artifacts.lowerReady {
		t.Fatal("failed pipeline returned lower-ready artifacts")
	}
}

func TestShapeRejectsSelectOneWithMultipleResults(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{
    "flow":"select",
    "select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":5},"filters":[],"limit":2},
    "consume":{"mode":"one","as":"target","then":{"flow":"finish","reason":"done"}}
  }`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticShapeInvalid)
}

func TestShapeAcceptsVisualAfterVisualPassIsDeployed(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"description":"Immediately finishes."`, `"description":"Immediately finishes.","presentation":{"icon_keywords":["flame","bolt","spark"],"cast":{"category":"cast","theme":"default","elements":["default"]}}`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
}
