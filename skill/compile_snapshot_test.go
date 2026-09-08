package skill

import (
	"strings"
	"testing"
)

func TestAttributeSnapshotResolvesHandleAndPoint(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"cast_start"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	plan := artifacts.snapshots.reads["$.phases[0].on.enter.steps[0].effect.amount"]
	if plan.Attribute != AttributeHandle(2) || plan.Snapshot != snapshotCastStart {
		t.Fatalf("snapshot plan = %#v", plan)
	}
}

func TestAttributeSnapshotRejectsUnsupportedPoint(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"health","snapshot":"each_tick"}}`, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticAttributeSnapshotInvalid)
}
