package actionflow

import (
	"errors"
	"math"
	"testing"
	"time"
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

func (a *reentrantAction) Start(ctx *ActionContext) error {
	a.reenter("start")
	return a.runnerTestAction.Start(ctx)
}

func (a *reentrantAction) Tick(ctx *ActionContext) (bool, ActionResult) {
	a.reenter("tick")
	return a.runnerTestAction.Tick(ctx)
}

func (a *reentrantAction) Cancel(ctx *ActionContext, reason string) {
	a.reenter("cancel")
	a.runnerTestAction.Cancel(ctx, reason)
}

// newReentrancyRunner accepts any Action as the build parameter so a
// reentrant action and a plain one can share a kind.
func newReentrancyRunner(t *testing.T, hooks ...ActionRunnerHooks) *ActionRunner {
	t.Helper()
	var hook ActionRunnerHooks
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (Action, error) {
		action, ok := param.(Action)
		if !ok {
			return nil, errors.New("invalid action")
		}
		return action, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Hooks:        hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// U-0100 (C2) 原承诺：Start / Tick / Cancel / transition 回调里重入 Start，内层装上的动作
// 是唯一的当前动作、只启动一次——不会两者并存，也不会留下过期的当前动作。
//
// B7（延后语义）改写：回调里的 Start 进延后队列，外层调用做完自己那一步后才执行，所以：
//   - 外层调用不再返回 ErrReentrantMutation（没有冲突可报），返回 nil；
//   - transition 情形下外层动作先完整启动（starts==1）再被延后的 Start 替换——之前它在
//     切换钩子里就被换下、从未 Start；
//   - 原承诺照样成立并加强：内层动作是当前动作、只启动一次；外层动作恰好 Cancel 一次、
//     OnEnded 一次（之前 cancel 情形会被嵌套 finish 再取消一次，O-A3）。
func TestActionRunnerCallbackStartReplacesTheOuterActionExactlyOnce(t *testing.T) {
	for _, from := range []string{"start", "tick", "cancel", "transition"} {
		t.Run(from, func(t *testing.T) {
			next := &runnerTestAction{label: "next"}
			var runner *ActionRunner
			var outer *reentrantAction
			ended := map[Action]int{}
			runner = newReentrancyRunner(t, ActionRunnerHooks{
				OnTransition: func(snapshot ActionSnapshot, entering bool) {
					if from == "transition" && entering && snapshot.Action == outer && !outer.reentered {
						outer.reentered = true
						_, outer.innerErr = runner.Start(1, next, 0, time.Now())
					}
				},
				OnEnded: func(snapshot ActionSnapshot, _ ActionReason) { ended[snapshot.Action]++ },
			})
			outer = &reentrantAction{runnerTestAction: runnerTestAction{label: "outer"}, runner: runner, from: from, next: next}
			replacement := &runnerTestAction{label: "replacement"}
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
				_, err = runner.Start(1, replacement, 0, time.Now())
			}
			if err != nil {
				t.Fatalf("outer call from %s = %v, want nil (the callback Start is deferred)", from, err)
			}
			if !outer.reentered || outer.innerErr != nil {
				t.Fatalf("inner Start reentered=%v err=%v", outer.reentered, outer.innerErr)
			}
			if got := runner.Current(1); got != next {
				t.Fatalf("current after reentrancy = %v, want the action the inner Start installed", got)
			}
			if next.starts != 1 || next.cancels != 0 {
				t.Fatalf("inner action started %d / canceled %d time(s)", next.starts, next.cancels)
			}
			if outer.starts != 1 || outer.cancels != 1 || ended[outer] != 1 {
				t.Fatalf("outer action starts=%d cancels=%d ended=%d, want 1/1/1", outer.starts, outer.cancels, ended[outer])
			}
			if from == "cancel" && (replacement.starts != 1 || replacement.cancels != 1 || ended[replacement] != 1) {
				// 外层 Start(replacement) 先于 Cancel 回调里发起的 Start(next)，按发起顺序执行。
				t.Fatalf("replacement starts=%d cancels=%d ended=%d, want 1/1/1", replacement.starts, replacement.cancels, ended[replacement])
			}
		})
	}
}

func TestActionRunnerRefusesInvalidConfigUnknownGroupsAndExhaustedIDs(t *testing.T) {
	registry := NewRegistry()
	if _, err := NewActionRunner(ActionRunnerConfig{GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true }}); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("runner without registry = %v", err)
	}
	if _, err := NewActionRunner(ActionRunnerConfig{Registry: registry}); !errors.Is(err, ErrActionGroupInvalid) {
		t.Fatalf("runner without group resolver = %v", err)
	}
	var ended []ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	runner.groupForKind = func(kind ActionKind) (ActionGroup, bool) { return 1, kind == 1 }
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

// reentrantMission 在自己的 Start 里再调 StartMission。MissionRunner 改为延后队列（第五轮
// 决定）之后，这次调用返回 nil、回调返回前不构建也不替换，外层启动做完后再按序执行。
type reentrantMission struct {
	runnerTestMission
	runner       *MissionRunner
	innerErr     error
	innerCurrent Mission
	builtAtCall  int
	built        *int
}

func (m *reentrantMission) Start(ctx *MissionContext, param any) error {
	m.innerErr = m.runner.StartMission(2, nil)
	m.innerCurrent = m.runner.CurMission()
	m.builtAtCall = *m.built
	return m.runnerTestMission.Start(ctx, param)
}

func TestMissionRunnerDefersReentrantStartAndRefusesExhaustedIDsAndIdleCancel(t *testing.T) {
	if _, err := NewMissionRunner(MissionRunnerConfig{}); !errors.Is(err, ErrMissionBuilderNotFound) {
		t.Fatalf("mission runner without registry = %v", err)
	}
	registry := NewRegistry()
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	built := 0
	mission := &reentrantMission{runnerTestMission: runnerTestMission{kind: 1}, runner: runner, built: &built}
	if err := registry.RegisterMission(1, func() Mission { return mission }); err != nil {
		t.Fatal(err)
	}
	var second Mission
	if err := registry.RegisterMission(2, func() Mission { built++; second = &runnerTestMission{kind: 2}; return second }); err != nil {
		t.Fatal(err)
	}
	if err := runner.CancelMission("nothing"); !errors.Is(err, ErrMissionNotRunning) {
		t.Fatalf("CancelMission with nothing running = %v", err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatalf("outer StartMission = %v", err)
	}
	if mission.innerErr != nil || mission.builtAtCall != 0 || mission.innerCurrent != mission {
		t.Fatalf("inner StartMission = %v built=%d current=%v; want nil, nothing built and the outer mission still current inside its own Start",
			mission.innerErr, mission.builtAtCall, mission.innerCurrent)
	}
	if built != 1 || runner.CurMission() != second || mission.ends != 1 {
		t.Fatalf("after the outer start: built=%d current=%v outer ends=%d; want the deferred start to replace the outer mission once",
			built, runner.CurMission(), mission.ends)
	}
	runner.nextID = math.MaxInt64
	if err := runner.StartMission(2, nil); !errors.Is(err, ErrMissionIDExhausted) || built != 1 {
		t.Fatalf("StartMission with exhausted ids = %v built=%d", err, built)
	}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1}}}
	if _, err := PlanFrom(plan); err != nil {
		t.Fatalf("fixture plan must be valid so the context guard is the only refuser: %v", err)
	}
	if err := NewPlanMission(3).Start(nil, plan); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanMission.Start without a context = %v", err)
	}
	if err := NewPlanMission(3).Start(&MissionContext{}, plan); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanMission.Start without an action list = %v", err)
	}
}
