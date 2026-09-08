package skill

import "testing"

func TestCompileRejectsInvalidPhaseGraphs(t *testing.T) {
	tests := []struct {
		name, json string
		code       DiagnosticCode
	}{
		{"duplicate id", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}},{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseDuplicate},
		{"missing target", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"missing"}}}]`), DiagnosticPhaseTargetMissing},
		{"self cycle", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"a"}}}]`), DiagnosticPhaseCycle},
		{"multi phase cycle", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"b"}}},{"id":"b","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"a"}}}]`), DiagnosticPhaseCycle},
		{"unreachable", phaseSkillJSON("a", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}},{"id":"b","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseUnreachable},
		{"missing initial", phaseSkillJSON("missing", `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"finish"}}}]`), DiagnosticPhaseInitialMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, tt.json), DefaultCompileEnvironment())
			if artifacts != nil {
				t.Fatal("expected nil artifacts")
			}
			requireDiagnostic(t, diagnostics, tt.code)
		})
	}
}

func phaseSkillJSON(initial, phases string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.graph","name":"Graph","description":"Tests graph.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"` + initial + `","phases":` + phases + `}`
}
