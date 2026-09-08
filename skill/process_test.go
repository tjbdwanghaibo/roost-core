package skill

import (
	"reflect"
	"testing"
)

func TestProcessStopIsUnifiedAndIdempotent(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	process := &ProcessInstance{ID: 7, CastID: castID, Status: ProcessRunning, Scope: ProcessScopeCast, HostState: ProcessHostState{ProcessID: 7, Active: true}}
	runtime.processes[process.ID] = process
	if _, err := host.StepProcess(ProcessStepCommand{Meta: ProcessCommandMeta{ProcessID: 7}, Motion: StaticMotionStep{}}, process.HostState); err != nil {
		t.Fatal(err)
	}
	if err := runtime.stopProcess(runtime.casts[castID], process, StopCauseCancel); err != nil {
		t.Fatal(err)
	}
	revision := host.CurrentRevision()
	if err := runtime.stopProcess(runtime.casts[castID], process, StopCauseCancel); err != nil || host.CurrentRevision() != revision {
		t.Fatalf("second stop changed world: %v", err)
	}
	if process.Status != ProcessCancelled {
		t.Fatalf("process = %#v", process)
	}
}

func TestProcessSignalsUseCanonicalOrder(t *testing.T) {
	signals := []ProcessSignal{
		{Kind: ProcessSignalTick, Target: 5},
		{Kind: ProcessSignalEnter, Target: 4},
		{Kind: ProcessSignalCollision, Target: 3, Distance: 20, ContactOrdinal: 1},
		{Kind: ProcessSignalEnd},
		{Kind: ProcessSignalHit, Target: 2, Distance: 10, ContactOrdinal: 2},
		{Kind: ProcessSignalTargetLost},
		{Kind: ProcessSignalLeave, Target: 7},
		{Kind: ProcessSignalTransition},
	}
	ordered := normalizeProcessSignals(signals)
	got := make([]ProcessSignalKind, len(ordered))
	for index := range ordered {
		got[index] = ordered[index].Kind
	}
	want := []ProcessSignalKind{
		ProcessSignalHit, ProcessSignalCollision, ProcessSignalTargetLost, ProcessSignalTransition,
		ProcessSignalLeave, ProcessSignalEnter, ProcessSignalTick, ProcessSignalEnd,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
