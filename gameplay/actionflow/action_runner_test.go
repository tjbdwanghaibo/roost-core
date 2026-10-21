package actionflow

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type runnerTestAction struct {
	label       string
	done        bool
	panicOnTick bool
	startErr    error
	// instant 让 Start 报告启动即完成，结果是 instantResult（D21 A7）。
	instant       bool
	instantResult ActionResult
	starts        int
	cancels       int
}

func (*runnerTestAction) Kind() ActionKind { return 1 }
func (a *runnerTestAction) Start(*ActionContext) (bool, ActionResult, error) {
	a.starts++
	return a.instant, a.instantResult, a.startErr
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
	runner.Freeze(1, FreezePause)
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
	runner.Freeze(1, FreezePause)
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
		ErrActionGroupNotFrozen, ErrActionNotFound, ErrReplaceRejected, ErrBlockedByGroup,
	} {
		if !strings.HasPrefix(err.Error(), "actionflow: ") {
			t.Errorf("error %q does not start with the package name", err)
		}
	}
}

// --- D21 第 1 期：Start 即完成、取代否决、跨组冲突、按 ID 结束、计数冻结 ---

// groupLog 是多组 runner 的观察记录：每次结束的 “ID 状态 原因”、每次报告的错误、每次冻结切换。
type groupLog struct {
	ended    []string
	reasons  map[int64]ActionReason
	reported []error
	freezes  []string
}

