package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestRuntimeCheckpointRestoresActiveTimelineDeterministically(t *testing.T) {
	program, environment := compileCastWindowSkill(t, true)
	seed := fixedTestSeed(88)

	baselineHost := runtimeTestHost(environment)
	baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: seed})
	baselineID, err := baseline.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Advance(2); err != nil {
		t.Fatal(err)
	}

	recoveredHost := runtimeTestHost(environment)
	source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: seed})
	recoveredID, err := source.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Advance(2); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) {
		if id == program.id && digest == program.identity.gameplayDigest {
			return program, nil
		}
		return nil, ErrCheckpointProgram
	})
	recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredID != baselineID {
		t.Fatalf("cast id=%d, want %d", recoveredID, baselineID)
	}
	for _, tick := range []Tick{3, 5, 9} {
		if err := baseline.Advance(tick); err != nil {
			t.Fatal(err)
		}
		if err := recovered.Advance(tick); err != nil {
			t.Fatal(err)
		}
		left, _ := baseline.InspectCast(baselineID)
		right, _ := recovered.InspectCast(recoveredID)
		if left.Status != right.Status || left.WindowStage != right.WindowStage || left.PulseIndex != right.PulseIndex {
			t.Fatalf("tick %d baseline=%#v recovered=%#v", tick, left, right)
		}
		if baselineHost.HealthForTest(2) != recoveredHost.HealthForTest(2) || baselineHost.ResourceForTest(1, "mana") != recoveredHost.ResourceForTest(1, "mana") {
			t.Fatalf("tick %d host state diverged", tick)
		}
	}
}

func TestRuntimeCheckpointRestoresActiveProcesses(t *testing.T) {
	tests := []struct {
		fixture string
		input   CastInput
		ticks   []Tick
	}{
		{fixture: "area_heal.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3}},
		{fixture: "dynamic_numeric.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3}},
		{fixture: "tracking_boomerang.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3, 10}},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			program, environment := compileRuntimeFixture(t, test.fixture)
			baselineHost := runtimeTestHost(environment)
			baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: fixedTestSeed(91)})
			if _, err := baseline.Activate(program, test.input); err != nil {
				t.Fatal(err)
			}
			recoveredHost := runtimeTestHost(environment)
			source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: fixedTestSeed(91)})
			if _, err := source.Activate(program, test.input); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := source.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(id, digest string) (*Program, error) {
				if id == program.id && digest == program.identity.gameplayDigest {
					return program, nil
				}
				return nil, ErrCheckpointProgram
			}))
			if err != nil {
				t.Fatal(err)
			}
			for _, tick := range test.ticks {
				if err := baseline.Advance(tick); err != nil {
					t.Fatal(err)
				}
				if err := recovered.Advance(tick); err != nil {
					t.Fatal(err)
				}
				want, _ := json.Marshal(baseline.StateSnapshot())
				got, _ := json.Marshal(recovered.StateSnapshot())
				if !bytes.Equal(got, want) || recoveredHost.CurrentRevision() != baselineHost.CurrentRevision() {
					t.Fatalf("tick %d diverged\nwant=%s\ngot=%s", tick, want, got)
				}
			}
		})
	}
}

func TestRuntimeCheckpointFailsClosed(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })

	corrupt := checkpoint
	corrupt.Payload = append([]byte(nil), checkpoint.Payload...)
	corrupt.Payload[len(corrupt.Payload)-1] ^= 1
	if _, err := RestoreRuntime(host, RuntimeOptions{}, corrupt, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("corrupt restore=%v", err)
	}
	wrongHost := runtimeTestHost(environment)
	wrongHost.revision++
	if _, err := RestoreRuntime(wrongHost, RuntimeOptions{}, checkpoint, resolver); !errors.Is(err, ErrCheckpointHostMismatch) {
		t.Fatalf("host mismatch=%v", err)
	}
	unsupported := checkpoint
	unsupported.Version++
	if _, err := RestoreRuntime(host, RuntimeOptions{}, unsupported, resolver); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Fatalf("version mismatch=%v", err)
	}
}

func TestRuntimeRestoreRequiresFullRecoveryForOmittedDeliveryBuffers(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "visual_projectile.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if batch := restored.StateDeltas(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Mutations) != 0 {
		t.Fatalf("state recovery=%#v", batch)
	}
	if batch := restored.StateEvents(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Events) != 0 {
		t.Fatalf("event recovery=%#v", batch)
	}
	if batch := restored.PollPresentation(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Events) != 0 {
		t.Fatalf("presentation recovery=%#v", batch)
	}
}

func TestRuntimeCheckpointRestoresPendingPassiveActivation(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "passive_counter.json")
	event := EventContext{EventID: 10, RootEventID: 10, Source: 2, Owner: 1, Target: 1, Result: "hit"}.WithGameplayTags([]GameplayTagHandle{2, 1})
	baselineHost := runtimeTestHost(environment)
	baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: fixedTestSeed(92)})
	if _, err := baseline.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	recoveredHost := runtimeTestHost(environment)
	source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: fixedTestSeed(92)})
	if _, err := source.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Advance(0); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Advance(0); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(baseline.StateSnapshot())
	got, _ := json.Marshal(recovered.StateSnapshot())
	if !bytes.Equal(got, want) || recoveredHost.CurrentRevision() != baselineHost.CurrentRevision() {
		t.Fatalf("passive continuation diverged\nwant=%s\ngot=%s", want, got)
	}
}
