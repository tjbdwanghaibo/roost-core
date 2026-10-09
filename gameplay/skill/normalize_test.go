package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalSkillWithoutPolicy = `{
  "schema":"roost.skill/v2",
  "id":"skill.test.default_policy",
  "name":"Default Policy",
  "description":"Uses the default tap policy.",
  "activation":{"type":"active"},
  "input_schema":{"type":"none"},
  "cooldown_ticks":0,
  "costs":[],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"finish","reason":"done"}}}]
}`

func TestNormalizeCreatesTypedDamageIR(t *testing.T) {
	definition := mustParseFixture(t, "simple_damage.json")
	ir, _, diagnostics := normalizeDefinition(definition)
	requireNoErrors(t, diagnostics)
	effect := firstEffectIR(t, ir)
	if _, ok := effect.(*damageEffectIR); !ok {
		t.Fatalf("got %T, want *damageEffectIR", effect)
	}
}

func TestNormalizeDefaultsTapPolicy(t *testing.T) {
	definition := mustParseJSON(t, minimalSkillWithoutPolicy)
	ir, _, diagnostics := normalizeDefinition(definition)
	requireNoErrors(t, diagnostics)
	if ir.activation.policy.mode != castModeTap {
		t.Fatalf("got %q, want tap", ir.activation.policy.mode)
	}
}

func TestNormalizeWalkValuesVisitsEachSourceOnce(t *testing.T) {
	input := `{
  "schema":"roost.skill/v2",
  "id":"skill.test.visitor",
  "name":"Visitor",
  "description":"Exercises value traversal.",
  "activation":{"type":"active","policy":{"mode":"tap"}},
  "input_schema":{"type":"none"},
  "cooldown_ticks":0,
  "costs":[{"resource":"mana","amount":7}],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{
    "flow":"select",
    "select":{"from":"$caster","kind":"entity","shape":{"type":"cone","range":6,"angle_deg":45000,"direction":"$input.direction"},"filters":[],"limit":1},
    "consume":{"mode":"one","as":"target","then":{"flow":"effect","effect":{"type":"damage","target":"$local.target","amount":11,"damage_type":"magic"}}},
    "on_empty":{"flow":"finish","reason":"empty"}
  }}}]
}`
	ir, _, diagnostics := normalizeDefinition(mustParseJSON(t, input))
	requireNoErrors(t, diagnostics)

	want := map[string]bool{
		"$.costs[0].amount":                               true,
		"$.phases[0].on.enter.select.from":                true,
		"$.phases[0].on.enter.select.shape.range":         true,
		"$.phases[0].on.enter.select.shape.angle_deg":     true,
		"$.phases[0].on.enter.select.shape.direction":     true,
		"$.phases[0].on.enter.consume.then.effect.target": true,
		"$.phases[0].on.enter.consume.then.effect.amount": true,
	}
	seen := make(map[string]int)
	ir.walkValues(func(value valueIR) { seen[value.sourceRef().Path]++ })
	for path := range want {
		if seen[path] != 1 {
			t.Fatalf("value path %s visited %d times, want once; all=%v", path, seen[path], seen)
		}
	}
	for path, count := range seen {
		if count != 1 {
			t.Fatalf("value path %s visited %d times", path, count)
		}
	}
}

func TestNormalizeDoesNotRetainMutableWireData(t *testing.T) {
	definition := mustParseFixture(t, "simple_damage.json")
	definition.GameplayTags = []string{"spell"}
	ir, _, diagnostics := normalizeDefinition(definition)
	requireNoErrors(t, diagnostics)

	definition.ID = "skill.changed"
	definition.GameplayTags[0] = "changed"
	definition.Phases[0].ID = "changed"

	if ir.id != "skill.test.simple_damage" {
		t.Fatalf("IR id changed to %q", ir.id)
	}
	if len(ir.gameplayTags) != 1 || ir.gameplayTags[0] != "spell" {
		t.Fatalf("IR tags changed to %v", ir.gameplayTags)
	}
	if ir.phases[0].id != "cast" {
		t.Fatalf("IR phase id changed to %q", ir.phases[0].id)
	}
}

func mustParseFixture(t *testing.T, name string) *Definition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func requireNoErrors(t *testing.T, diagnostics []Diagnostic) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticError {
			t.Fatalf("unexpected diagnostic: %#v", diagnostic)
		}
	}
}

func firstEffectIR(t *testing.T, ir *skillIR) effectIR {
	t.Helper()
	var result effectIR
	ir.walkEffects(func(effect effectIR) {
		if result == nil {
			result = effect
		}
	})
	if result == nil {
		t.Fatal("no effect IR found")
	}
	return result
}

func TestNormalizeRejectsInvalidIdentifiersWithSourcePath(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, "skill.test.minimal", "Skill Invalid", 1)
	_, _, diagnostics := normalizeDefinition(mustParseJSON(t, input))
	if len(diagnostics) == 0 || diagnostics[0].Path != "$.id" {
		t.Fatalf("diagnostics = %#v, want $.id error", diagnostics)
	}
}

func TestNormalizeSourceMapIncludesSelectVariantNodes(t *testing.T) {
	input := `{
  "schema":"roost.skill/v2","id":"skill.test.source_map","name":"Source Map","description":"Tracks variant nodes.",
  "activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},
  "cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{
    "flow":"select",
    "select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":5},"filters":[{"type":"relation","value":"enemy"}],"limit":1},
    "consume":{"mode":"one","as":"target","then":{"flow":"finish","reason":"done"}},
    "on_empty":{"flow":"finish","reason":"empty"}
  }}}]
}`
	_, sources, diagnostics := normalizeDefinition(mustParseJSON(t, input))
	requireNoErrors(t, diagnostics)
	seen := make(map[string]bool)
	for _, ref := range sources.refs {
		seen[ref.Path] = true
	}
	for _, path := range []string{
		"$.phases[0].on.enter.select.shape",
		"$.phases[0].on.enter.select.filters[0]",
		"$.phases[0].on.enter.consume",
	} {
		if !seen[path] {
			t.Errorf("source map missing %s", path)
		}
	}
}
