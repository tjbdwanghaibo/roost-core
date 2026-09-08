package skill

import (
	"reflect"
	"strings"
	"testing"
)

func TestVisualManifestCompilesCanonicalVisual(t *testing.T) {
	input := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"activation": {`, `"presentation":{"icon_keywords":["flare","blade","spark"],"cast":{"category":"cast","theme":"default","elements":["default"]}},"activation": {`, 1)
	input = strings.Replace(input, `"damage_type": "physical"`, `"damage_type": "physical","visual":{"category":"impact","theme":"default","elements":["default"]}`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))
	manifest := InspectVisualManifest(program)
	if len(manifest.Entries) != 2 {
		t.Fatalf("manifest entries = %#v, want cast and effect visuals", manifest.Entries)
	}
	if manifest.Digest == "" {
		t.Fatal("manifest digest is empty")
	}
}

func TestVisualRejectsInvalidSchemaAndCatalogBindings(t *testing.T) {
	valid := visualDamageJSON(t, `{"category":"impact","theme":"default","elements":["default"]}`)
	for name, input := range map[string]string{
		"unknown_category":  strings.Replace(valid, `"category":"impact"`, `"category":"beam"`, 1),
		"duplicate_element": strings.Replace(valid, `["default"]`, `["default","default"]`, 1),
		"wrong_count":       strings.Replace(valid, `["default"]`, `["default","fire"]`, 1),
		"invalid_keyword":   strings.Replace(valid, `"description":`, `"presentation":{"icon_keywords":["skill","blade","spark"]},"description":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticVisualInvalid)
		})
	}
	invalidWire := strings.Replace(valid, `"theme":"default"`, `"theme":"default","asset_path":"client/prefab"`, 1)
	if _, err := Parse([]byte(invalidWire)); err == nil {
		t.Fatal("asset_path must be rejected by strict visual wire decoding")
	}
}

func TestVisualTableDeduplicatesAndDoesNotChangeGameplayIdentity(t *testing.T) {
	visual := `{"category":"impact","theme":"default","elements":["default"]}`
	with := visualDamageJSON(t, visual)
	withVisual := mustCompileProgram(t, mustParseJSON(t, with))
	with = strings.ReplaceAll(with, " ", "")
	with = strings.ReplaceAll(with, "\n", "")
	with = strings.ReplaceAll(with, "\r", "")
	with = strings.Replace(with, `{"flow":"finish","reason":"done"}`, `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical","visual":`+visual+`}}, {"flow":"finish","reason":"done"}`, 1)
	if strings.Count(with, `"type":"damage"`) != 2 {
		t.Fatalf("test fixture contains %d damage effects", strings.Count(with, `"type":"damage"`))
	}
	program := mustCompileProgram(t, mustParseJSON(t, with))
	manifest := InspectVisualManifest(program)
	if len(manifest.Entries) != 1 {
		t.Fatalf("visual entries = %#v, want one canonical entry", manifest.Entries)
	}
	var indexes []VisualIndex
	for _, operation := range program.operations {
		continuations, _, ok := operationEffectContinuations(operation)
		if ok && continuations.hasVisual {
			indexes = append(indexes, continuations.visual)
		}
	}
	if !reflect.DeepEqual(indexes, []VisualIndex{0, 0}) {
		t.Fatalf("effect visual indexes = %v, want [0 0]", indexes)
	}
	without := mustCompileProgram(t, mustParseFixture(t, "simple_damage.json"))
	if InspectIdentity(without).GameplayDigest != InspectIdentity(withVisual).GameplayDigest {
		t.Fatal("visual-only change altered gameplay digest")
	}
	first := InspectVisualManifest(program)
	first.Entries[0].Elements[0] = "mutated"
	if InspectVisualManifest(program).Entries[0].Elements[0] != "default" {
		t.Fatal("manifest projection aliases immutable program data")
	}
}

func visualDamageJSON(t *testing.T, visual string) string {
	t.Helper()
	input := string(mustReadFixture(t, "simple_damage.json"))
	input = strings.Replace(input, `"damage_type": "physical"`, `"damage_type":"physical","visual":`+visual, 1)
	return input
}
