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
	// clear_memory 的名字必须声明（RR-20261005-NC-210），否则类型检查先于生命期 pass 拒绝。
	input = strings.Replace(input, `"memory":{}`, `"memory":{"x":{"type":"int","default":0}}`, 1)
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticLifecycleControlConflict)
}
