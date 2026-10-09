package skill

import (
	"strings"
	"testing"
)

func TestCompileRejectsDamageToPosition(t *testing.T) {
	data := string(mustReadFixture(t, "simple_damage.json"))
	data = strings.Replace(data, `"type": "entity"`, `"type": "position"`, 1)
	data = strings.Replace(data, `"target": "$input.target"`, `"target": "$input.position"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)
}

func TestCompileRejectsUnknownLocal(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"$input.target"`, `"$local.missing"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticReferenceUnknown)
}

func TestCompileRejectsDamageAmountWithWrongType(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount": true`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)
}

func TestCompileRejectsInputReferenceOutsideSchema(t *testing.T) {
	data := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"type": "entity"`, `"type": "none"`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticInputUnavailable)
}
