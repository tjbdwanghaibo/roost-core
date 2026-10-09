package actionflow

import (
	"errors"
	"testing"
	"time"
)

type runnerTestMission struct {
	id       int64
	kind     MissionKind
	status   MissionStatus
	startErr error
	ends     int
}

func (m *runnerTestMission) SetRuntime(id int64, _ MissionManager) { m.id = id }
func (m *runnerTestMission) ID() int64                             { return m.id }
func (m *runnerTestMission) Kind() MissionKind                     { return m.kind }
func (m *runnerTestMission) Status() MissionStatus                 { return m.status }
func (m *runnerTestMission) Start(*MissionContext, any) error {
	if m.startErr == nil {
		m.status = MissionStatusRunning
	}
	return m.startErr
}
func (*runnerTestMission) Tick(*MissionContext, time.Time) {}
func (*runnerTestMission) OnActionEnd(*MissionContext, int64, ActionKind, ActionReason) {
}
func (m *runnerTestMission) End(_ *MissionContext, reason ActionReason) {
	m.ends++
	result := reason.ToActionResult(ActionStatusCanceled)
	if result.Status == ActionStatusFailed {
		m.status = MissionStatusFailed
	} else {
		m.status = MissionStatusCanceled
	}
}
func (*runnerTestMission) CanReplaceBy(MissionKind, any) bool { return true }
func (m *runnerTestMission) MissionInfo() MissionInfo {
	return MissionInfo{Status: m.status, CurrentStep: -1}
}

func TestMissionRunnerBuildFailurePreservesCurrent(t *testing.T) {
	registry := NewRegistry()
	current := &runnerTestMission{kind: 1}
	if err := registry.RegisterMission(1, func() Mission { return current }); err != nil {
		t.Fatal(err)
	}
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(2, nil); !errors.Is(err, ErrMissionBuilderNotFound) {
		t.Fatalf("missing builder error = %v", err)
	}
	if runner.CurMission() != current || current.ends != 0 {
		t.Fatalf("build failure replaced current mission: current=%v ends=%d", runner.CurMission(), current.ends)
	}
}

func TestMissionRunnerStartFailureCleansState(t *testing.T) {
	registry := NewRegistry()
	failed := &runnerTestMission{kind: 1, startErr: errors.New("start")}
	if err := registry.RegisterMission(1, func() Mission { return failed }); err != nil {
		t.Fatal(err)
	}
	states := make([]bool, 0, 2)
	cleared := int64(0)
	runner, err := NewMissionRunner(MissionRunnerConfig{
		Registry: registry,
		Hooks: MissionRunnerHooks{
			OnState: func(active bool) { states = append(states, active) },
			ClearActions: func(id int64, _ ActionReason) error {
				cleared = id
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); !errors.Is(err, failed.startErr) {
		t.Fatalf("start error = %v", err)
	}
	if runner.CurMission() != nil || len(states) != 2 || !states[0] || states[1] || cleared != failed.id || failed.ends != 1 {
		t.Fatalf("start rollback incomplete: current=%v states=%v cleared=%d id=%d ends=%d", runner.CurMission(), states, cleared, failed.id, failed.ends)
	}
}