// newGroupsRunner 构造多组 runner：动作种类就是组号（kind k 在组 k），参数是 *runnerTestAction 或任意 Action。
func newGroupsRunner(t *testing.T, log *groupLog, conflict func(running, incoming ActionSnapshot) Conflict) *ActionRunner {
	t.Helper()
	registry := NewRegistry()
	for kind := ActionKind(1); kind <= 4; kind++ {
		if err := registry.RegisterAction(kind, func(param any) (Action, error) { return param.(Action), nil }); err != nil {
			t.Fatal(err)
		}
	}
	log.reasons = make(map[int64]ActionReason)
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), kind >= 1 && kind <= 4 },
		Conflict:     conflict,
		Hooks: ActionRunnerHooks{
			OnEnded: func(s ActionSnapshot, reason ActionReason) {
				result := reason.ToActionResult(ActionStatusSuccess)
				log.ended = append(log.ended, fmt.Sprintf("%d %d %s", s.ID, result.Status, result.Reason))
				log.reasons[s.ID] = reason
			},
			OnError:  func(err error) { log.reported = append(log.reported, err) },
			OnFreeze: func(g ActionGroup, frozen bool) { log.freezes = append(log.freezes, fmt.Sprintf("%d %v", g, frozen)) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// 启动即完成（A7）：Start 报告 done 的动作不进入运行、不被 Cancel，立即以它的结果结束（结果为空算成功），本组队列
// 随即推进；回调里延后启动的瞬时动作同样以自己的 ID 结束一次。
func TestAnActionThatCompletesInStartEndsAtOnceAndTheQueueMoves(t *testing.T) {
	var log groupLog
	runner := newGroupsRunner(t, &log, nil)
	blocker := &runnerTestAction{label: "blocker"}
	if _, err := runner.Start(1, blocker, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	queued := &runnerTestAction{label: "queued"}
	if _, err := runner.Enqueue(1, queued, 0); err != nil {
		t.Fatal(err)
	}
	instant := &runnerTestAction{label: "instant", instant: true}
	id, err := runner.Start(1, instant, 0, time.Now())
	if err != nil || id == 0 {
		t.Fatalf("instant Start = %d, %v", id, err)
	}
	if runner.Current(1) != queued || instant.starts != 1 || instant.cancels != 0 || queued.starts != 1 {
		t.Fatalf("after an instant action: current %v (want the queued one), instant starts %d cancels %d, queued starts %d",
			runner.Current(1), instant.starts, instant.cancels, queued.starts)
	}
	want := []string{"1 4 replaced by next action", fmt.Sprintf("%d 2 action done", id)}
	if fmt.Sprint(log.ended) != fmt.Sprint(want) {
		t.Fatalf("ended %v, want %v", log.ended, want)
	}

	failed := &runnerTestAction{label: "instant failure", instant: true, instantResult: ActionResult{Status: ActionStatusFailed, Reason: "target gone"}}
	id, err = runner.Start(2, failed, 0, time.Now())
	if err != nil || runner.Current(2) != nil || log.ended[len(log.ended)-1] != fmt.Sprintf("%d 3 target gone", id) {
		t.Fatalf("instant failure: err %v current %v ended %v", err, runner.Current(2), log.ended)
	}

	// 回调里延后的瞬时动作：拿到的 ID 在回调返回后结束一次。
	inner := &runnerTestAction{label: "inner", instant: true}
	starter := &callbackStarter{runner: runner, from: "tick", next: inner}
	if _, err := runner.Start(1, starter, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if starter.innerErr != nil || starter.innerID == 0 || !strings.HasPrefix(log.ended[len(log.ended)-1], fmt.Sprintf("%d 2 ", starter.innerID)) || inner.starts != 1 {
		t.Fatalf("deferred instant action: id %d err %v starts %d ended %v", starter.innerID, starter.innerErr, inner.starts, log.ended)
	}
	if len(log.reported) != 0 {
		t.Fatalf("reported %v", log.reported)
	}
}

// keepingAction 是实现了 Replacer 的动作：按 decide 回答，并记下被问到的新动作。
type keepingAction struct {
	runnerTestAction
	decide func(next ActionSnapshot) ReplaceDecision
	asked  []ActionKind
}

func (a *keepingAction) ReplaceBy(_ *ActionContext, next ActionSnapshot) ReplaceDecision {
	a.asked = append(a.asked, next.Kind)
	return a.decide(next)
}

// 取代否决（A2）：Keep 保留当前动作，新动作拿到 ID、不 Start、以取消结束一次，Start 返回 nil；Reject 保留当前动作，
// 直接调用返回 ErrReplaceRejected，回调里延后的以失败结束；强制结束（End / EndAction）不问。
func TestTheCurrentActionCanKeepItselfOrRejectAReplacement(t *testing.T) {
	var log groupLog
	runner := newGroupsRunner(t, &log, nil)
	decision := ReplaceKeep
	current := &keepingAction{decide: func(ActionSnapshot) ReplaceDecision { return decision }}
	if _, err := runner.Start(1, current, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	same := &runnerTestAction{label: "same request"}
	id, err := runner.Start(1, same, 0, time.Now())
	if err != nil || id == 0 || runner.Current(1) != current || same.starts != 0 || current.cancels != 0 {
		t.Fatalf("keep: Start = %d, %v; current kept %v; new starts %d; current cancels %d", id, err, runner.Current(1) == current, same.starts, current.cancels)
	}
	if got := log.ended; len(got) != 1 || got[0] != fmt.Sprintf("%d 4 kept current action", id) {
		t.Fatalf("keep: ended %v", got)
	}

	decision = ReplaceReject
	rejected := &runnerTestAction{label: "rejected"}
	if _, err := runner.Start(1, rejected, 0, time.Now()); !errors.Is(err, ErrReplaceRejected) || runner.Current(1) != current || rejected.starts != 0 {
		t.Fatalf("reject: Start = %v, current kept %v, starts %d", err, runner.Current(1) == current, rejected.starts)
	}
	// 回调里被拒：ID 已交出，以失败结束。
	deferred := &runnerTestAction{label: "deferred"}
	var deferredID int64
	if err := runner.Update(1, func(Action) error {
		deferredID, err = runner.Start(1, deferred, 0, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if reason := log.reasons[deferredID]; deferredID == 0 || !errors.Is(reason.Err, ErrReplaceRejected) || reason.Result.Status != ActionStatusFailed {
		t.Fatalf("deferred reject: id %d reason %+v", deferredID, reason)
	}
	if fmt.Sprint(current.asked) != "[1 1 1]" {
		t.Fatalf("Replacer asked for %v", current.asked)
	}
	if err := runner.End(1, true, NewActionReason("forced")); err != nil || runner.Current(1) != nil || len(current.asked) != 3 {
		t.Fatalf("forced end: %v current %v asked %v", err, runner.Current(1), current.asked)
	}
}

// 跨组冲突（A3）：BlockIncoming 拒绝新动作、什么都不改（直接调用返回 ErrBlockedByGroup，排队项以失败结束）；
// PreemptRunning 先取消对方组的当前动作（取消状态）、不推进对方的队列，再启动新动作；nil 规则下互不影响。
func TestConflictRulesBlockOrPreemptOtherGroups(t *testing.T) {
	var log groupLog
	// 组 2 的动作挡住组 1；组 3 的动作抢占组 1；组 4 与谁都不冲突。
	runner := newGroupsRunner(t, &log, func(running, incoming ActionSnapshot) Conflict {
		switch {
		case running.Group == 2 && incoming.Group == 1:
			return BlockIncoming
		case running.Group == 1 && incoming.Group == 3:
			return PreemptRunning
		}
		return NoConflict
	})
	casting := &runnerTestAction{label: "cast"}
	if _, err := runner.Start(2, casting, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	walk := &runnerTestAction{label: "walk"}
	if _, err := runner.Start(1, walk, 0, time.Now()); !errors.Is(err, ErrBlockedByGroup) || walk.starts != 0 || runner.Current(2) != casting {
		t.Fatalf("blocked start: %v starts %d", err, walk.starts)
	}
	// 排队项成为当前动作时同样受约束：以失败结束（经 OnError 报告），队列接着试下一个。
	queuedWalk := &runnerTestAction{label: "queued walk"}
	queuedID, err := runner.Enqueue(1, queuedWalk, 0)
	if err == nil || !errors.Is(err, ErrBlockedByGroup) || queuedWalk.starts != 0 {
		t.Fatalf("blocked queued head: %v starts %d", err, queuedWalk.starts)
	}
	if reason := log.reasons[queuedID]; !errors.Is(reason.Err, ErrBlockedByGroup) || reason.Result.Status != ActionStatusFailed {
		t.Fatalf("blocked queued head reason %+v", reason)
	}
	if err := runner.End(2, true, NewActionReason("cast done")); err != nil {
		t.Fatal(err)
	}

	walkID, err := runner.Start(1, walk, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	later := &runnerTestAction{label: "later walk"}
	if _, err := runner.Enqueue(1, later, 0); err != nil {
		t.Fatal(err)
	}
	other := &runnerTestAction{label: "unrelated"}
	if _, err := runner.Start(4, other, 0, time.Now()); err != nil || runner.Current(1) != walk {
		t.Fatalf("no conflict: %v walk still current %v", err, runner.Current(1) == walk)
	}
	stun := &runnerTestAction{label: "knockback"}
	stunID, err := runner.Start(3, stun, 0, time.Now())
	if err != nil || runner.Current(3) != stun || runner.Current(1) != nil || walk.cancels != 1 || later.starts != 0 || runner.QueueLength(1) != 1 {
		t.Fatalf("preempt: %v current3 %v current1 %v walk cancels %d later starts %d queue %d",
			err, runner.Current(3) == stun, runner.Current(1), walk.cancels, later.starts, runner.QueueLength(1))
	}
	if last := log.ended[len(log.ended)-1]; last != fmt.Sprintf("%d 4 preempted by action %d", walkID, stunID) {
		t.Fatalf("preempted ended %q (log %v)", last, log.ended)
	}
	if len(log.reported) != 1 || !errors.Is(log.reported[0], ErrBlockedByGroup) {
		t.Fatalf("reported %v", log.reported)
	}
}

// 按 ID 结束（A4）：当前动作按 force 结束并推进队列；排队项移出队列、以取消结束；找不到时直接调用返回
// ErrActionNotFound，回调里延后的找不到不报告。
func TestEndActionEndsTheCurrentOrAQueuedActionByID(t *testing.T) {
	var log groupLog
	runner := newGroupsRunner(t, &log, nil)
	first := &runnerTestAction{label: "first"}
	firstID, _ := runner.Start(1, first, 0, time.Now())
	second := &runnerTestAction{label: "second"}
	secondID, _ := runner.Enqueue(1, second, 0)
	third := &runnerTestAction{label: "third"}
	thirdID, _ := runner.Enqueue(1, third, 0)

	if err := runner.EndAction(thirdID, false, NewActionReason("not needed")); err != nil {
		t.Fatal(err)
	}
	if runner.QueueLength(1) != 1 || third.starts != 0 || log.ended[0] != fmt.Sprintf("%d 4 not needed", thirdID) {
		t.Fatalf("queued end: queue %d starts %d ended %v", runner.QueueLength(1), third.starts, log.ended)
	}
	if err := runner.EndAction(firstID, true, NewActionReason("stop")); err != nil {
		t.Fatal(err)
	}
	if runner.Current(1) != second || first.cancels != 1 || log.ended[1] != fmt.Sprintf("%d 4 stop", firstID) {
		t.Fatalf("current end: current second %v cancels %d ended %v", runner.Current(1) == second, first.cancels, log.ended)
	}
	if err := runner.EndAction(firstID, true, NewActionReason("again")); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("ending an ended action = %v", err)
	}
	if err := runner.Update(1, func(Action) error { return runner.EndAction(firstID, true, NewActionReason("deferred")) }); err != nil {
		t.Fatal(err)
	}
	if err := runner.EndAction(secondID, false, NewActionResultReason(ActionResult{Status: ActionStatusSuccess, Reason: "done early"})); err != nil {
		t.Fatal(err)
	}
	if second.cancels != 0 || log.ended[2] != fmt.Sprintf("%d 2 done early", secondID) || len(log.reported) != 0 {
		t.Fatalf("non-forced end: cancels %d ended %v reported %v", second.cancels, log.ended, log.reported)
	}
}

// 计数冻结（A5）：两个来源各冻结一次，撤销一次仍冻结，全部撤销才解冻并推进队列；多撤销一次返回
// ErrActionGroupNotFrozen、计数不变负；暂停模式不打断当前动作，冻结期间 Tick 不推进它；打断模式取消当前动作、
// 保留排队项。OnFreeze 只在 0↔1 时调用。
func TestFreezingIsCountedAndCancelModeInterruptsTheCurrentAction(t *testing.T) {
	var log groupLog
	runner := newGroupsRunner(t, &log, nil)
	walking := &runnerTestAction{label: "walking", done: true}
	if _, err := runner.Start(1, walking, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Freeze(1, FreezePause); err != nil {
		t.Fatal(err)
	}
	if err := runner.Freeze(1, FreezePause); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil || runner.Current(1) != walking || runner.FreezeCount(1) != 2 {
		t.Fatalf("paused group: tick %v current %v count %d", err, runner.Current(1), runner.FreezeCount(1))
	}
	if err := runner.Recover(1, time.Now()); err != nil || !runner.Frozen(1) {
		t.Fatalf("one of two sources recovered: %v frozen %v", err, runner.Frozen(1))
	}
	if err := runner.Recover(1, time.Now()); err != nil || runner.Frozen(1) {
		t.Fatalf("both recovered: %v frozen %v", err, runner.Frozen(1))
	}
	if err := runner.Recover(1, time.Now()); !errors.Is(err, ErrActionGroupNotFrozen) || runner.FreezeCount(1) != 0 {
		t.Fatalf("unbalanced Recover = %v count %d", err, runner.FreezeCount(1))
	}
	if err := runner.Tick(1, time.Now()); err != nil || runner.Current(1) != nil || walking.cancels != 0 {
		t.Fatalf("resumed action did not finish on its tick: %v current %v cancels %d", err, runner.Current(1), walking.cancels)
	}

	moving := &runnerTestAction{label: "moving"}
	movingID, _ := runner.Start(1, moving, 0, time.Now())
	next := &runnerTestAction{label: "next"}
	if _, err := runner.Enqueue(1, next, 0); err != nil {
		t.Fatal(err)
	}
	if err := runner.Freeze(1, FreezeCancel); err != nil {
		t.Fatal(err)
	}
	if runner.Current(1) != nil || moving.cancels != 1 || runner.QueueLength(1) != 1 || next.starts != 0 {
		t.Fatalf("cancel freeze: current %v cancels %d queue %d next starts %d", runner.Current(1), moving.cancels, runner.QueueLength(1), next.starts)
	}
	if reason := log.reasons[movingID]; !errors.Is(reason.Err, ErrActionGroupFrozen) || reason.Result.Status != ActionStatusCanceled {
		t.Fatalf("interrupted reason %+v", reason)
	}
	if _, err := runner.Start(1, &runnerTestAction{}, 0, time.Now()); !errors.Is(err, ErrActionGroupFrozen) {
		t.Fatalf("Start while frozen = %v", err)
	}
	if err := runner.Recover(1, time.Now()); err != nil || runner.Current(1) != next {
		t.Fatalf("recover after cancel freeze: %v current %v", err, runner.Current(1))
	}
	if want := "[1 true 1 false 1 true 1 false]"; fmt.Sprint(log.freezes) != want {
		t.Fatalf("OnFreeze %v, want %v", log.freezes, want)
	}
	if len(log.reported) != 0 {
		t.Fatalf("reported %v", log.reported)
	}
}

// 回调里的 FreezeCancel：计数立即生效（之后的 Start 被拒），打断延后到回调返回后执行。
func TestCancelFreezeFromACallbackInterruptsAfterTheCallback(t *testing.T) {
	var log groupLog
	runner := newGroupsRunner(t, &log, nil)
	moving := &runnerTestAction{label: "moving"}
	if _, err := runner.Start(1, moving, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	var inside struct {
		frozen  bool
		current Action
		start   error
	}
	if err := runner.Update(2, func(Action) error { return nil }); err != nil {
		t.Fatal(err)
	}
	worker := &runnerTestAction{label: "worker"}
	if _, err := runner.Start(2, worker, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Update(2, func(Action) error {
		if err := runner.Freeze(1, FreezeCancel); err != nil {
			return err
		}
		inside.frozen, inside.current = runner.Frozen(1), runner.Current(1)
		_, inside.start = runner.Start(1, &runnerTestAction{}, 0, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !inside.frozen || inside.current != moving || inside.start != nil {
		t.Fatalf("inside the callback: frozen %v current moving %v deferred start %v", inside.frozen, inside.current == moving, inside.start)
	}
	if runner.Current(1) != nil || moving.cancels != 1 {
		t.Fatalf("after the callback: current %v cancels %d", runner.Current(1), moving.cancels)
	}
	// 延后的 Start 在打断之后执行，组已冻结：以失败结束。
	if last := log.ended[len(log.ended)-1]; !strings.Contains(last, " 3 ") {
		t.Fatalf("deferred start into the frozen group ended %v", log.ended)
	}
}

// nowRecordingAction 记下 Start 收到的 ActionContext.Now。
type nowRecordingAction struct{ started time.Time }

func (*nowRecordingAction) Kind() ActionKind { return 7 }
func (a *nowRecordingAction) Start(ctx *ActionContext) (bool, ActionResult, error) {
	a.started = ctx.Now
	return false, ActionResult{}, nil
}
func (*nowRecordingAction) Tick(*ActionContext) (bool, ActionResult) { return false, ActionResult{} }
func (*nowRecordingAction) Cancel(*ActionContext, string)            {}

// steppingBusinessClock 让进程业务时钟每读一次走 1ms：现读第二次就与执行开始时冻结的值不同。
func steppingBusinessClock(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	tick := time.UnixMilli(1_800_000_000_000)
	clock.SetSource(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Millisecond)
		return tick
	})
	t.Cleanup(func() { clock.SetSource(nil) })
}

// D22-1：没给时间时，ActionContext.Now 与 MissionRunner 的缺省时间是本次执行冻结的业务时间（fctx.Now），
// 与 handler 里读的同一时刻。
func TestDefaultNowIsTheExecutionsFrozenBusinessTime(t *testing.T) {
	steppingBusinessClock(t)
	execution, release := fctx.NewContext()
	defer release()

	registry := NewRegistry()
	if err := registry.RegisterAction(7, func(param any) (Action, error) { return param.(*nowRecordingAction), nil }); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{Registry: registry, GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true }})
	if err != nil {
		t.Fatal(err)
	}
	action := &nowRecordingAction{}
	if _, err := runner.Start(7, action, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !action.started.Equal(execution.Now) {
		t.Fatalf("ActionContext.Now = %v, want the execution's frozen business time %v", action.started, execution.Now)
	}
	if got := (&MissionRunner{}).now(); !got.Equal(execution.Now) {
		t.Fatalf("MissionRunner default now = %v, want the execution's frozen business time %v", got, execution.Now)
	}
}
