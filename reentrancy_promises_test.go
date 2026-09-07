package actionflow

import (
	"errors"
	"math"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

// reentrantAction calls back into the runner from inside one of its own
// callbacks, once. Whatever it starts replaces itself, which the runner has to
// detect rather than keep driving a stale entry.
type reentrantAction struct {
	runnerTestAction
	runner    *ActionRunner
	from      string // "start", "tick" or "cancel"
	next      *runnerTestAction
	reentered bool
	innerErr  error
}

func (a *reentrantAction) reenter(where string) {
	if a.from != where || a.reentered {
		return
	}
	a.reentered = true
	_, a.innerErr = a.runner.Start(1, a.next, 0, time.Now())
}

func (a *reentrantAction) Start(ctx *coreflow.ActionContext) error {
	a.reenter("start")
	return a.runnerTestAction.Start(ctx)
}

func (a *reentrantAction) Tick(ctx *coreflow.ActionContext) (bool, coreflow.ActionResult) {
	a.reenter("tick")
	return a.runnerTestAction.Tick(ctx)
}

func (a *reentrantAction) Cancel(ctx *coreflow.ActionContext, reason string) {
	a.reenter("cancel")
	a.runnerTestAction.Cancel(ctx, reason)
}

// newReentrancyRunner accepts any coreflow.Action as the build parameter so a
// reentrant action and a plain one can share a kind.
func newReentrancyRunner(t *testing.T, hooks ...ActionRunnerHooks) *ActionRunner {
	t.Helper()
	var hook ActionRunnerHooks
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (coreflow.Action, error) {
		action, ok := param.(coreflow.Action)
		if !ok {
			return nil, errors.New("invalid action")
		}
		return action, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) { return 1, true },
		Hooks:        hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// U-0100 (C2): reentrancy from Start, Tick and Cancel each surface as
// ErrReentrantMutation on the outer call, while the inner Start's replacement
// stands as the current action — never both, never a stale one.
func TestActionRunnerReportsReentrantMutationFromEachCallback(t *testing.T) {
	for _, from := range []string{"start", "tick", "cancel", "transition"} {
		t.Run(from, func(t *testing.T) {
			next := &runnerTestAction{label: "next"}
			var runner *ActionRunner
			var outer *reentrantAction
			runner = newReentrancyRunner(t, ActionRunnerHooks{OnTransition: func(snapshot ActionSnapshot, entering bool) {
				// The transition hook fires before the action's own Start; a hook
				// that starts something else from there is reentrancy too.
				if from == "transition" && entering && snapshot.Action == outer && !outer.reentered {
					outer.reentered = true
					_, outer.innerErr = runner.Start(1, next, 0, time.Now())
				}
			}})
			outer = &reentrantAction{runnerTestAction: runnerTestAction{label: "outer"}, runner: runner, from: from, next: next}
			var err error
			switch from {
			case "start", "transition":
				_, err = runner.Start(1, outer, 0, time.Now())
			case "tick":
				if _, err = runner.Start(1, outer, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
				err = runner.Tick(1, time.Now())
			case "cancel":
				if _, err = runner.Start(1, outer, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
				_, err = runner.Start(1, &runnerTestAction{label: "replacement"}, 0, time.Now())
			}
			if !errors.Is(err, ErrReentrantMutation) {
				t.Fatalf("outer call from %s = %v, want ErrReentrantMutation", from, err)
			}
			if !outer.reentered || outer.innerErr != nil {
				t.Fatalf("inner Start reentered=%v err=%v", outer.reentered, outer.innerErr)
			}
			if got := runner.Current(1); got != next {
				t.Fatalf("current after reentrancy = %v, want the action the inner Start installed", got)
			}
			if next.starts != 1 {
				t.Fatalf("inner action started %d times", next.starts)
			}
			if from == "transition" && outer.starts != 0 {
				t.Fatalf("an action replaced during its transition hook still had Start called %d time(s)", outer.starts)
			}
		})
	}
}

func TestActionRunnerRefusesInvalidConfigUnknownGroupsAndExhaustedIDs(t *testing.T) {
	registry := NewRegistry()
	if _, err := NewActionRunner(ActionRunnerConfig{GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) { return 1, true }}); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("runner without registry = %v", err)
	}
	if _, err := NewActionRunner(ActionRunnerConfig{Registry: registry}); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("runner without group resolver = %v", err)
	}
	var ended []coreflow.ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	runner.groupForKind = func(kind coreflow.ActionKind) (coreflow.ActionGroup, bool) { return 1, kind == 1 }
	if _, err := runner.Start(2, &runnerTestAction{}, 0, time.Now()); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("Start with an unknown group = %v", err)
	}
	if _, err := runner.Enqueue(2, &runnerTestAction{}, 0); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("Enqueue with an unknown group = %v", err)
	}
	runner.nextID = math.MaxInt64
	action := &runnerTestAction{}
	if _, err := runner.Start(1, action, 0, time.Now()); !errors.Is(err, ErrActionIDExhausted) {
		t.Fatalf("Start with exhausted ids = %v", err)
	}
	if action.starts != 0 || runner.Current(1) != nil {
		t.Fatal("a refused start left state behind")
	}
}

type reentrantMission struct {
	runnerTestMission
	runner   *MissionRunner
	innerErr error
}

func (m *reentrantMission) Start(ctx *coreflow.MissionContext, param any) error {
	m.innerErr = m.runner.StartMission(2, nil)
	return m.runnerTestMission.Start(ctx, param)
}

func TestMissionRunnerRefusesReentrantStartExhaustedIDsAndIdleCancel(t *testing.T) {
	if _, err := NewMissionRunner(MissionRunnerConfig{}); !errors.Is(err, ErrMissionBuilderNotFound) {
		t.Fatalf("mission runner without registry = %v", err)
	}
	registry := NewRegistry()
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	mission := &reentrantMission{runnerTestMission: runnerTestMission{kind: 1}, runner: runner}
	if err := registry.RegisterMission(1, func() coreflow.Mission { return mission }); err != nil {
		t.Fatal(err)
	}
	built := 0
	if err := registry.RegisterMission(2, func() coreflow.Mission { built++; return &runnerTestMission{kind: 2} }); err != nil {
		t.Fatal(err)
	}
	if err := runner.CancelMission("nothing"); !errors.Is(err, ErrMissionNotRunning) {
		t.Fatalf("CancelMission with nothing running = %v", err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatalf("outer StartMission = %v", err)
	}
	if !errors.Is(mission.innerErr, ErrReentrantMutation) || built != 0 {
		t.Fatalf("inner StartMission = %v built=%d; want ErrReentrantMutation and no build", mission.innerErr, built)
	}
	if runner.CurMission() != mission {
		t.Fatalf("current mission = %v, want the outer one", runner.CurMission())
	}
	runner.nextID = math.MaxInt64
	if err := runner.StartMission(2, nil); !errors.Is(err, ErrMissionIDExhausted) || built != 0 {
		t.Fatalf("StartMission with exhausted ids = %v built=%d", err, built)
	}
	plan := coreflow.MissionPlan{Steps: []coreflow.MissionStep{{Action: 1}}}
	if _, err := PlanFrom(plan); err != nil {
		t.Fatalf("fixture plan must be valid so the context guard is the only refuser: %v", err)
	}
	if err := NewPlanMission(3).Start(nil, plan); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanMission.Start without a context = %v", err)
	}
	if err := NewPlanMission(3).Start(&coreflow.MissionContext{}, plan); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanMission.Start without an action list = %v", err)
	}
}
