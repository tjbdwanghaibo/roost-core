package actionflow

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// 回调——动作的 Start / Tick / Cancel，以及 OnQueued / OnTransition / OnEnded 钩子——里对
// ActionRunner 的变更（Start、Enqueue、End、EndAll、ClearQueue、ClearMission、Recover、Tick）
// 不立即执行，进入延后命令队列，等最外层调用把自己的那一步做完后按发起顺序执行；判定只在
// ActionRunner.submit 一处。
//
// 承诺：
//   - 回调里调用变更方法时 Deferring() 为真；Start / Enqueue 立即返回已分配的 ID、错误为 nil，
//     回调返回前当前位不变、被发起的动作还没有 Start；最外层返回前命令全部执行完。
//   - 拿到 ID 的动作一定有结论：启动了就照常结束；延后执行时已无法启动（组被冻结、预算
//     耗尽）也发一次 OnEnded。
//   - 延后队列有界（ErrDeferredQueueFull），回调互相触发不会无限执行（ErrDeferredRunaway），
//     之后 runner 照常可用。
// 历史：维护者 2026-10-05 决定 B7（方向 b），取代 U-0100 / NC-122 的 ErrReentrantMutation 分支

// callbackStarter 在指定回调里发起一次 Start(next)，并记录回调内看到的状态。
type callbackStarter struct {
	runnerTestAction
	runner   *ActionRunner
	from     string
	next     Action
	fired    bool
	seen     callbackView
	innerID  int64
	innerErr error
}

type callbackView struct {
	deferring    bool
	current      Action
	nextStarts   int
	queueLength  int
	snapshotSeen bool
}

func (a *callbackStarter) fire(where string) {
	if a.from != where || a.fired {
		return
	}
	a.fired = true
	a.innerID, a.innerErr = a.runner.Start(1, a.next, 0, time.Now())
	a.seen = callbackView{deferring: a.runner.Deferring(), current: a.runner.Current(1), queueLength: a.runner.QueueLength(1)}
	if counted, ok := a.next.(*runnerTestAction); ok {
		a.seen.nextStarts = counted.starts
	}
}

func (a *callbackStarter) Start(ctx *ActionContext) error {
	a.fire("start")
	return a.runnerTestAction.Start(ctx)
}

func (a *callbackStarter) Tick(ctx *ActionContext) (bool, ActionResult) {
	a.fire("tick")
	return a.runnerTestAction.Tick(ctx)
}

func (a *callbackStarter) Cancel(ctx *ActionContext, reason string) {
	a.fire("cancel")
	a.runnerTestAction.Cancel(ctx, reason)
}

