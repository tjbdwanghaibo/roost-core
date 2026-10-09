package skill

import (
	"errors"
	"testing"
)

type carryRejectOnceHost struct {
	*ownedCarryLifecycleHost
	attached              bool
	detachAttempts, stops int
	reject                error
}

func (host *carryRejectOnceHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if carry, ok := command.Motion.(CarryMotionStep); ok && !carry.Attached {
		host.detachAttempts++
		if host.detachAttempts == 1 {
			return SpawnStepResult{}, host.reject
		}
		result, err := host.ownedCarryLifecycleHost.StepSpawn(command, state)
		if err == nil {
			host.attached = false
		}
		return result, err
	}
	return host.ownedCarryLifecycleHost.StepSpawn(command, state)
}
func (host *carryRejectOnceHost) StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error) {
	host.stops++
	return host.MemoryHost.StopSpawn(command, state)
}

func TestFailedCarryDetachRetriesBeforeSpawnRetirement(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "live"
		if restore {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			runtime, base, spawn := startOwnedCarryLifecycle(t, `{"type":"end"}`, "")
			callbacks := 0
			base.observeApply = func() { callbacks++ }
			program := spawn.Program
			host := &carryRejectOnceHost{ownedCarryLifecycleHost: base, attached: true, reject: errors.New("detach refused")}
			runtime.host = host
			if err := runtime.Shutdown(); !errors.Is(err, host.reject) {
				t.Fatalf("shutdown error=%v", err)
			}
			if spawn.Status != SpawnStopPending || !spawn.Motion.CarryAttached || host.stops != 0 {
				t.Errorf("failed detach retired spawn: status=%s attached=%v stops=%d", spawn.Status, spawn.Motion.CarryAttached, host.stops)
			}
			if restore {
				checkpoint, err := runtime.Checkpoint()
				if err != nil {
					t.Fatal(err)
				}
				runtime, err = RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := runtime.Advance(4); err != nil {
				t.Fatal(err)
			}
			if host.attached || host.detachAttempts != 2 || host.stops != 1 {
				t.Fatalf("carry left attached: attached=%v attempts=%d stops=%d", host.attached, host.detachAttempts, host.stops)
			}
			if callbacks != 1 {
				t.Fatalf("cancel callbacks=%d", callbacks)
			}
		})
	}
}
