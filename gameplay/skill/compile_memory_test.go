package skill

import "testing"

const nullableMemorySkillPrefix = `{"schema":"roost.skill/v2","id":"skill.test.memory","name":"Memory","description":"Tests memory.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"anchor":{"type":"entity","default":null}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":`

func TestCompileRejectsMaybeInitializedMemoryRead(t *testing.T) {
	input := nullableMemorySkillPrefix + `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts != nil {
		t.Fatal("expected nil artifacts")
	}
	requireDiagnostic(t, diagnostics, DiagnosticMemoryMaybeUninitialized)
}

func TestCompileAcceptsGuardedMemoryRead(t *testing.T) {
	input := nullableMemorySkillPrefix + `{"flow":"if","condition":{"op":"exists","args":["$memory.anchor"]},"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]},"else":{"flow":"finish"}}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected guarded compile, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}

func TestCompileAcceptsInitializedMemoryRead(t *testing.T) {
	input := `{"schema":"roost.skill/v2","id":"skill.test.memory.initialized","name":"Memory","description":"Tests initialized memory.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"anchor":{"type":"entity","default":"$caster"}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$memory.anchor","amount":10,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	artifacts, diagnostics := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	if artifacts == nil {
		t.Fatalf("expected initialized memory compile, diagnostics=%#v", diagnostics)
	}
	requireNoErrors(t, diagnostics)
}
