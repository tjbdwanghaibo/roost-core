package skill

import (
	"reflect"
	"strings"
	"testing"
)

func TestProgramIdentityAndIndicesAreDeterministic(t *testing.T) {
	definition := mustParseFixture(t, "simple_damage.json")
	first := mustCompileProgram(t, definition)
	second := mustCompileProgram(t, definition)
	if !reflect.DeepEqual(Inspect(first), Inspect(second)) {
		t.Fatal("program views differ across identical compilations")
	}
	if InspectIdentity(first) != InspectIdentity(second) {
		t.Fatal("program identities differ across identical compilations")
	}
	if !reflect.DeepEqual(InspectRandomSites(first), InspectRandomSites(second)) {
		t.Fatal("random site indices differ across identical compilations")
	}
}

func TestProgramIdentitySeparatesGameplayAndPresentation(t *testing.T) {
	base := mustCompileProgram(t, mustParseFixture(t, "simple_damage.json"))
	presentationDefinition := mustParseFixture(t, "simple_damage.json")
	presentationDefinition.Name = "Renamed"
	presentation := mustCompileProgram(t, presentationDefinition)
	gameplayJSON := strings.Replace(string(mustReadFixture(t, "simple_damage.json")), `"amount": 10`, `"amount": 11`, 1)
	gameplay := mustCompileProgram(t, mustParseJSON(t, gameplayJSON))

	baseIdentity := InspectIdentity(base)
	presentationIdentity := InspectIdentity(presentation)
	gameplayIdentity := InspectIdentity(gameplay)
	if baseIdentity.GameplayDigest != presentationIdentity.GameplayDigest {
		t.Fatal("presentation-only name changed gameplay digest")
	}
	if baseIdentity.PresentationDigest == presentationIdentity.PresentationDigest || baseIdentity.SourceDocumentDigest == presentationIdentity.SourceDocumentDigest {
		t.Fatal("presentation/source digest did not observe name change")
	}
	if baseIdentity.GameplayDigest == gameplayIdentity.GameplayDigest {
		t.Fatal("damage amount change did not affect gameplay digest")
	}
}

func TestVisualCatalogIdentityDoesNotChangeGameplayDigest(t *testing.T) {
	definition := mustParseFixture(t, "simple_damage.json")
	baseEnvironment := DefaultCompileEnvironment()
	changedEnvironment := DefaultCompileEnvironment()
	changedEnvironment.Visual = changedEnvironment.Visual.WithTestRevision("visual-2")
	base, diagnostics := Compile(definition, baseEnvironment)
	requireNoErrors(t, diagnostics)
	changed, diagnostics := Compile(definition, changedEnvironment)
	requireNoErrors(t, diagnostics)
	baseIdentity, changedIdentity := InspectIdentity(base), InspectIdentity(changed)
	if baseIdentity.GameplayDigest != changedIdentity.GameplayDigest {
		t.Fatal("visual catalog changed gameplay digest")
	}
	if baseIdentity.PresentationDigest == changedIdentity.PresentationDigest {
		t.Fatal("visual catalog did not change presentation digest")
	}
}

func TestProgramIdentityIgnoresMemoryMapInsertionOrder(t *testing.T) {
	json := strings.Replace(minimalSkillJSON, `"memory":{}`, `"memory":{"alpha":{"type":"int","default":1},"beta":{"type":"int","default":2}}`, 1)
	firstDefinition := mustParseJSON(t, json)
	secondDefinition := mustParseJSON(t, json)
	alpha, beta := secondDefinition.Memory["alpha"], secondDefinition.Memory["beta"]
	secondDefinition.Memory = map[string]MemoryDeclaration{"beta": beta, "alpha": alpha}
	first := mustCompileProgram(t, firstDefinition)
	second := mustCompileProgram(t, secondDefinition)
	if InspectIdentity(first) != InspectIdentity(second) || !reflect.DeepEqual(Inspect(first), Inspect(second)) {
		t.Fatal("memory map insertion order changed program identity or indices")
	}
}
