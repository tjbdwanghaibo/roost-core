package skill

import (
	"strings"
	"testing"
)

func TestCompileRejectsQuantityMismatchInDamageAmount(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"move_speed","snapshot":"current"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticQuantityMismatch)
}

func TestCompileAcceptsCombatAmountAttributeForDamage(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount":{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"current"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected artifacts, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}
