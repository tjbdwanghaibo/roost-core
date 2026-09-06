package actionflow

import (
	"errors"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

type stickyMission struct{ runnerTestMission }

func (*stickyMission) CanReplaceBy(coreflow.MissionKind, any) bool { return false }

// A frozen action group is one the owner has deliberately parked (a scene
// transition, a disconnect); nothing may start in it until Recover.
func TestActionRunnerRefusesToStartInAFrozenGroup(t *testing.T) {
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	runner.Freeze(1)
	if !runner.Frozen(1) {
		t.Fatal("group not frozen")
	}
	action := &runnerTestAction{label: "parked"}
	if _, err := runner.Start(1, action, 0, time.Now()); !errors.Is(err, ErrActionGroupFrozen) {
		t.Fatalf("Start in a frozen group = %v", err)
	}
	if action.starts != 0 || runner.Current(1) != nil {
		t.Fatalf("a refused start left state behind: starts=%d current=%v", action.starts, runner.Current(1))
	}
	if err := runner.Recover(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, action, 0, time.Now()); err != nil || action.starts != 1 {
		t.Fatalf("Start after Recover = %v starts=%d", err, action.starts)
	}
}

// Missions are exclusive per runner: a kind of 0 with no default is a
// programming error, and a running mission that declines replacement keeps
// the runner (the replacement is not built, the current is not ended).
func TestMissionRunnerRefusesUnknownKindAndUnreplaceableMission(t *testing.T) {
	registry := NewRegistry()
	sticky := &stickyMission{runnerTestMission{kind: 1}}
	built := 0
	if err := registry.RegisterMission(1, func() coreflow.Mission { return sticky }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(2, func() coreflow.Mission { built++; return &runnerTestMission{kind: 2} }); err != nil {
		t.Fatal(err)
	}
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(0, nil); !errors.Is(err, ErrKindInvalid) {
		t.Fatalf("StartMission(0) without a default kind = %v", err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(2, nil); !errors.Is(err, ErrMissionRunning) {
		t.Fatalf("StartMission over an unreplaceable mission = %v", err)
	}
	if built != 0 || sticky.ends != 0 || runner.CurMission() != sticky {
		t.Fatalf("refused replacement touched state: built=%d ends=%d current=%v", built, sticky.ends, runner.CurMission())
	}
}
