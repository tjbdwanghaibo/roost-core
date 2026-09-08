package skill

import (
	"strings"
	"testing"
)

func TestCompileRejectsExistsOnNonOptionalValue(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"if","condition":{"op":"exists","args":["$caster"]},"then":{"flow":"finish"},"else":{"flow":"finish"}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticOptionalInvalid)
}
