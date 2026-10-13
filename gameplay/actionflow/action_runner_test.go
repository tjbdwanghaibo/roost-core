package actionflow

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type runnerTestAction struct {
	label       string
	done        bool
	panicOnTick bool
	startErr    error
	starts      int
	cancels     int
}

func (*runnerTestAction) Kind() ActionKind { return 1 }
func (a *runnerTestAction) Start(*ActionContext) error {
	a.starts++
	return a.startErr
}
func (a *runnerTestAction) Tick(*ActionContext) (bool, ActionResult) {
	if a.panicOnTick {
		panic("tick failed")
	}
	return a.done, ActionResult{Status: ActionStatusSuccess}
}
func (a *runnerTestAction) Cancel(*ActionContext, string) { a.cancels++ }

func newRunnerForTest(t *testing.T, ended *[]ActionReason, reported *[]error) *ActionRunner {
	t.Helper()
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (Action, error) {
		action, ok := param.(*runnerTestAction)
		if !ok {
			return nil, errors.New("invalid action")
		}
		return action, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(ActionKind) (ActionGroup, bool) {
			return 1, true
		},
		Hooks: ActionRunnerHooks{
			OnEnded: func(_ ActionSnapshot, reason ActionReason) {
				*ended = append(*ended, reason)
			},
			OnError: func(err error) { *reported = append(*reported, err) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestActionRunnerExplicitStartPrecedesQueue(t *testing.T) {
	var ended []ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	old := &runnerTestAction{label: "old"}
	queued := &runnerTestAction{label: "queued"}
	replacement := &runnerTestAction{label: "replacement", done: true}
	if _, err := runner.Start(1, old, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Enqueue(1, queued, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, replacement, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := runner.Current(1); got != replacement {
		t.Fatalf("current action = %p, want replacement %p", got, replacement)
	}
	if queued.starts != 0 || replacement.starts != 1 || old.cancels != 1 {
		t.Fatalf("unexpected lifecycle: queued starts=%d replacement starts=%d old cancels=%d", queued.starts, replacement.starts, old.cancels)
	}
	if len(ended) != 1 || ended[0].Result.Status != ActionStatusCanceled {
		t.Fatalf("replacement reason must be canceled: %+v", ended)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := runner.Current(1); got != queued || queued.starts != 1 {
		t.Fatalf("queued action was not started after replacement completed: current=%p starts=%d", got, queued.starts)
	}
	if len(reported) != 0 {
		t.Fatalf("unexpected errors: %v", reported)
	}
}

func TestActionRunnerEnqueueStartsIdleGroup(t *testing.T) {
	var ended []ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	action := &runnerTestAction{label: "queued"}
	id, err := runner.Enqueue(1, action, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 || runner.Current(1) != action || action.starts != 1 || runner.QueueLength(1) != 0 {
		t.Fatalf("idle enqueue did not start: id=%d current=%v starts=%d queued=%d", id, runner.Current(1), action.starts, runner.QueueLength(1))
	}
}

func TestActionRunnerQueuedStartFailureContinuesQueue(t *testing.T) {
	var ended []ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	runner.Freeze(1)
	failed := &runnerTestAction{startErr: errors.New("start")}
	next := &runnerTestAction{}
	if _, err := runner.Enqueue(1, failed, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Enqueue(1, next, 0); err != nil {
		t.Fatal(err)
	}
	if err := runner.Recover(1, time.Now()); !errors.Is(err, failed.startErr) {
		t.Fatalf("recover error = %v", err)
	}
	if runner.Current(1) != next || failed.cancels != 1 || next.starts != 1 {
		t.Fatalf("queue did not continue: current=%v failed cancels=%d next starts=%d", runner.Current(1), failed.cancels, next.starts)
	}
	if len(ended) != 1 || ended[0].Result.Status != ActionStatusFailed || len(reported) != 1 {
		t.Fatalf("failed queue item was not observed: ended=%+v reported=%v", ended, reported)
	}
}

func TestActionRunnerRecoversTickPanic(t *testing.T) {
	var ended []ActionReason
	var reported []error
	runner := newRunnerForTest(t, &ended, &reported)
	action := &runnerTestAction{panicOnTick: true}
	if _, err := runner.Start(1, action, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if runner.Current(1) != nil {
		t.Fatal("panicking action must be removed")
	}
	if len(ended) != 1 || ended[0].Result.Status != ActionStatusFailed {
		t.Fatalf("panic must end as failure: %+v", ended)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "tick panic") {
		t.Fatalf("panic must be reported: %v", reported)
	}
}

func TestRegistrySealAndBuilderPanic(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (Action, error) { panic("builder") }); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	if err := registry.RegisterMission(1, func() Mission { return nil }); !errors.Is(err, ErrRegistrySealed) {
		t.Fatalf("register after seal: %v", err)
	}
	if _, err := registry.BuildAction(1, nil); err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("builder panic not converted to error: %v", err)
	}
}

func BenchmarkActionRunnerTick(b *testing.B) {
	registry := NewRegistry()
	action := &runnerTestAction{}
	if err := registry.RegisterAction(1, func(any) (Action, error) { return action, nil }); err != nil {
		b.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(ActionKind) (ActionGroup, bool) {
			return 1, true
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	if _, err := runner.Start(1, nil, 0, now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := runner.Tick(1, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkActionRunnerLifecycle(b *testing.B) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (Action, error) {
		return &runnerTestAction{done: true}, nil
	}); err != nil {
		b.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry: registry,
		GroupForKind: func(ActionKind) (ActionGroup, bool) {
			return 1, true
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := runner.Start(1, nil, 0, now); err != nil {
			b.Fatal(err)
		}
		if err := runner.Tick(1, now); err != nil {
			b.Fatal(err)
		}
	}
}

// F02-7 是结构性守卫：刻意用未初始化的内部状态让 apply panic，验证最外层入口
// 仍释放 executing。正常业务回调已有 recover；本例不代表发现可达的业务 panic。
func TestOuterExecutionResetsAfterInternalPanic(t *testing.T) {
	r := &ActionRunner{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("uninitialized internal state did not panic")
			}
		}()
		_ = r.submit(runnerCommand{op: opStart, group: 1})
	}()
	if r.Deferring() || r.applyingOuter || r.runaway || len(r.deferred) != 0 {
		t.Fatal("outer execution flags retained after internal panic")
	}
}

func TestActionReasonPreservesStructuredResult(t *testing.T) {
	reason := NewActionResultReason(ActionResult{Status: ActionStatusCanceled, Reason: "shutdown"})
	if got := reason.ToActionResult(ActionStatusFailed); got.Status != ActionStatusCanceled || got.Reason != "shutdown" {
		t.Fatalf("result = %+v", got)
	}
	err := errors.New("boom")
	if got := NewActionErrorReason(err); !errors.Is(got.Err, err) || got.Result.Status != ActionStatusFailed {
		t.Fatalf("error reason = %+v", got)
	}
}

// Update 不在回调里时把 fn 的运行期间也算作回调（fn 里的变更延后到 fn 返回后执行）。
// fn panic 被恢复成错误返回，执行状态复位：之后的 Start 照常立即启动，不会全部变成永不执行的延后命令。
// 历史：RR-20261005-NC-242
func TestUpdateWhoseFnPanicsLeavesTheRunnerUsable(t *testing.T) {
	runner := newReentrancyRunner(t)
	first := &runnerTestAction{label: "first"}
	if _, err := runner.Start(1, first, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	var updateErr error
	func() {
		defer func() { _ = recover() }() // 修前 panic 向上传播
		updateErr = runner.Update(1, func(Action) error { panic("update fn failed") })
	}()
	if runner.Deferring() {
		t.Fatal("runner still reports Deferring() after Update's fn panicked: every later mutation is parked forever")
	}
	next := &runnerTestAction{label: "next"}
	if _, err := runner.Start(1, next, 0, time.Now()); err != nil {
		t.Fatalf("Start after the panicking Update = %v", err)
	}
	if next.starts != 1 || runner.Current(1) != next {
		t.Fatalf("Start after the panicking Update did not run: starts=%d current is next=%v", next.starts, runner.Current(1) == next)
	}
	if updateErr == nil || !strings.Contains(updateErr.Error(), "update fn failed") {
		t.Fatalf("Update error = %v, want the recovered panic", updateErr)
	}
}

// Start 替换当前动作时，旧动作的 Cancel panic 只经 OnError 报告：旧动作照样被替换、结束一次，
// 新动作照常启动——回调里发起的 Start 已把 ID 交给调用方，拿到 ID 的动作必须有结论。
// 历史：RR-20261005-NC-243
type cancelPanicAction struct{ runnerTestAction }

func (a *cancelPanicAction) Cancel(*ActionContext, string) { panic("cancel exploded") }

// tickStarter 在 Tick 里发起一次 Start(next)，记下拿到的 ID。
type tickStarter struct {
	cancelPanicAction
	runner *ActionRunner
	next   Action
	id     int64
	err    error
}

func (a *tickStarter) Tick(*ActionContext) (bool, ActionResult) {
	if a.id == 0 {
		a.id, a.err = a.runner.Start(1, a.next, 0, time.Now())
	}
	return false, ActionResult{}
}

func TestReplacedActionWhoseCancelPanicsStillLetsTheNewActionStart(t *testing.T) {
	t.Run("deferred_start", func(t *testing.T) {
		ended := map[int64]int{}
		var reported []error
		var runner *ActionRunner
		runner = newReentrancyRunner(t, ActionRunnerHooks{
			OnEnded: func(s ActionSnapshot, _ ActionReason) { ended[s.ID]++ },
			OnError: func(err error) { reported = append(reported, err) },
		})
		next := &runnerTestAction{label: "next"}
		outer := &tickStarter{runner: runner, next: next}
		if _, err := runner.Start(1, outer, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := runner.Tick(1, time.Now()); err != nil {
			t.Fatalf("outer Tick = %v", err)
		}
		if outer.err != nil || outer.id == 0 {
			t.Fatalf("deferred Start = (%d, %v), want an ID and nil", outer.id, outer.err)
		}
		if next.starts != 1 || runner.Current(1) != next {
			t.Fatalf("the action whose ID was handed out never started: starts=%d ended=%d (B7: an ID always reaches a conclusion)",
				next.starts, ended[outer.id])
		}
		if !reportedContains(reported, "cancel exploded") {
			t.Fatalf("reported = %v, want the replaced action's cancel panic", reported)
		}
	})
	t.Run("direct_start", func(t *testing.T) {
		var reported []error
		runner := newReentrancyRunner(t, ActionRunnerHooks{OnError: func(err error) { reported = append(reported, err) }})
		if _, err := runner.Start(1, &cancelPanicAction{}, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		next := &runnerTestAction{label: "next"}
		id, err := runner.Start(1, next, 0, time.Now())
		if err != nil || id == 0 {
			t.Fatalf("Start replacing an action whose Cancel panics = (%d, %v), want an ID and nil", id, err)
		}
		if next.starts != 1 || runner.Current(1) != next {
			t.Fatalf("group left idle: next starts=%d", next.starts)
		}
		if !reportedContains(reported, "cancel exploded") {
			t.Fatalf("reported = %v, want the replaced action's cancel panic", reported)
		}
	})
}

func reportedContains(errs []error, text string) bool {
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), text) {
			return true
		}
	}
	return false
}

// 已发过 OnQueued、ID 已交给调用方的排队动作，被 ClearQueue / EndAll / ClearMission 丢弃时
// 发一次 OnEnded（取消状态），与“排队动作启动失败也发 OnEnded”一致。
// 历史：RR-20261005-NC-121
func TestDiscardedQueuedActionsAreReportedAsEnded(t *testing.T) {
	for _, op := range []string{"ClearQueue", "EndAll", "ClearMission"} {
		t.Run(op, func(t *testing.T) {
			var queued []int64
			ended := map[int64]ActionReason{}
			var transitions []int64
			runner := newReentrancyRunner(t, ActionRunnerHooks{
				OnQueued:     func(s ActionSnapshot) { queued = append(queued, s.ID) },
				OnTransition: func(s ActionSnapshot, _ bool) { transitions = append(transitions, s.ID) },
				OnEnded:      func(s ActionSnapshot, reason ActionReason) { ended[s.ID] = reason },
			})
			const missionID = 7
			if _, err := runner.Start(1, &runnerTestAction{label: "running"}, missionID, time.Now()); err != nil {
				t.Fatal(err)
			}
			waiting := &runnerTestAction{label: "queued"}
			queuedID, err := runner.Enqueue(1, waiting, missionID)
			if err != nil {
				t.Fatal(err)
			}
			switch op {
			case "ClearQueue":
				runner.ClearQueue()
			case "EndAll":
				if err := runner.EndAll(true, NewActionReason("end all")); err != nil {
					t.Fatal(err)
				}
			case "ClearMission":
				if err := runner.ClearMission(missionID, true, NewActionReason("mission over")); err != nil {
					t.Fatal(err)
				}
			}
			if len(queued) != 1 || queued[0] != queuedID {
				t.Fatalf("queued hook = %v, want [%d]", queued, queuedID)
			}
			reason, ok := ended[queuedID]
			if !ok {
				t.Fatalf("%s dropped queued action %d without OnEnded (ended=%v)", op, queuedID, ended)
			}
			if reason.Result.Status != ActionStatusCanceled {
				t.Fatalf("queued action %d ended with %+v, want canceled", queuedID, reason.Result)
			}
			if runner.QueueLength(1) != 0 {
				t.Fatalf("queue length after %s = %d", op, runner.QueueLength(1))
			}
			// 从未启动的动作不调用 Start / Cancel，也不发激活切换。
			if waiting.starts != 0 || waiting.cancels != 0 {
				t.Fatalf("discarded queued action was started=%d canceled=%d", waiting.starts, waiting.cancels)
			}
			for _, id := range transitions {
				if id == queuedID {
					t.Fatalf("discarded queued action %d got a transition", queuedID)
				}
			}
		})
	}
}

// 控制：ClearMission 只丢本任务的排队项，别的任务的排队项既不结束也不出队。
func TestClearMissionKeepsAnotherMissionsQueuedActions(t *testing.T) {
	ended := map[int64]bool{}
	runner := newReentrancyRunner(t, ActionRunnerHooks{
		OnEnded: func(s ActionSnapshot, _ ActionReason) { ended[s.ID] = true },
	})
	if _, err := runner.Start(1, &runnerTestAction{}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	otherID, err := runner.Enqueue(1, &runnerTestAction{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ClearMission(3, true, NewActionReason("unrelated")); err != nil {
		t.Fatal(err)
	}
	if ended[otherID] || runner.QueueLength(1) != 1 {
		t.Fatalf("ClearMission(3) touched mission 2's queued action: ended=%v queue=%d", ended, runner.QueueLength(1))
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

// A frozen action group is one the owner has deliberately parked (a scene
// transition, a disconnect); nothing may start in it until Recover.
func TestActionRunnerRefusesToStartInAFrozenGroup(t *testing.T) {
	var ended []ActionReason
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

func TestActionRunnerRefusesABuilderThatReturnsNoAction(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (Action, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{Registry: registry, GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, nil, 0, time.Now()); !errors.Is(err, ErrBuilderNil) {
		t.Fatalf("Start with a builder returning no action = %v, want ErrBuilderNil", err)
	}
	if runner.Current(1) != nil {
		t.Fatal("a nil action was installed as current")
	}
}

// 包的错误都以包名 actionflow: 开头，日志与错误链里能直接看出出处。
func TestErrorsArePrefixedWithThePackageName(t *testing.T) {
	for _, err := range []error{
		ErrActionBuilderDuplicate, ErrActionBuilderNotFound, ErrActionGroupFrozen, ErrActionGroupInvalid,
		ErrActionIDExhausted, ErrBuilderNil, ErrDeferredQueueFull, ErrDeferredRunaway, ErrKindInvalid,
		ErrMissionBuilderDuplicate, ErrMissionBuilderNotFound, ErrMissionIDExhausted, ErrMissionNotRunning,
		ErrMissionPlanInvalid, ErrMissionRunning, ErrMissionStepInvalid, ErrRegistrySealed,
	} {
		if !strings.HasPrefix(err.Error(), "actionflow: ") {
			t.Errorf("error %q does not start with the package name", err)
		}
	}
}
