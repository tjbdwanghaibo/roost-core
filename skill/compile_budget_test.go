package skill

import (
	"strings"
	"testing"
)

func TestBudgetRejectsRepeatAboveLimit(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"clear_memory","name":"x"}}},{"flow":"finish"}]}`, 1)
	environment := DefaultCompileEnvironment()
	environment.Limits.MaxRepeat = 2
	environment.Digest = authorityDigest(environment)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), environment)
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticBudgetExceeded)
}
