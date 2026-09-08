package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func FuzzParseGeneratedNeverPanics(f *testing.F) {
	f.Add([]byte(minimalSkillJSON))
	f.Add([]byte(`{"schema":"roost.skill/v2","error":{"code":"UNSUPPORTED_CAPABILITY","message":"x","unsupported":[]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseGenerated(data)
	})
}

func FuzzRestoreRuntimeCheckpointNeverPanics(f *testing.F) {
	definition, err := ParseGenerated([]byte(minimalSkillJSON))
	if err != nil {
		f.Fatal(err)
	}
	environment := DefaultCompileEnvironment()
	if definition.Definition == nil {
		f.Fatal("minimal skill did not parse as a definition")
	}
	program, diagnostics := Compile(definition.Definition, environment)
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticError {
			f.Fatalf("compile diagnostic: %#v", diagnostic)
		}
	}
	host := NewMemoryHost(program.AuthorityIdentity())
	seed, err := NewRuntime(host, RuntimeOptions{}).Checkpoint()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed.Payload)
	f.Add([]byte(`{"world_revision":0}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		digest := sha256.Sum256(data)
		checkpoint := RuntimeCheckpoint{Version: RuntimeCheckpointVersion, Payload: append([]byte(nil), data...), Checksum: hex.EncodeToString(digest[:])}
		_, _ = RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	})
}
