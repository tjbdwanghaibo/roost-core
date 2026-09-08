package skill

import (
	"strings"
	"testing"
)

func TestPassiveActivationQueuesThenAppliesFilterAndOncePerRootLedger(t *testing.T) {
	program, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `["spell"]`, `[]`))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	event := EventContext{EventID: 10, RootEventID: 10, Tick: 0, Owner: 1, Source: 2}.WithGameplayTags([]GameplayTagHandle{1})
	id, err := runtime.ActivatePassive(program, event)
	if err != nil || id == 0 || runtime.CastCount() != 0 {
		t.Fatalf("enqueue = %d %v casts=%d", id, err, runtime.CastCount())
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("activation = %v casts=%d", err, runtime.CastCount())
	}
	if _, err := runtime.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("once-per-root failed: %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "once_per_root")

	missingTag := EventContext{EventID: 11, RootEventID: 11, Owner: 1, Source: 2}
	if _, err := runtime.ActivatePassive(program, missingTag); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("filter failed: %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "filter")
}

func TestPassiveSelfTriggerAndDepthAreBounded(t *testing.T) {
	program, environment := compileRuntimeJSON(t, passiveSkillJSON(1, `[]`, `[]`))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	self := EventContext{EventID: 20, RootEventID: 20, Owner: 1, Source: 1, SkillID: program.id}
	if _, err := runtime.ActivatePassive(program, self); err != nil {
		t.Fatal(err)
	}
	depth := EventContext{EventID: 21, RootEventID: 21, Owner: 1, Source: 2, ProcDepth: 1}
	if _, err := runtime.ActivatePassive(program, depth); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 0 {
		t.Fatalf("bounded proc = %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "self_trigger")
	assertSuppressionReason(t, runtime.RuntimeEvents(), "max_depth")
}

func TestPassiveActivationPerTickLimitSuppressesWithoutCreatingCast(t *testing.T) {
	input := strings.Replace(passiveSkillJSON(2, `[]`, `[]`), `"once_per_root_event":true`, `"once_per_root_event":false`, 1)
	program, environment := compileRuntimeJSON(t, input)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MaxPassiveActivationsPerTick: 1})
	for id := EventID(30); id < 32; id++ {
		if _, err := runtime.ActivatePassive(program, EventContext{EventID: id, RootEventID: id, Owner: 1, Source: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("tick bound = %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "max_activations_per_tick")
}

func assertSuppressionReason(t *testing.T, events []RuntimeEvent, reason string) {
	t.Helper()
	for _, event := range events {
		if event.Kind == "passive_suppressed" && event.Context.Result == reason {
			return
		}
	}
	t.Fatalf("missing suppression %q in %#v", reason, events)
}
