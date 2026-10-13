package actionflow

import (
	"errors"
	"fmt"
	"math"
	"strings"
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

// MissionRunner 延后队列（维护者 2026-10-06 第五轮决定，与 ActionRunner / B7 一致）：回调——
// 任务的 Start / Tick / OnActionEnd / End，以及 OnState / OnChanged / OnEnded / ClearActions
// 钩子——里对 MissionRunner 的变更（StartMission、CancelMission、EndCurMission、Tick、
// OnActionEnd）不立即执行，进延后命令队列，最外层调用做完自己那一步后按发起顺序执行。判定
// 只在一处（MissionRunner.submit）。之前任务启动 / 结束途中的 StartMission 返回
// ErrReentrantMutation，Tick / OnActionEnd 里的 StartMission 则嵌套立即执行。
//
// 承诺：
//   - 回调里调用变更方法时 Deferring() 为真，StartMission 返回 nil，回调返回前被发起的任务
//     还没有 Start；最外层返回前命令全部执行完，被发起的任务恰好启动一次。
//   - 延后命令的错误（ErrMissionRunning、构建失败、Start 失败）经 OnError 报告
//     （`actionflow: deferred start mission: ...`），最外层调用自己的错误照旧返回。
//   - 延后队列有界（ErrDeferredQueueFull），回调互相触发由执行预算截停（ErrDeferredRunaway），
//     之后 runner 照常可用；回调 panic 被恢复，执行状态不会卡住。

// scriptedMission 在指定回调点调用 fire，用来从任务自己的回调里重入 MissionRunner。
type scriptedMission struct {
	runnerTestMission
	label    string
	starts   int
	startErr error
	fire     func(where string)
	events   *[]string
}

func (m *scriptedMission) note(event string) {
	if m.events != nil {
		*m.events = append(*m.events, event+":"+m.label)
	}
}

func (m *scriptedMission) call(where string) {
	if m.fire != nil {
		m.fire(where)
	}
}

func (m *scriptedMission) Start(ctx *MissionContext, param any) error {
	m.starts++
	m.note("start")
	m.call("mission start")
	if m.startErr != nil {
		return m.startErr
	}
	return m.runnerTestMission.Start(ctx, param)
}

func (m *scriptedMission) Tick(*MissionContext, time.Time) { m.call("mission tick") }

func (m *scriptedMission) OnActionEnd(*MissionContext, int64, ActionKind, ActionReason) {
	m.call("mission action end")
}

func (m *scriptedMission) End(ctx *MissionContext, reason ActionReason) {
	m.note("end")
	m.call("mission end")
	m.runnerTestMission.End(ctx, reason)
}

type missionCallbackView struct {
	fired      bool
	deferring  bool
	innerErr   error
	nextStarts int
}

func TestMissionCallbackMutationsAreDeferredUntilTheOuterCallReturns(t *testing.T) {
	points := []string{"state", "changed", "mission start", "mission tick", "mission action end", "mission end", "ended", "clear actions"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			var runner *MissionRunner
			var events []string
			armed := false
			view := missionCallbackView{}
			first := &scriptedMission{runnerTestMission: runnerTestMission{kind: 1}, label: "first", events: &events}
			next := &scriptedMission{runnerTestMission: runnerTestMission{kind: 2}, label: "next", events: &events}
			fire := func(where string) {
				if !armed || where != point || view.fired {
					return
				}
				view.fired = true
				view.innerErr = runner.StartMission(2, nil)
				view.deferring = runner.Deferring()
				view.nextStarts = next.starts
			}
			first.fire = fire
			registry := NewRegistry()
			if err := registry.RegisterMission(1, func() Mission { return first }); err != nil {
				t.Fatal(err)
			}
			if err := registry.RegisterMission(2, func() Mission { return next }); err != nil {
				t.Fatal(err)
			}
			var reported []error
			var err error
			runner, err = NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
				OnState:      func(bool) { fire("state") },
				OnChanged:    func(MissionInfo) { fire("changed") },
				OnEnded:      func(Mission, ActionReason) { fire("ended") },
				ClearActions: func(int64, ActionReason) error { fire("clear actions"); return nil },
				OnError:      func(err error) { reported = append(reported, err) },
			}})
			if err != nil {
				t.Fatal(err)
			}

			var outer error
			switch point {
			case "state", "changed", "mission start":
				armed = true
				outer = runner.StartMission(1, nil)
			default:
				if err := runner.StartMission(1, nil); err != nil {
					t.Fatal(err)
				}
				armed = true
				switch point {
				case "mission tick":
					runner.Tick(time.Now())
				case "mission action end":
					runner.OnActionEnd(7, 1, NewActionReason("done"))
				default:
					runner.EndCurMission(NewActionReason("outer end"))
				}
			}

			if !view.fired {
				t.Fatalf("callback %q never fired", point)
			}
			if view.innerErr != nil {
				t.Fatalf("StartMission from %s = %v, want nil: a callback mutation is deferred, not a conflict", point, view.innerErr)
			}
			if view.nextStarts != 0 {
				t.Fatalf("mission issued from %s started inside the callback (%d start(s)), want after the outer call", point, view.nextStarts)
			}
			if !view.deferring {
				t.Fatalf("Deferring() was false inside %s", point)
			}
			if outer != nil {
				t.Fatalf("outer call = %v, want nil", outer)
			}
			if runner.Deferring() {
				t.Fatal("Deferring() still true after the outer call returned")
			}
			if runner.CurMission() != next || next.starts != 1 || next.ends != 0 {
				t.Fatalf("after the outer call: current=%v next starts=%d ends=%d, want next running, started once", runner.CurMission(), next.starts, next.ends)
			}
			if first.ends != 1 {
				t.Fatalf("first mission ended %d time(s), want 1", first.ends)
			}
			if len(reported) != 0 {
				t.Fatalf("unexpected OnError reports: %v", reported)
			}
			// 被发起的任务在第一个任务结束之后才启动（替换也一样：先结束旧的再启动新的）。
			if got := fmt.Sprint(events[len(events)-2:]); got != "[end:first start:next]" {
				t.Fatalf("events = %v, want first to end before next starts", events)
			}
		})
	}
}

