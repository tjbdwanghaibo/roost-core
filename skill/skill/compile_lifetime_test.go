package skill

import (
	"strings"
	"testing"
)

func TestCompileRejectsFallthroughEnterWithoutTimeout(t *testing.T) {
	data := strings.Replace(strings.ReplaceAll(string(mustReadFixture(t, "simple_damage.json")), "\r\n", "\n"), `,
            {
              "flow": "finish",
              "reason": "done"
            }`, "", 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, data), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticLifecycleFallthrough)
}

func TestCompileRejectsParallelFinish(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"parallel","branches":[{"flow":"finish"},{"flow":"effect","effect":{"type":"clear_memory","name":"x"}}]}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticLifecycleControlConflict)
}
