package skill

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPresentationPlanExposesCastAndEffectMounts(t *testing.T) {
	input := visualDamageJSON(t, `{"category":"impact","theme":"default","elements":["default"]}`)
	input = strings.Replace(input, `"activation": {`, `"presentation":{"cast":{"category":"cast","theme":"default","elements":["default"]}},"activation": {`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))

	plan := InspectPresentationPlan(program)
	if plan.Cast == nil || len(plan.Effects) != 1 || !plan.Effects[0].HasEffect {
		t.Fatalf("presentation plan = %#v", plan)
	}
	if plan.Identity.PresentationDigest == "" || plan.Manifest.Digest == "" {
		t.Fatal("presentation plan identity is incomplete")
	}
	plan.Manifest.Entries[0].Elements[0] = "mutated"
	if InspectPresentationPlan(program).Manifest.Entries[0].Elements[0] == "mutated" {
		t.Fatal("presentation plan aliases immutable program storage")
	}
}

func TestRuntimePresentationEventsAreCommittedOrderedAndPollable(t *testing.T) {
	input := visualDamageJSON(t, `{"category":"impact","theme":"default","elements":["default"]}`)
	input = strings.Replace(input, `"activation": {`, `"presentation":{"cast":{"category":"cast","theme":"default","elements":["default"]}},"activation": {`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))
	environment := DefaultCompileEnvironment()
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{PresentationLimit: 8})

	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	events := runtime.PresentationEvents(0)
	if len(events) != 2 || events[0].Kind != PresentationCast || events[1].Kind != PresentationEffect {
		t.Fatalf("presentation events = %#v", events)
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 || events[1].WorldRevision == 0 {
		t.Fatalf("presentation ordering/revision = %#v", events)
	}
	if events[1].CastID != castID || events[1].Source != 1 || events[1].PrimaryTarget != 2 || !events[1].HasEffect {
		t.Fatalf("presentation routing = %#v", events[1])
	}
	if events[1].Anchor.Source != 1 || events[1].Anchor.Target != 2 {
		t.Fatalf("presentation anchor = %#v", events[1].Anchor)
	}
	if tail := runtime.PresentationEvents(1); len(tail) != 1 || tail[0].Sequence != 2 {
		t.Fatalf("presentation tail = %#v", tail)
	}
}

func TestPresentationPollingReportsRetentionLoss(t *testing.T) {
	input := visualDamageJSON(t, `{"category":"impact","theme":"default","elements":["default"]}`)
	input = strings.Replace(input, `"activation": {`, `"presentation":{"cast":{"category":"cast","theme":"default","elements":["default"]}},"activation": {`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{PresentationLimit: 1})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	batch := runtime.PollPresentation(0, 1)
	if !batch.CursorExpired || batch.OldestSequence != 2 || batch.LatestSequence != 2 {
		t.Fatalf("expired batch = %#v", batch)
	}
}

func TestSpawnVisualCompilesAndEmitsLifecycle(t *testing.T) {
	input := string(mustReadFixture(t, "projectile_area.json"))
	input = strings.Replace(input, `"spawn":{"kind":"area"`, `"spawn":{"kind":"area","visual":{"category":"area","theme":"default","elements":["default"]}`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))
	plan := InspectPresentationPlan(program)
	if len(plan.Spawns) != 1 || !plan.Spawns[0].HasSpawn {
		t.Fatalf("spawn mounts = %#v", plan.Spawns)
	}
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	events := runtime.PresentationEvents(0)
	seenStart, seenStop := false, false
	for _, event := range events {
		if event.Kind == PresentationSpawnStart {
			seenStart = true
		}
		if event.Kind == PresentationSpawnStop {
			seenStop = true
		}
	}
	if !seenStart || !seenStop {
		t.Fatalf("spawn lifecycle events = %#v", events)
	}

	wrong := strings.Replace(input, `"category":"area"`, `"category":"impact"`, 1)
	_, diagnostics := Compile(mustParseJSON(t, wrong), DefaultCompileEnvironment())
	requireDiagnostic(t, diagnostics, DiagnosticVisualInvalid)
}

func TestRuntimeDoesNotEmitEffectPresentationForExpectedFailure(t *testing.T) {
	program := mustCompileProgram(t, mustParseJSON(t, visualDamageJSON(t, `{"category":"impact","theme":"default","elements":["default"]}`)))
	host := runtimeTestHost(DefaultCompileEnvironment())
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 999}); err != nil {
		t.Fatal(err)
	}
	if events := runtime.PresentationEvents(0); len(events) != 0 {
		t.Fatalf("failed effect emitted presentation: %#v", events)
	}
}

type testVisualResolver struct {
	revision string
	digest   string
	fail     bool
}

func (resolver testVisualResolver) CatalogIdentity() (string, string) {
	return resolver.revision, resolver.digest
}

func (resolver testVisualResolver) Resolve(visual VisualView) (VisualAsset, error) {
	if resolver.fail {
		return VisualAsset{}, errors.New("missing")
	}
	return VisualAsset{Key: visual.Category + "/" + visual.Theme, Preload: append([]string(nil), visual.Elements...)}, nil
}

func TestResolvePresentationPlanValidatesCatalogAndDetachesAssets(t *testing.T) {
	plan := PresentationPlan{
		Manifest: SkillVisualManifest{CatalogRevision: "r1", CatalogDigest: "d1", Entries: []VisualView{{Index: 0, Category: "impact", Theme: "default", Elements: []string{"fire"}}}},
		Effects:  []PresentationMount{{VisualIndex: 0, HasEffect: true}},
	}
	resolved, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r1", digest: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Assets) != 1 || resolved.Assets[0].Asset.Key != "impact/default" {
		t.Fatalf("resolved = %#v", resolved)
	}
	resolved.Assets[0].Asset.Preload[0] = "mutated"
	if plan.Manifest.Entries[0].Elements[0] != "fire" {
		t.Fatal("resolved assets alias the input plan")
	}
	if _, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r2", digest: "d1"}); !errors.Is(err, ErrVisualCatalogMismatch) {
		t.Fatalf("catalog mismatch = %v", err)
	}
	plan.Effects[0].VisualIndex = 9
	if _, err := ResolvePresentationPlan(plan, testVisualResolver{revision: "r1", digest: "d1"}); !errors.Is(err, ErrVisualMountInvalid) {
		t.Fatalf("invalid mount = %v", err)
	}
}

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
