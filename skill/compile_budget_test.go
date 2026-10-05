package skill

import (
	"strings"
	"testing"
)

func TestBudgetRejectsRepeatAboveLimit(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"clear_memory","name":"x"}}},{"flow":"finish"}]}`, 1)
	// clear_memory 的名字必须声明（RR-20261005-NC-210），否则类型检查先于预算 pass 拒绝。
	input = strings.Replace(input, `"memory":{}`, `"memory":{"x":{"type":"int","default":0}}`, 1)
	environment := DefaultCompileEnvironment()
	environment.Limits.MaxRepeat = 2
	environment.Digest = authorityDigest(environment)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), environment)
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticBudgetExceeded)
}
