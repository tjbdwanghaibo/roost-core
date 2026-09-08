package skillcompose

import (
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-skill/skill"
)

func TestProfileExtractionIsStableAndUsesInspectorFacts(t *testing.T) {
	definition, err := skill.Parse([]byte(`{"schema":"roost.skill/v2","id":"skill.compose.chain","name":"Chain","description":"test","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$input.target","kind":"entity","shape":{"type":"chain","hop_range":1,"max_targets":1,"allow_repeat":false,"hop_interval_ticks":0},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":{"flow":"effect","effect":{"type":"damage","target":"$local.target","amount":1,"damage_type":"physical"}}},"on_empty":{"flow":"finish"}},{"flow":"finish"}]}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	program, diagnostics := skill.Compile(definition, skill.DefaultCompileEnvironment())
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == skill.DiagnosticError {
			t.Fatal(diagnostic)
		}
	}
	first, err := ExtractProfile(program)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExtractProfile(program)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("profiles differ: %#v %#v", first, second)
	}
	if !contains(first.Features, "select.chain") || !contains(first.Features, "effect.damage") {
		t.Fatalf("features = %#v", first.Features)
	}
}
func contains(values []FeatureKey, want FeatureKey) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