func TestCallbackMutationsAreDeferredUntilTheOuterCallReturns(t *testing.T) {
	for _, from := range []string{"start", "tick", "cancel", "transition", "ended", "queued"} {
		t.Run(from, func(t *testing.T) {
			next := &runnerTestAction{label: "next"}
			var runner *ActionRunner
			var outer *callbackStarter
			ended := map[int64]int{}
			runner = newReentrancyRunner(t, ActionRunnerHooks{
				OnTransition: func(s ActionSnapshot, entering bool) {
					if entering && s.Action == outer {
						outer.fire("transition")
					}
				},
				OnEnded: func(s ActionSnapshot, _ ActionReason) {
					ended[s.ID]++
					if s.Action == outer {
						outer.fire("ended")
					}
				},
				OnQueued: func(s ActionSnapshot) {
					if s.Action == outer {
						outer.fire("queued")
					}
				},
			})
			outer = &callbackStarter{runnerTestAction: runnerTestAction{label: "outer", done: from == "ended"}, runner: runner, from: from, next: next}
			var before Action
			var err error
			switch from {
			case "start", "transition":
				_, err = runner.Start(1, outer, 0, time.Now())
			case "tick", "ended":
				if _, err = runner.Start(1, outer, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
				if from == "tick" {
					before = outer // ended：runner 自己在发 OnEnded 前已清当前位，回调里看到 nil
				}
				err = runner.Tick(1, time.Now())
			case "cancel":
				if _, err = runner.Start(1, outer, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
				before = outer
				err = runner.End(1, true, NewActionReason("stop"))
			case "queued":
				blocker := &runnerTestAction{label: "blocker"}
				if _, err = runner.Start(1, blocker, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
				before = blocker
				_, err = runner.Enqueue(1, outer, 0)
			}
			if err != nil {
				t.Fatalf("outer call from %s = %v, want nil: a callback mutation is deferred, not a conflict", from, err)
			}
			if !outer.fired {
				t.Fatalf("callback %s never fired", from)
			}
			if outer.innerErr != nil || outer.innerID == 0 {
				t.Fatalf("Start inside %s = (%d, %v), want an allocated id and nil", from, outer.innerID, outer.innerErr)
			}
			if !outer.seen.deferring {
				t.Fatalf("Deferring() was false inside %s", from)
			}
			if outer.seen.nextStarts != 0 {
				t.Fatalf("the deferred action was started inside the %s callback", from)
			}
			if from != "start" && from != "transition" && outer.seen.current != before {
				t.Fatalf("current changed inside the %s callback: %v, want %v", from, outer.seen.current, before)
			}
			if runner.Deferring() {
				t.Fatal("Deferring() still true after the outer call returned")
			}
			if got := runner.Current(1); got != next || next.starts != 1 {
				t.Fatalf("after %s: current=%v next starts=%d, want the deferred action running once", from, got, next.starts)
			}
			// 被替换的动作恰好 Cancel 一次、结束一次。
			if from != "ended" && from != "queued" && (outer.cancels != 1 || ended[1] != 1) {
				t.Fatalf("replaced action canceled %d / ended %d time(s), want 1 / 1", outer.cancels, ended[1])
			}
		})
	}
}

// 直接 Start 失败也发一次 OnEnded，与排队 / 延后路径对称：进入切换的动作恰好离开一次、结束一次。
func TestDirectStartFailureEndsTheActionOnce(t *testing.T) {
	runner, trace := newStartFailureRunner(t)
	failed := &runnerTestAction{startErr: errStartRefused}
	if _, err := runner.Start(1, failed, 0, time.Now()); !errors.Is(err, errStartRefused) {
		t.Fatalf("Start = %v", err)
	}
	if trace.leaves[1] != 1 || trace.ended[1] != 1 {
		t.Fatalf("failed action left %d / ended %d time(s), want 1 / 1", trace.leaves[1], trace.ended[1])
	}
}

// 回调里发起、延后执行时已无法启动（这里是同一回调随后冻结了组）：ID 已交给调用方，必须
// 收到一次 OnEnded，错误经 OnError 报告。
func TestDeferredStartThatCanNoLongerRunStillEndsItsID(t *testing.T) {
	var reported []error
	ended := map[int64]ActionReason{}
	var runner *ActionRunner
	var deferredID int64
	next := &runnerTestAction{label: "next"}
	runner = newReentrancyRunner(t, ActionRunnerHooks{
		OnEnded: func(s ActionSnapshot, reason ActionReason) {
			ended[s.ID] = reason
			if deferredID == 0 {
				deferredID, _ = runner.Start(1, next, 0, time.Now())
				runner.Freeze(1)
			}
		},
		OnError: func(err error) { reported = append(reported, err) },
	})
	if _, err := runner.Start(1, &runnerTestAction{done: true}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatalf("Tick = %v", err)
	}
	if deferredID == 0 {
		t.Fatal("hook never got an id")
	}
	reason, ok := ended[deferredID]
	if !ok || !errors.Is(reason.Err, ErrActionGroupFrozen) || reason.Result.Status != ActionStatusFailed {
		t.Fatalf("deferred start into a frozen group ended=%v reason=%+v, want one failed OnEnded with ErrActionGroupFrozen", ok, reason)
	}
	if next.starts != 0 || runner.Current(1) != nil {
		t.Fatalf("frozen group ran the deferred action: starts=%d current=%v", next.starts, runner.Current(1))
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrActionGroupFrozen) {
		t.Fatalf("reported = %v", reported)
	}
}

// 回调里显式 Start 仍先于队列：结束钩子里发起的动作装上之后，排队项不会被先启动再被替换。
func TestHookStartStillPrecedesQueuedActions(t *testing.T) {
	var runner *ActionRunner
	hookStarted := &runnerTestAction{label: "hook"}
	fired := false
	runner = newReentrancyRunner(t, ActionRunnerHooks{OnEnded: func(ActionSnapshot, ActionReason) {
		if !fired {
			fired = true
			_, _ = runner.Start(1, hookStarted, 0, time.Now())
		}
	}})
	if _, err := runner.Start(1, &runnerTestAction{done: true}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	queued := &runnerTestAction{label: "queued"}
	if _, err := runner.Enqueue(1, queued, 0); err != nil {
		t.Fatal(err)
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if runner.Current(1) != hookStarted || queued.starts != 0 || queued.cancels != 0 || runner.QueueLength(1) != 1 {
		t.Fatalf("current=%v queued starts=%d cancels=%d queue=%d", runner.Current(1), queued.starts, queued.cancels, runner.QueueLength(1))
	}
}

// EndAll 先结束调用时存在的全部动作，回调里发起的变更随后按序执行：结束钩子里推进的下一步
// 在 EndAll 返回时仍在运行，不与 EndAll 的结束过程交错。
func TestEndAllEndsEverythingBeforeCallbackMutationsRun(t *testing.T) {
	var runner *ActionRunner
	var order []string
	followUp := &runnerTestAction{label: "follow-up"}
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:     newActionOnlyRegistry(t),
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Hooks: ActionRunnerHooks{
			OnEnded: func(s ActionSnapshot, _ ActionReason) {
				order = append(order, fmt.Sprintf("ended:%d", s.Group))
				if s.Group == 1 && s.Action != followUp {
					_, _ = runner.Start(1, followUp, 0, time.Now())
				}
			},
			OnTransition: func(s ActionSnapshot, entering bool) {
				if entering && s.Action == followUp {
					order = append(order, "start:follow-up")
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, &runnerTestAction{}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(2, &runnerTestAction{}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runner.EndAll(true, NewActionReason("end all")); err != nil {
		t.Fatal(err)
	}
	want := []string{"ended:1", "ended:2", "start:follow-up"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if runner.Current(1) != followUp {
		t.Fatalf("current(1) = %v, want the follow-up issued by the hook", runner.Current(1))
	}
}

// 结束钩子每次都发起一个启动必然失败的动作：之前是无界递归，现在由延后执行预算截停，外层
// 拿到 ErrDeferredRunaway，被截停的、已交出 ID 的动作各收到一次 OnEnded，runner 之后照常可用。
func TestCallbacksThatKeepTriggeringEachOtherAreBounded(t *testing.T) {
	var runner *ActionRunner
	ended := map[int64]int{}
	issued := map[int64]bool{}
	starts := 0
	var reported []error
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:         newActionOnlyRegistry(t),
		GroupForKind:     func(ActionKind) (ActionGroup, bool) { return 1, true },
		MaxDeferredSteps: 50,
		Hooks: ActionRunnerHooks{
			OnEnded: func(s ActionSnapshot, _ ActionReason) {
				ended[s.ID]++
				if id, err := runner.Start(1, &countingFailAction{starts: &starts}, 0, time.Now()); err == nil {
					issued[id] = true
				}
			},
			OnError: func(err error) { reported = append(reported, err) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, &runnerTestAction{done: true}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	err = runner.Tick(1, time.Now())
	if !errors.Is(err, ErrDeferredRunaway) {
		t.Fatalf("Tick = %v, want ErrDeferredRunaway", err)
	}
	if starts == 0 || starts > 50 {
		t.Fatalf("failing action started %d time(s), want bounded by the step budget", starts)
	}
	for id := range issued {
		if ended[id] != 1 {
			t.Fatalf("issued action %d ended %d time(s), want exactly 1", id, ended[id])
		}
	}
	if runner.Deferring() {
		t.Fatal("runner still deferring after the runaway was cut")
	}
	usable := &runnerTestAction{label: "after"}
	if _, err := runner.Start(1, usable, 0, time.Now()); err != nil || runner.Current(1) != usable {
		t.Fatalf("runner unusable after runaway: err=%v current=%v", err, runner.Current(1))
	}
}

// 一次回调里发起的命令数有上限：超出的 Start 立即被拒（不分配 ID），已接受的照常执行。
func TestDeferredQueueIsBounded(t *testing.T) {
	var runner *ActionRunner
	var accepted []int64
	var refused error
	fired := false
	runner, err := NewActionRunner(ActionRunnerConfig{
		Registry:            newActionOnlyRegistry(t),
		GroupForKind:        func(ActionKind) (ActionGroup, bool) { return 1, true },
		MaxDeferredCommands: 3,
		Hooks: ActionRunnerHooks{OnTransition: func(_ ActionSnapshot, entering bool) {
			if !entering || fired {
				return
			}
			fired = true
			for i := 0; i < 4; i++ {
				id, err := runner.Enqueue(1, &runnerTestAction{}, 0)
				if err != nil {
					refused = err
					if id != 0 {
						t.Errorf("refused Enqueue returned id %d", id)
					}
					continue
				}
				accepted = append(accepted, id)
			}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, &runnerTestAction{}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 3 || !errors.Is(refused, ErrDeferredQueueFull) {
		t.Fatalf("accepted=%v refused=%v, want 3 accepted and ErrDeferredQueueFull", accepted, refused)
	}
	if runner.QueueLength(1) != 3 {
		t.Fatalf("queue length = %d, want the 3 accepted enqueues", runner.QueueLength(1))
	}
}

type countingFailAction struct {
	runnerTestAction
	starts *int
}

func (a *countingFailAction) Start(*ActionContext) error {
	*a.starts++
	return errStartRefused
}

func newActionOnlyRegistry(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry()
	for kind := ActionKind(1); kind <= 2; kind++ {
		if err := registry.RegisterAction(kind, func(param any) (Action, error) {
			action, ok := param.(Action)
			if !ok {
				return nil, errors.New("invalid action")
			}
			return action, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

// planActionList 是 ActionList 的最小接线：只实现 PlanMission 用到的 CreateAction。
type planActionList struct {
	ActionList
	runner   *ActionRunner
	missions *MissionRunner
	deferred []bool
}

func (l *planActionList) CreateAction(kind ActionKind, param any) (int64, error) {
	l.deferred = append(l.deferred, l.runner.Deferring())
	return l.runner.Start(kind, param, l.missions.CurrentMissionID(), time.Time{})
}

// 接线兼容：OnEnded → MissionRunner.OnActionEnd → PlanMission 推进下一步 → CreateAction。
// 第二步的 Start 发生在 OnEnded 回调里，被延后，但返回的 ID 就是之后真正启动、结束的那个
// 动作的 ID，任务按 ID 匹配照常推进到成功；ClearActions → ClearMission 同样在回调里延后。
func TestPlanMissionAdvancesThroughADeferredStart(t *testing.T) {
	first := &runnerTestAction{label: "first", done: true}
	second := &runnerTestAction{label: "second", done: true}
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
	var ended []ActionReason
	list.missions, err = NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
		Context:      func(now time.Time) *MissionContext { return &MissionContext{ActionList: list, Now: now} },
		ClearActions: func(id int64, reason ActionReason) error { return list.runner.ClearMission(id, true, reason) },
		OnEnded:      func(_ Mission, reason ActionReason) { ended = append(ended, reason) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: first}, {Action: 2, Param: second}}}
	if err := list.missions.StartMission(1, plan); err != nil {
		t.Fatal(err)
	}
	if err := list.runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := list.deferred; len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("CreateAction deferring = %v, want [false true] (second step issued from OnEnded)", list.deferred)
	}
	if list.runner.Current(1) != second || second.starts != 1 {
		t.Fatalf("second step not running: current=%v starts=%d", list.runner.Current(1), second.starts)
	}
	if info := list.missions.MissionInfo(); info.CurrentStep != 1 || info.Status != MissionStatusRunning {
		t.Fatalf("mission info = %+v, want step 1 running", info)
	}
	if err := list.runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || list.missions.CurMission() != nil || list.missions.MissionInfo().Status != MissionStatusSuccess {
		t.Fatalf("mission did not finish: ended=%+v info=%+v", ended, list.missions.MissionInfo())
	}
}

// 动作 Start 失败后 runner 调它的 Cancel；Cancel 里发起的 Start 延后到失败处理完成后执行：
// 失败动作先完整收尾（离开一次、结束一次、Cancel 一次），再装上 next；外层只返回启动错误本身。
// next 是当前动作、只启动一次、能被 Tick 结束；排队路径不越过它启动下一项。
// 历史：RR-20261005-NC-122

var errStartRefused = errors.New("start refused")

// cancelReentersAction 的 Start 总是失败，Cancel 第一次被调用时重入启动 next。
type cancelReentersAction struct {
	runnerTestAction
	runner    *ActionRunner
	next      Action
	reentered bool
	innerErr  error
}

func (a *cancelReentersAction) Start(*ActionContext) error {
	a.starts++
	return errStartRefused
}

func (a *cancelReentersAction) Cancel(ctx *ActionContext, reason string) {
	a.runnerTestAction.Cancel(ctx, reason)
	if a.reentered {
		return
	}
	a.reentered = true
	_, a.innerErr = a.runner.Start(1, a.next, 0, time.Now())
}

type startFailureTrace struct {
	leaves map[int64]int
	ended  map[int64]int
}

func newStartFailureRunner(t *testing.T) (*ActionRunner, *startFailureTrace) {
	trace := &startFailureTrace{leaves: map[int64]int{}, ended: map[int64]int{}}
	runner := newReentrancyRunner(t, ActionRunnerHooks{
		OnTransition: func(s ActionSnapshot, entering bool) {
			if !entering {
				trace.leaves[s.ID]++
			}
		},
		OnEnded: func(s ActionSnapshot, _ ActionReason) { trace.ended[s.ID]++ },
	})
	return runner, trace
}

func TestStartFailureWhoseCancelStartsAnotherActionKeepsThatAction(t *testing.T) {
	runner, trace := newStartFailureRunner(t)
	next := &runnerTestAction{label: "next"}
	failing := &cancelReentersAction{runner: runner, next: next}
	_, err := runner.Start(1, failing, 0, time.Now())
	if failing.innerErr != nil {
		t.Fatalf("reentrant Start = %v", failing.innerErr)
	}
	if got := runner.Current(1); got != next {
		t.Fatalf("current = %v, want the action the reentrant Start installed (started %d time(s)); failing left %d time(s)",
			got, next.starts, trace.leaves[1])
	}
	if !errors.Is(err, errStartRefused) || errors.Is(err, ErrReentrantMutation) {
		t.Fatalf("Start = %v, want the start error alone", err)
	}
	if next.starts != 1 || next.cancels != 0 || failing.cancels != 1 {
		t.Fatalf("next starts=%d cancels=%d, failing cancels=%d", next.starts, next.cancels, failing.cancels)
	}
	// 失败的动作 ID 是 1：恰好离开一次、结束一次。
	if trace.leaves[1] != 1 || trace.ended[1] != 1 {
		t.Fatalf("failing action left %d time(s) and ended %d time(s), want 1 and 1", trace.leaves[1], trace.ended[1])
	}
	// 重入装上的动作没有被离开 / 结束，Tick 照常驱动它。
	if trace.leaves[2] != 0 || trace.ended[2] != 0 {
		t.Fatalf("installed action left=%d ended=%d", trace.leaves[2], trace.ended[2])
	}
	next.done = true
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if trace.ended[2] != 1 {
		t.Fatalf("installed action never completed through Tick: ended=%v", trace.ended)
	}
}

func TestQueuedStartFailureWhoseCancelStartsAnotherActionKeepsThatAction(t *testing.T) {
	runner, trace := newStartFailureRunner(t)
	next := &runnerTestAction{label: "next"}
	after := &runnerTestAction{label: "after"}
	failing := &cancelReentersAction{runner: runner, next: next}
	runner.Freeze(1)
	failingID, err := runner.Enqueue(1, failing, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Enqueue(1, after, 0); err != nil {
		t.Fatal(err)
	}
	err = runner.Recover(1, time.Now())
	if got := runner.Current(1); got != next {
		t.Fatalf("current = %v, want the reentrantly installed action (next starts=%d, after starts=%d, failing ended %d time(s))",
			got, next.starts, after.starts, trace.ended[failingID])
	}
	if !errors.Is(err, errStartRefused) || errors.Is(err, ErrReentrantMutation) {
		t.Fatalf("Recover = %v, want the start error alone", err)
	}
	if after.starts != 0 || runner.QueueLength(1) != 1 {
		t.Fatalf("the queue ran past the installed action: after starts=%d queue=%d", after.starts, runner.QueueLength(1))
	}
	if trace.ended[failingID] != 1 || trace.leaves[failingID] != 1 {
		t.Fatalf("failing queued action ended %d / left %d time(s), want 1 / 1", trace.ended[failingID], trace.leaves[failingID])
	}
}

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

// Start / Tick / Cancel / transition 回调里发起的 Start 在外层调用做完自己那一步后执行：外层
// 调用返回 nil；内层动作是当前动作、只启动一次；外层动作恰好启动一次、Cancel 一次、OnEnded 一次。
// 历史：U-0100
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
