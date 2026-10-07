package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

func TestAbilityOverlayBoundSurvivesRestoreAndReleasesOnExpiry(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "overlay-bound", `{"mode":"tap"}`, 0, `{"flow":"finish"}`)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MaxAbilityOverlays: 2})
	for _, owner := range []EntityID{1, 2} {
		if err := runtime.RegisterAbility(AbilityRegistration{Owner: owner, Handle: AbilityHandle(owner), Program: program}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(owner EntityID) error {
		handle := runtime.abilityByProgram[skillStateKey{Caster: owner, Skill: program.id}]
		_, err := runtime.ModifyAbilityState(owner, handle, "enabled", "set", BoolRuntimeValue(false), 2, EventContext{})
		return err
	}
	for _, owner := range []EntityID{1, 2} {
		if err := set(owner); err != nil {
			t.Fatal(err)
		}
	}
	assertFull := func() {
		t.Helper()
		id, events := runtime.nextAbilityOverlay, len(runtime.RuntimeEvents())
		if err := set(1); !errors.Is(err, ErrRuntimeCapacityExceeded) {
			t.Fatalf("third overlay at cap 2 = %v, want capacity rejection", err)
		}
		if runtime.nextAbilityOverlay != id || len(runtime.RuntimeEvents()) != events {
			t.Fatal("rejected overlay changed state")
		}
	}
	assertFull()
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	for _, bound := range []int{0, 1} {
		var payload runtimeCheckpointPayload
		if err := json.Unmarshal(checkpoint.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payload.MaxAbilityOverlays = bound
		tampered := checkpoint
		tampered.Payload, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(tampered.Payload)
		tampered.Checksum = hex.EncodeToString(digest[:])
		if _, err := RestoreRuntime(host, RuntimeOptions{}, tampered, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
			t.Fatalf("restore overlay bound %d: %v", bound, err)
		}
	}
	legacy := checkpoint
	legacy.Version = 9
	if _, err := RestoreRuntime(host, RuntimeOptions{}, legacy, resolver); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Fatalf("accepted old checkpoint: %v", err)
	}
	runtime, err = RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatal(err)
	}
	assertFull()
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	if err := set(1); err != nil {
		t.Fatalf("expired overlays did not release capacity: %v", err)
	}
}
