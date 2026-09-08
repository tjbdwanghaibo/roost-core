package skill

import (
	"strings"
	"testing"
)

func TestProgramDoesNotAliasDefinition(t *testing.T) {
	withCost := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"costs": []`, `"costs": [{"resource":"mana","amount":5}]`, 1)
	definition := mustParseJSON(t, withCost)
	program := mustCompileProgram(t, definition)
	before := Inspect(program)
	definition.Name = "mutated"
	definition.GameplayTags = append(definition.GameplayTags, "mutated")
	definition.Costs[0].Resource = "mutated"
	definition.Phases[0].ID = "mutated"
	after := Inspect(program)
	if !programViewsEqual(before, after) {
		t.Fatalf("program changed after definition mutation: before=%#v after=%#v", before, after)
	}
}

func TestLowerProducesTypedDamageOperation(t *testing.T) {
	program := mustCompileProgram(t, mustParseFixture(t, "simple_damage.json"))
	found := false
	for _, operation := range program.operations {
		if _, ok := operation.(damageOperation); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("compiled program does not contain a typed damage operation")
	}
}

func TestLowerPreservesStableInputSelectionAndRandomTables(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":5},"filters":[],"order":{"by":"random","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`, 1)
	first := mustCompileProgram(t, mustParseJSON(t, input))
	second := mustCompileProgram(t, mustParseJSON(t, input))
	if !programViewsEqual(Inspect(first), Inspect(second)) {
		t.Fatal("selection program view is not deterministic")
	}
	if len(InspectSelections(first)) != 1 || len(InspectRandomSites(first)) != 1 {
		t.Fatalf("selection/random tables not lowered: selections=%#v random=%#v", InspectSelections(first), InspectRandomSites(first))
	}
}

func mustCompileProgram(t *testing.T, definition *Definition) *Program {
	t.Helper()
	program, diagnostics := Compile(definition, DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if program == nil {
		t.Fatal("Compile returned nil program")
	}
	return program
}
