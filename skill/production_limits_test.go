package skill

import (
	"errors"
	"strings"
	"testing"
)

func TestParseLimitsRejectWorkBeforeSemanticDecode(t *testing.T) {
	if _, err := ParseWithLimits([]byte(`{"schema":"roost.skill/v2"}`), ParseLimits{MaxBytes: 4}); !errors.Is(err, ErrParseLimitExceeded) {
		t.Fatalf("byte limit error = %v", err)
	}
	nested := strings.Repeat("[", 5) + "0" + strings.Repeat("]", 5)
	if _, err := ParseWithLimits([]byte(nested), ParseLimits{MaxBytes: 1024, MaxDepth: 3}); !errors.Is(err, ErrParseLimitExceeded) {
		t.Fatalf("depth limit error = %v", err)
	}
	if _, err := ParseGeneratedWithLimits([]byte(`{"error":{"code":"UNSUPPORTED_CAPABILITY","message":"toolong"},"schema":"roost.skill/v2"}`), ParseLimits{MaxBytes: 1024, MaxStringBytes: 4}); !errors.Is(err, ErrParseLimitExceeded) {
		t.Fatalf("string limit error = %v", err)
	}
}

func TestRuntimeRetentionIsBounded(t *testing.T) {
	runtime := NewRuntime(nil, RuntimeOptions{RuntimeEventLimit: 2, CastEventLimit: 2, CompletedCastLimit: 1, RootEventLimit: 2})
	runtime.appendRuntimeEvent(RuntimeEvent{Kind: "1"})
	runtime.appendRuntimeEvent(RuntimeEvent{Kind: "2"})
	runtime.appendRuntimeEvent(RuntimeEvent{Kind: "3"})
	if len(runtime.runtimeEvents) != 2 || runtime.runtimeEventDropped != 1 {
		t.Fatalf("events=%d dropped=%d", len(runtime.runtimeEvents), runtime.runtimeEventDropped)
	}
	castEvents := &castInstance{}
	runtime.appendCastEvent(castEvents, RuntimeEvent{Kind: "1"})
	runtime.appendCastEvent(castEvents, RuntimeEvent{Kind: "2"})
	runtime.appendCastEvent(castEvents, RuntimeEvent{Kind: "3"})
	if len(castEvents.events) != 2 || castEvents.eventsDropped != 1 {
		t.Fatalf("cast events=%d dropped=%d", len(castEvents.events), castEvents.eventsDropped)
	}
	runtime.casts[1] = &castInstance{id: 1, status: CastFinished, committed: true, abilityFinished: true}
	runtime.casts[2] = &castInstance{id: 2, status: CastFinished, committed: true, abilityFinished: true}
	runtime.activeCastCount = 2
	runtime.trackCompletedCastLocked(runtime.casts[1])
	runtime.trackCompletedCastLocked(runtime.casts[2])
	if len(runtime.casts) != 1 || runtime.casts[2] == nil {
		t.Fatalf("casts=%v", runtime.casts)
	}
}

func TestClockMutationCarriesDerivedTimeWithoutEntityFanout(t *testing.T) {
	before := RuntimeStateSnapshot{Tick: 1, Casts: []CastStateSnapshot{{ID: 1, StartTick: 0, ElapsedTicks: 1}}, Cooldowns: []CooldownStateSnapshot{{Caster: 1, ProgramID: "s", DueTick: 10, Remaining: 9}}, Abilities: []AbilityStateSnapshot{{Owner: 1, Handle: 1, ProgramID: "s", CooldownRemaining: 9}}}
	after := before
	after.Tick = 2
	after.LatestStateMutationSequence = 1
	after.Casts = append([]CastStateSnapshot(nil), before.Casts...)
	after.Casts[0].ElapsedTicks = 2
	after.Cooldowns = append([]CooldownStateSnapshot(nil), before.Cooldowns...)
	after.Cooldowns[0].Remaining = 8
	after.Abilities = append([]AbilityStateSnapshot(nil), before.Abilities...)
	after.Abilities[0].CooldownRemaining = 8
	mutations := diffRuntimeState(before, after)
	if len(mutations) != 1 || mutations[0].Kind != StateMutationClock {
		t.Fatalf("mutations=%#v", mutations)
	}
	mutations[0].Sequence, mutations[0].Tick = 1, after.Tick
	if err := ApplyStateMutation(&before, mutations[0]); err != nil {
		t.Fatal(err)
	}
	if !runtimeSnapshotsEqual(before, after) {
		t.Fatalf("folded=%#v want=%#v", before, after)
	}
}