// 延后的 StartMission 执行时出错（这里是构建器不存在、旧任务拒绝被替换），发起方早已拿到 nil，
// 错误经 OnError 报告，最外层调用不受影响。
func TestDeferredMissionStartErrorsAreReported(t *testing.T) {
	registry := NewRegistry()
	sticky := &stickyMission{runnerTestMission{kind: 3}}
	if err := registry.RegisterMission(1, func() Mission { return &runnerTestMission{kind: 1} }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(2, func() Mission { return &runnerTestMission{kind: 2} }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(3, func() Mission { return sticky }); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	var reported []error
	var inner []error
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
		OnEnded: func(m Mission, _ ActionReason) {
			if m.Kind() == 1 {
				inner = append(inner, runner.StartMission(9, nil), runner.StartMission(3, nil), runner.StartMission(2, nil))
			}
		},
		OnError: func(err error) { reported = append(reported, err) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	if err := runner.CancelMission("outer"); err != nil {
		t.Fatalf("outer CancelMission = %v, want nil (deferred errors are not the outer call's)", err)
	}
	if len(inner) != 3 || inner[0] != nil || inner[1] != nil || inner[2] != nil {
		t.Fatalf("deferred StartMission returned %v, want nil, nil, nil", inner)
	}
	if len(reported) != 2 || !errors.Is(reported[0], ErrMissionBuilderNotFound) || !errors.Is(reported[1], ErrMissionRunning) ||
		!strings.Contains(reported[0].Error(), "deferred start mission") {
		t.Fatalf("OnError = %v, want [deferred start mission: builder not found, deferred start mission: mission running]", reported)
	}
	if runner.CurMission() != sticky {
		t.Fatalf("current = %v, want the unreplaceable mission", runner.CurMission())
	}
}

// 结束钩子每次都发起一个启动必然失败的任务：失败的任务又走结束钩子，互相触发。执行预算截停，
// 外层拿到 ErrDeferredRunaway 并经 OnError 报告，runner 之后照常可用。
func TestMissionCallbacksThatKeepTriggeringEachOtherAreBounded(t *testing.T) {
	registry := NewRegistry()
	attempts := 0
	if err := registry.RegisterMission(1, func() Mission { return &runnerTestMission{kind: 1} }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(2, func() Mission {
		attempts++
		return &runnerTestMission{kind: 2, startErr: errors.New("refuse")}
	}); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	var reported []error
	loop := true
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, MaxDeferredSteps: 10, Hooks: MissionRunnerHooks{
		OnEnded: func(Mission, ActionReason) {
			if loop {
				_ = runner.StartMission(2, nil)
			}
		},
		OnError: func(err error) { reported = append(reported, err) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	runner.EndCurMission(NewActionReason("kick off"))
	if attempts != 10 {
		t.Fatalf("deferred start attempts = %d, want exactly the step budget 10", attempts)
	}
	var runaway error
	for _, err := range reported {
		if errors.Is(err, ErrDeferredRunaway) {
			runaway = err
		}
	}
	if runaway == nil {
		t.Fatalf("OnError = %v, want ErrDeferredRunaway", reported)
	}
	if err := runner.CancelMission("outer"); !errors.Is(err, ErrMissionNotRunning) {
		t.Fatalf("CancelMission after runaway = %v, want ErrMissionNotRunning (nothing running)", err)
	}
	loop = false
	if runner.Deferring() {
		t.Fatal("Deferring() still true after the runaway was cut off")
	}
	if err := runner.StartMission(1, nil); err != nil || runner.CurMission() == nil {
		t.Fatalf("runner unusable after runaway: err=%v current=%v", err, runner.CurMission())
	}
}

// StartMission 是最外层调用、回调里的变更停在 ErrDeferredRunaway：外层直接返回它。
func TestOuterMissionStartReturnsRunaway(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterMission(1, func() Mission { return &runnerTestMission{kind: 1} }); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	loop := true
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, MaxDeferredSteps: 3, Hooks: MissionRunnerHooks{
		// 任务一运行就结束它，一结束就再启动一个：永不停止。
		OnChanged: func(info MissionInfo) {
			if loop && info.Status == MissionStatusRunning {
				runner.EndCurMission(NewActionReason("again"))
			}
		},
		OnEnded: func(Mission, ActionReason) {
			if loop {
				_ = runner.StartMission(1, nil)
			}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); !errors.Is(err, ErrDeferredRunaway) {
		t.Fatalf("StartMission = %v, want ErrDeferredRunaway", err)
	}
	if runner.Deferring() {
		t.Fatal("Deferring() still true after runaway")
	}
	loop = false
	runner.EndCurMission(NewActionReason("stop"))
	if err := runner.StartMission(1, nil); err != nil || runner.CurMission() == nil {
		t.Fatalf("runner unusable after runaway: err=%v current=%v", err, runner.CurMission())
	}
}

func TestMissionDeferredQueueIsBounded(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterMission(1, func() Mission { return &runnerTestMission{kind: 1} }); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	var inner []error
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, MaxDeferredCommands: 2, Hooks: MissionRunnerHooks{
		OnEnded: func(Mission, ActionReason) {
			if inner == nil {
				inner = []error{runner.StartMission(1, nil), runner.StartMission(1, nil), runner.StartMission(1, nil)}
			}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	runner.EndCurMission(NewActionReason("end"))
	if len(inner) != 3 || inner[0] != nil || inner[1] != nil || !errors.Is(inner[2], ErrDeferredQueueFull) {
		t.Fatalf("queued StartMission results = %v, want nil, nil, ErrDeferredQueueFull", inner)
	}
	if runner.CurMission() == nil {
		t.Fatal("the two accepted deferred starts did not run")
	}
}

// 回调 panic（钩子、任务 Start）被恢复成错误：已经发起的延后命令照常执行，执行状态复位，
// runner 之后照常可用（每个回调点都 recover）。
func TestMissionRunnerStaysUsableAfterCallbacksPanic(t *testing.T) {
	registry := NewRegistry()
	next := &scriptedMission{runnerTestMission: runnerTestMission{kind: 2}, label: "next"}
	if err := registry.RegisterMission(1, func() Mission { return &runnerTestMission{kind: 1} }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(2, func() Mission { return next }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(3, func() Mission {
		return &scriptedMission{runnerTestMission: runnerTestMission{kind: 3}, fire: func(where string) {
			if where == "mission start" {
				panic("start boom")
			}
		}}
	}); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	var reported []error
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
		OnEnded: func(m Mission, _ ActionReason) {
			if m.Kind() == 1 {
				_ = runner.StartMission(3, nil)
				_ = runner.StartMission(2, nil)
				panic("ended boom")
			}
		},
		OnError: func(err error) { reported = append(reported, err) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatal(err)
	}
	runner.EndCurMission(NewActionReason("end"))
	if runner.Deferring() {
		t.Fatal("Deferring() stuck true after a callback panic")
	}
	if runner.CurMission() != next || next.starts != 1 {
		t.Fatalf("deferred start after panics: current=%v next starts=%d", runner.CurMission(), next.starts)
	}
	text := fmt.Sprint(reported)
	if !strings.Contains(text, "ended hook panic: ended boom") || !strings.Contains(text, "deferred start mission") || !strings.Contains(text, "start panic: start boom") {
		t.Fatalf("OnError = %v, want both panics reported", reported)
	}
}

// EndAll 清场（维护者第五轮决定：保持现状，文档写清）：EndAll 先结束全部动作，回调随之发起的
// 动作在它返回后启动——PlanMission 的 OnFail 指向下一步时，EndAll 返回时下一步的动作已在运行。
// 要清场，先 EndCurMission（任务结束后 OnActionEnd 被忽略，不再推进），再 EndAll。
// 本用例钉住这两种顺序的结果，现状即为绿。
func TestEndCurMissionBeforeEndAllLeavesNothingRunning(t *testing.T) {
	newWiring := func(t *testing.T) (*planActionList, *runnerTestAction) {
		t.Helper()
		list := &planActionList{}
		var err error
		list.runner, err = NewActionRunner(ActionRunnerConfig{
			Registry:     newActionOnlyRegistry(t),
			GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
			Hooks: ActionRunnerHooks{OnEnded: func(s ActionSnapshot, reason ActionReason) {
				list.missions.OnActionEnd(s.ID, s.Kind, reason)
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		registry := NewRegistry()
		if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
			t.Fatal(err)
		}
		list.missions, err = NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
			Context:      func(now time.Time) *MissionContext { return &MissionContext{ActionList: list, Now: now} },
			ClearActions: func(id int64, reason ActionReason) error { return list.runner.ClearMission(id, true, reason) },
		}})
		if err != nil {
			t.Fatal(err)
		}
		second := &runnerTestAction{label: "second"}
		plan := MissionPlan{Steps: []MissionStep{
			{Action: 1, Param: &runnerTestAction{label: "first"}, OnFail: NextMissionStep(1)},
			{Action: 2, Param: second},
		}}
		if err := list.missions.StartMission(1, plan); err != nil {
			t.Fatal(err)
		}
		return list, second
	}

	t.Run("EndAll alone", func(t *testing.T) {
		list, second := newWiring(t)
		if err := list.runner.EndAll(true, NewActionReason("clear the field")); err != nil {
			t.Fatal(err)
		}
		if list.runner.Current(1) != second || second.starts != 1 || list.missions.CurMission() == nil {
			t.Fatalf("after EndAll: current=%v second starts=%d mission=%v; want the plan's next step running (documented)",
				list.runner.Current(1), second.starts, list.missions.CurMission())
		}
	})
	t.Run("EndCurMission then EndAll", func(t *testing.T) {
		list, second := newWiring(t)
		list.missions.EndCurMission(NewActionReason("clear the field"))
		if err := list.runner.EndAll(true, NewActionReason("clear the field")); err != nil {
			t.Fatal(err)
		}
		if list.runner.Current(1) != nil || second.starts != 0 || list.missions.CurMission() != nil {
			t.Fatalf("after EndCurMission + EndAll: current=%v second starts=%d mission=%v; want nothing running",
				list.runner.Current(1), second.starts, list.missions.CurMission())
		}
	})
}

type stickyMission struct{ runnerTestMission }

func (*stickyMission) CanReplaceBy(MissionKind, any) bool { return false }

// Missions are exclusive per runner: a kind of 0 with no default is a
// programming error, and a running mission that declines replacement keeps
// the runner (the replacement is not built, the current is not ended).
func TestMissionRunnerRefusesUnknownKindAndUnreplaceableMission(t *testing.T) {
	registry := NewRegistry()
	sticky := &stickyMission{runnerTestMission{kind: 1}}
	built := 0
	if err := registry.RegisterMission(1, func() Mission { return sticky }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(2, func() Mission { built++; return &runnerTestMission{kind: 2} }); err != nil {
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

// 任务计划在归一化时拒绝所有空 / 越界的形状（无步骤、起点越界、动作为 none、后继步骤越界、
// nil 指针），之后按步索引执行时再守一次越界；任务运行器的钩子若在启动途中结束了任务，结束
// 延后到启动之后执行，不把已结束的任务留作当前任务；动作构建器交出 nil 动作被 ErrBuilderNil 拒绝。
// 历史：U-0125

func TestNormalizePlanRefusesEveryInvalidShape(t *testing.T) {
	valid := MissionPlan{Steps: []MissionStep{{Action: 1}, {Action: 2}}}
	if _, err := NormalizePlan(valid); err != nil {
		t.Fatalf("baseline plan rejected: %v", err)
	}
	cases := []struct {
		name string
		plan MissionPlan
		want error
		text string
	}{
		{"no steps", MissionPlan{}, ErrMissionPlanInvalid, ""},
		{"start below zero", MissionPlan{Start: -1, Steps: valid.Steps}, ErrMissionPlanInvalid, ""},
		{"start past the end", MissionPlan{Start: 2, Steps: valid.Steps}, ErrMissionPlanInvalid, ""},
		{"action none", MissionPlan{Steps: []MissionStep{{Action: 1}, {Action: 0}}}, ErrMissionStepInvalid, "action none at 1"},
		{"success next past the end", MissionPlan{Steps: []MissionStep{{Action: 1, OnSuccess: NextMissionStep(5)}}}, ErrMissionStepInvalid, "next step 5"},
		{"fail next below zero", MissionPlan{Steps: []MissionStep{{Action: 1, OnFail: NextMissionStep(-1)}}}, ErrMissionStepInvalid, "next step -1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizePlan(tc.plan)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("NormalizePlan = %v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
	var none *MissionPlan
	if _, err := PlanFrom(none); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanFrom(nil *MissionPlan) = %v", err)
	}
	if _, err := PlanFrom(&valid); err != nil {
		t.Fatalf("PlanFrom(*MissionPlan) = %v", err)
	}
	if _, err := PlanFrom("not a plan"); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanFrom(string) = %v", err)
	}
}

func TestPlanMissionRefusesAStepIndexOutsideThePlan(t *testing.T) {
	plan, err := NormalizePlan(MissionPlan{Steps: []MissionStep{{Action: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	mission := &PlanMission{plan: plan}
	for _, step := range []int{-1, 1, 99} {
		if err := mission.startStep(nil, step); !errors.Is(err, ErrMissionStepInvalid) {
			t.Fatalf("startStep(%d) = %v, want ErrMissionStepInvalid", step, err)
		}
	}
}

// 第五轮决定（MissionRunner 改为延后队列）之后：钩子在启动途中发起的结束延后到启动做完
// 之后执行，StartMission 返回 nil，任务完整启动一次、结束一次。原承诺“不把一个已结束的任务
// 当作当前任务”不变：返回后没有当前任务。
func TestMissionRunnerHookThatEndsTheMissionDuringStartEndsItAfterTheStart(t *testing.T) {
	registry := NewRegistry()
	mission := &runnerTestMission{kind: 1}
	if err := registry.RegisterMission(1, func() Mission { return mission }); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	statusAtEnd := MissionStatusIdle
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
		OnChanged: func(MissionInfo) {
			runner.EndCurMission(NewActionReason("ended from hook"))
		},
		OnState: func(active bool) {
			if !active {
				statusAtEnd = mission.status
			}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); err != nil {
		t.Fatalf("StartMission with a hook ending the mission = %v, want nil (the end is deferred)", err)
	}
	if runner.CurMission() != nil {
		t.Fatalf("a mission ended during its own start is still current: %v", runner.CurMission())
	}
	if mission.ends != 1 || statusAtEnd != MissionStatusCanceled {
		t.Fatalf("mission ends=%d status at end=%v, want ended once after a complete start", mission.ends, statusAtEnd)
	}
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
