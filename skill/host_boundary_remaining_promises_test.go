package skill

import (
	"errors"
	"fmt"
	"testing"
)

func TestNilHostAdvanceAndPassiveReturnError(t *testing.T) {
	program := *summonCostingProgram(t)
	program.activationKind = "passive"
	for _, name := range []string{"advance", "passive"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("nil host caused panic: %v", value)
				}
			}()
			runtime := NewRuntime(nil, RuntimeOptions{})
			var err error
			if name == "advance" {
				err = runtime.Advance(1)
			} else {
				_, err = runtime.ActivatePassive(&program, EventContext{EventID: 1, RootEventID: 1, Source: 1})
			}
			if !errors.Is(err, ErrProgramInvariant) {
				t.Fatalf("nil host error=%v", err)
			}
		})
	}
}

func TestCheckpointCapabilityFailureKeepsSentinel(t *testing.T) {
	flow := `{"flow":"wait","ticks":5,"then":{"flow":"finish"}}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("restore-cost", `[{"resource":"mana","amount":1}]`, flow)), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	_, err = RestoreRuntime(&emptyTableHost{MemoryHost: host}, RuntimeOptions{}, checkpoint, resolver)
	if !errors.Is(err, ErrCheckpointProgram) || !errors.Is(err, ErrHostCapabilityMissing) {
		t.Fatalf("checkpoint lost capability sentinel: %v", err)
	}
}

func TestHostAdmissionCacheRemainsBounded(t *testing.T) {
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{})
	for i := range 4096 {
		program := &Program{id: fmt.Sprintf("generated-%d", i), hostRequirements: []HostCapability{{Kind: HostCapabilitySummon}}}
		if err := runtime.admitHostCapabilitiesLocked(program); err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.hostAdmitted) > 1024 {
		t.Fatalf("host admission retained %d one-shot programs", len(runtime.hostAdmitted))
	}
}
