package skill

import (
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

func TestProcessVisualCompilesAndEmitsLifecycle(t *testing.T) {
	input := string(mustReadFixture(t, "projectile_area.json"))
	input = strings.Replace(input, `"process":{"kind":"area"`, `"process":{"kind":"area","visual":{"category":"area","theme":"default","elements":["default"]}`, 1)
	program := mustCompileProgram(t, mustParseJSON(t, input))
	plan := InspectPresentationPlan(program)
	if len(plan.Processes) != 1 || !plan.Processes[0].HasProcess {
		t.Fatalf("process mounts = %#v", plan.Processes)
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
		if event.Kind == PresentationProcessStart {
			seenStart = true
		}
		if event.Kind == PresentationProcessStop {
			seenStop = true
		}
	}
	if !seenStart || !seenStop {
		t.Fatalf("process lifecycle events = %#v", events)
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
