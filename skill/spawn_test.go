package skill

import (
	"reflect"
	"testing"
)

func TestSpawnStopIsUnifiedAndIdempotent(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	spawn := &SpawnInstance{ID: 7, CastID: castID, Status: SpawnRunning, Scope: SpawnScopeCast, HostState: SpawnHostState{SpawnID: 7, Active: true}}
	fileSpawnForTest(runtime, spawn)
	if _, err := host.StepSpawn(SpawnStepCommand{Meta: SpawnCommandMeta{SpawnID: 7}, Motion: StaticMotionStep{}}, spawn.HostState); err != nil {
		t.Fatal(err)
	}
	if err := runtime.stopSpawn(runtime.casts[castID], spawn, StopCauseCancel); err != nil {
		t.Fatal(err)
	}
	revision := host.CurrentRevision()
	if err := runtime.stopSpawn(runtime.casts[castID], spawn, StopCauseCancel); err != nil || host.CurrentRevision() != revision {
		t.Fatalf("second stop changed world: %v", err)
	}
	if spawn.Status != SpawnCancelled {
		t.Fatalf("spawn = %#v", spawn)
	}
}

func TestSpawnSignalsUseCanonicalOrder(t *testing.T) {
	signals := []SpawnSignal{
		{Kind: SpawnSignalTick, Target: 5},
		{Kind: SpawnSignalEnter, Target: 4},
		{Kind: SpawnSignalCollision, Target: 3, Distance: 20, ContactOrdinal: 1},
		{Kind: SpawnSignalEnd},
		{Kind: SpawnSignalHit, Target: 2, Distance: 10, ContactOrdinal: 2},
		{Kind: SpawnSignalTargetLost},
		{Kind: SpawnSignalLeave, Target: 7},
		{Kind: SpawnSignalTransition},
	}
	ordered := normalizeSpawnSignals(signals)
	got := make([]SpawnSignalKind, len(ordered))
	for index := range ordered {
		got[index] = ordered[index].Kind
	}
	want := []SpawnSignalKind{
		SpawnSignalHit, SpawnSignalCollision, SpawnSignalTargetLost, SpawnSignalTransition,
		SpawnSignalLeave, SpawnSignalEnter, SpawnSignalTick, SpawnSignalEnd,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
