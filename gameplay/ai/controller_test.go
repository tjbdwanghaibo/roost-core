package ai

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type controllerTestStrategy struct {
	name      string
	initErr   error
	stopCount int
	tickCount int
}

func (s *controllerTestStrategy) Name() string             { return s.name }
func (s *controllerTestStrategy) Init(*Context) error      { return s.initErr }
func (s *controllerTestStrategy) Tick(*Context, time.Time) { s.tickCount++ }
func (*controllerTestStrategy) OnActionEnd(*Context, int64, coreflow.ActionKind, coreflow.ActionReason) {
}
func (*controllerTestStrategy) OnMissionEnd(*Context, coreflow.Mission, coreflow.ActionReason) {
}
func (*controllerTestStrategy) CanStopByNext(Strategy) bool { return true }
func (s *controllerTestStrategy) Stop(*Context, string)     { s.stopCount++ }

func TestControllerFailedInitKeepsPreviousStrategy(t *testing.T) {
	endedActions := 0
	controller := NewController(ControllerHooks{EndActions: func(coreflow.ActionReason) { endedActions++ }})
	previous := &controllerTestStrategy{name: "previous"}
	if err := controller.SetStrategy(previous); err != nil {
		t.Fatal(err)
	}
	failed := &controllerTestStrategy{name: "failed", initErr: errors.New("init")}
	if err := controller.SetStrategy(failed); !errors.Is(err, ErrStrategyInit) {
		t.Fatalf("SetStrategy error = %v", err)
	}
	if controller.Strategy() != previous {
		t.Fatal("failed replacement must keep previous strategy")
	}
	if previous.stopCount != 0 || endedActions != 0 || failed.stopCount != 1 {
		t.Fatalf("unexpected rollback lifecycle: previous stops=%d ended=%d failed stops=%d", previous.stopCount, endedActions, failed.stopCount)
	}
}

func TestBehaviorTreeReadyDoesNotAdvanceSequence(t *testing.T) {
	calls := 0
	ready := &FuncNode[int]{Run: func(*int) Status { return StatusReady }}
	next := &FuncNode[int]{Run: func(*int) Status { calls++; return StatusSuccess }}
	tree := &Sequence[int]{Children: []Node[int]{ready, next}}
	if status := tree.Tick(nil); status != StatusRunning {
		t.Fatalf("status = %v, want running", status)
	}
	if calls != 0 {
		t.Fatal("sequence advanced past ready child")
	}
}

// Race-freedom is necessary but not sufficient: a Blackboard that dropped
// writes, or returned a Snapshot aliasing live state, would have passed the
// previous version of this test since it discarded every return value.
func TestBlackboardConcurrentAccessKeepsEveryWorkersWrite(t *testing.T) {
	board := NewBlackboard()
	const workers, rounds = 8, 100
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			key := fmt.Sprintf("worker_%d", id)
			for index := 0; index < rounds; index++ {
				board.Set(key, index)
				// Reading a neighbour's key concurrently is what the race
				// detector is here for.
				_, _ = board.Get(fmt.Sprintf("worker_%d", (id+1)%workers))
				_ = board.Snapshot()
			}
		}(worker)
	}
	wait.Wait()

	for worker := 0; worker < workers; worker++ {
		key := fmt.Sprintf("worker_%d", worker)
		value, ok := board.Get(key)
		if !ok {
			t.Fatalf("%s is missing after concurrent writes", key)
		}
		if value != rounds-1 {
			t.Fatalf("%s = %v, want the last write %d", key, value, rounds-1)
		}
	}

	// Snapshot must be a copy: mutating it must not reach the board.
	snapshot := board.Snapshot()
	snapshot["worker_0"] = "tampered"
	if value, _ := board.Get("worker_0"); value == "tampered" {
		t.Fatal("Snapshot aliases live state; it must return a copy")
	}
}

// B7 兼容：actionflow.ActionRunner 把回调里的变更延后执行。TaskflowAction 的 Launch 若在
// runner 回调里被调用（这里：业务在 OnEnded 里立刻把结束交给 AI 并再 Tick 一次），Start
// 返回已分配的 ID 与 nil，叶子照常进入 Running 等这个 ID；延后的动作随后真正启动，结束时
// 同一个 ID 送达，树照常完成。叶子不需要知道自己是否被延后。

type doneAction struct{ starts int }

func (*doneAction) Kind() coreflow.ActionKind { return 1 }
func (a *doneAction) Start(*coreflow.ActionContext) (bool, coreflow.ActionResult, error) {
	a.starts++
	return false, coreflow.ActionResult{}, nil
}
func (*doneAction) Tick(*coreflow.ActionContext) (bool, coreflow.ActionResult) {
	return true, coreflow.ActionResult{Status: coreflow.ActionStatusSuccess}
}
func (*doneAction) Cancel(*coreflow.ActionContext, string) {}

func TestTaskflowActionLaunchedInsideARunnerCallbackStillCompletes(t *testing.T) {
	registry := coreflow.NewRegistry()
	if err := registry.RegisterAction(1, func(param any) (coreflow.Action, error) {
		action, ok := param.(coreflow.Action)
		if !ok {
			return nil, errors.New("invalid action")
		}
		return action, nil
	}); err != nil {
		t.Fatal(err)
	}
	var runner *coreflow.ActionRunner
	var controller *Controller
	var launched []int64
	var launchedDeferred []bool
	var actions []*doneAction
	leaf := &TaskflowAction[treeData]{Launch: func(*treeCtx) (int64, error) {
		action := &doneAction{}
		actions = append(actions, action)
		launchedDeferred = append(launchedDeferred, runner.Deferring())
		id, err := runner.Start(1, action, 0, time.Time{})
		launched = append(launched, id)
		return id, err
	}}
	var results []Status
	strategy := NewBehaviorStrategy[treeData](leaf, BehaviorStrategyOptions[treeData]{
		Data: &treeData{}, OnResult: func(status Status) { results = append(results, status) },
	})
	controller = NewController(ControllerHooks{})
	if err := controller.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	var err error
	runner, err = coreflow.NewActionRunner(coreflow.ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(coreflow.ActionKind) (coreflow.ActionGroup, bool) { return 1, true },
		Hooks: coreflow.ActionRunnerHooks{OnEnded: func(s coreflow.ActionSnapshot, reason coreflow.ActionReason) {
			controller.OnActionEnd(s.ID, s.Kind, reason)
			if len(launched) < 2 {
				controller.Tick(time.Unix(1, 0)) // 消费结束 → 树 Success 并重置
				controller.Tick(time.Unix(2, 0)) // 重新决策 → 在回调里 Launch
			}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.Tick(time.Unix(0, 0)) // 回调外 Launch 第一个动作
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(launched) != 2 || launchedDeferred[0] || !launchedDeferred[1] || launched[1] == 0 {
		t.Fatalf("launched=%v deferred=%v, want a second launch issued inside the callback with an id", launched, launchedDeferred)
	}
	if actions[1].starts != 1 {
		t.Fatalf("deferred launch started %d time(s)", actions[1].starts)
	}
	if snapshot, ok := runner.CurrentSnapshot(1); !ok || snapshot.ID != launched[1] {
		t.Fatalf("running action = %+v, want id %d", snapshot, launched[1])
	}
	if err := runner.Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	controller.Tick(time.Unix(3, 0))
	if len(results) != 2 || results[0] != StatusSuccess || results[1] != StatusSuccess {
		t.Fatalf("tree results = %v, want two successes (the deferred launch's completion reached the leaf)", results)
	}
}

// Freeze 只暂停决策（Tick），不吞掉冻结期间到达的动作 / 任务结束通知：TaskflowAction 叶子
// 等的那条 ActionEnd 在 Recover 之后照常送达，树能继续决策。
// 历史：RR-20261005-NC-120

// endCountingStrategy 记录送达的结束通知，用来确认任务结束也照常送达。
type endCountingStrategy struct {
	controllerTestStrategy
	actionEnds  []int64
	missionEnds int
}

func (s *endCountingStrategy) OnActionEnd(_ *Context, id int64, _ coreflow.ActionKind, _ coreflow.ActionReason) {
	s.actionEnds = append(s.actionEnds, id)
}
func (s *endCountingStrategy) OnMissionEnd(*Context, coreflow.Mission, coreflow.ActionReason) {
	s.missionEnds++
}

func TestAFrozenControllerStillDeliversTheCompletionATreeIsWaitingFor(t *testing.T) {
	data := &treeData{}
	leaf := &TaskflowAction[treeData]{Launch: func(ctx *treeCtx) (int64, error) {
		ctx.Data.launched++
		return ctx.Data.launched, nil
	}}
	var results []Status
	strategy := NewBehaviorStrategy[treeData](leaf, BehaviorStrategyOptions[treeData]{
		Data: data, OnResult: func(status Status) { results = append(results, status) },
	})
	controller := NewController(ControllerHooks{})
	if err := controller.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	controller.Tick(time.Unix(1, 0)) // 发起动作 1，树 Running
	if data.launched != 1 {
		t.Fatalf("launched = %d, want 1", data.launched)
	}

	controller.Freeze()
	controller.OnActionEnd(1, 1, coreflow.NewActionReason("done"))
	controller.Tick(time.Unix(2, 0)) // 冻结中：不决策
	if data.launched != 1 || len(results) != 0 {
		t.Fatalf("a frozen controller made a decision: launched=%d results=%v", data.launched, results)
	}

	controller.Recover()
	for second := int64(3); second < 8; second++ {
		controller.Tick(time.Unix(second, 0))
	}
	if len(results) == 0 || results[0] != StatusSuccess {
		t.Fatalf("after Recover the tree never saw the completion that arrived while frozen: launched=%d results=%v",
			data.launched, results)
	}
	if data.launched < 2 {
		t.Fatalf("the tree did not re-evaluate after the completed action: launched=%d", data.launched)
	}
}

func TestAFrozenControllerStillDeliversMissionEndsButNotTicks(t *testing.T) {
	strategy := &endCountingStrategy{controllerTestStrategy: controllerTestStrategy{name: "counting"}}
	controller := NewController(ControllerHooks{})
	if err := controller.SetStrategy(strategy); err != nil {
		t.Fatal(err)
	}
	controller.Freeze()
	controller.Tick(time.Unix(1, 0))
	controller.OnActionEnd(7, 1, coreflow.NewActionReason("done"))
	controller.OnMissionEnd(nil, coreflow.NewActionReason("done"))
	if strategy.tickCount != 0 {
		t.Fatalf("frozen controller ticked the strategy %d time(s)", strategy.tickCount)
	}
	if len(strategy.actionEnds) != 1 || strategy.actionEnds[0] != 7 || strategy.missionEnds != 1 {
		t.Fatalf("completions while frozen: action ends=%v mission ends=%d, want [7] and 1",
			strategy.actionEnds, strategy.missionEnds)
	}
}

// Parallel 的结果一旦确定（RequireAll 有一个子节点失败、RequireOne 有一个成功），同一次 Tick
// 里排在后面的子节点不再被 Tick，不会发起没人要的动作；仍在运行的子节点照常被打断。
// 历史：RR-20261005-NC-240

// countingLeaf 是一个发起动作的叶子，记录发起与打断次数。
func countingLeaf(launched, interrupted *int) Node[treeCtx] {
	return &TaskflowAction[treeData]{
		Launch: func(*treeCtx) (int64, error) {
			*launched++
			return int64(*launched), nil
		},
		OnInterrupt: func(int64) { *interrupted++ },
	}
}

func TestParallelStopsTickingChildrenOnceTheOutcomeIsDecided(t *testing.T) {
	cases := []struct {
		name   string
		policy ParallelPolicy
		first  bool // 第一个子节点（条件）的结果
		want   Status
	}{
		{name: "require_all_first_child_fails", policy: ParallelRequireAll, first: false, want: StatusFailure},
		{name: "require_one_first_child_succeeds", policy: ParallelRequireOne, first: true, want: StatusSuccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launched, interrupted := 0, 0
			first := tc.first
			parallel := &Parallel[treeCtx]{Policy: tc.policy, Children: []Node[treeCtx]{
				&Condition[treeCtx]{Check: func(*treeCtx) bool { return first }},
				countingLeaf(&launched, &interrupted),
			}}
			if got := parallel.Tick(&treeCtx{Data: &treeData{}}); got != tc.want {
				t.Fatalf("status = %v, want %v", got, tc.want)
			}
			if launched != 0 || interrupted != 0 {
				t.Fatalf("a child after the deciding one was ticked: launched=%d interrupted=%d, want 0/0", launched, interrupted)
			}
		})
	}
}

// 控制：结果未定时照常 Tick 全部未完成的子节点；之后结果确定时，已在运行的子节点经 Reset
// 收到打断。
func TestParallelStillInterruptsRunningChildrenWhenItCompletes(t *testing.T) {
	launched, interrupted := 0, 0
	fail := false
	parallel := &Parallel[treeCtx]{Policy: ParallelRequireAll, Children: []Node[treeCtx]{
		&FuncNode[treeCtx]{Run: func(*treeCtx) Status {
			if fail {
				return StatusFailure
			}
			return StatusRunning
		}},
		countingLeaf(&launched, &interrupted),
	}}
	ctx := &treeCtx{Data: &treeData{}}
	if got := parallel.Tick(ctx); got != StatusRunning || launched != 1 {
		t.Fatalf("first tick status=%v launched=%d, want running/1", got, launched)
	}
	fail = true
	if got := parallel.Tick(ctx); got != StatusFailure || interrupted != 1 || launched != 1 {
		t.Fatalf("second tick status=%v launched=%d interrupted=%d, want failure/1/1", got, launched, interrupted)
	}
}

// 策略回调（Tick / OnActionEnd / OnMissionEnd）里调用 SetStrategy 或 Shutdown 时，切换延后到
// 最外层回调返回后执行：旧策略这一轮发起的动作都在 EndActions 之前，不会成为切换后无人等待的孤儿。
// 历史：RR-20261005-NC-241

// launchLog 记录事件顺序：发起动作、EndActions、Stop。
type launchLog struct{ events []string }

func (l *launchLog) add(event string) { l.events = append(l.events, event) }

func switchingTree(log *launchLog, request func()) Node[treeCtx] {
	return &Sequence[treeCtx]{Children: []Node[treeCtx]{
		&FuncNode[treeCtx]{Run: func(*treeCtx) Status {
			request()
			return StatusSuccess
		}},
		&TaskflowAction[treeData]{Launch: func(*treeCtx) (int64, error) {
			log.add("launch")
			return 7, nil
		}},
	}}
}

type stopRecordingStrategy struct {
	*BehaviorStrategy[treeData]
	log *launchLog
}

func (s stopRecordingStrategy) Stop(ctx *Context, reason string) {
	s.log.add("stop")
	s.BehaviorStrategy.Stop(ctx, reason)
}

func TestSwitchRequestedInsideATickRunsAfterTheTickReturns(t *testing.T) {
	cases := []struct {
		name    string
		request func(c *Controller, next Strategy) error
	}{
		{name: "set_strategy", request: func(c *Controller, next Strategy) error { return c.SetStrategy(next) }},
		{name: "shutdown", request: func(c *Controller, _ Strategy) error { c.Shutdown("test"); return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &launchLog{}
			var controller *Controller
			next := &controllerTestStrategy{name: "next"}
			var requestErr error
			root := switchingTree(log, func() { requestErr = tc.request(controller, next) })
			old := stopRecordingStrategy{NewBehaviorStrategy[treeData](root, BehaviorStrategyOptions[treeData]{Data: &treeData{}}), log}
			controller = NewController(ControllerHooks{EndActions: func(coreflow.ActionReason) { log.add("end_actions") }})
			if err := controller.SetStrategy(old); err != nil {
				t.Fatal(err)
			}
			log.events = nil
			controller.Tick(time.Unix(1, 0))
			if requestErr != nil {
				t.Fatalf("switch inside the tick = %v, want nil (deferred)", requestErr)
			}
			// 旧策略这一轮发起的动作必须在 Stop 之前，并且（有 EndActions 的切换）被它覆盖。
			want := []string{"launch", "end_actions", "stop"}
			if tc.name == "shutdown" {
				want = []string{"launch", "stop"}
			}
			if !equalEvents(log.events, want) {
				t.Fatalf("events = %v, want %v: the replaced tree kept launching after it was stopped", log.events, want)
			}
			wantCurrent := Strategy(next)
			if tc.name == "shutdown" {
				wantCurrent = nil
			}
			if controller.Strategy() != wantCurrent {
				t.Fatalf("current strategy = %v, want %v", strategyName(controller.Strategy()), strategyName(wantCurrent))
			}
		})
	}
}

// 回调里的切换出错（新策略 Init 失败）时发起方早已拿到 nil，错误经 OnError 报告，旧策略保留。
func TestDeferredSwitchFailureIsReportedAndKeepsTheCurrentStrategy(t *testing.T) {
	var reported []error
	var controller *Controller
	failing := &controllerTestStrategy{name: "failing", initErr: errors.New("boom")}
	current := &tickHookStrategy{controllerTestStrategy: controllerTestStrategy{name: "current"}}
	current.onTick = func() {
		if err := controller.SetStrategy(failing); err != nil {
			t.Errorf("SetStrategy inside Tick = %v, want nil", err)
		}
		if controller.Strategy() != current {
			t.Error("the switch took effect inside the tick")
		}
	}
	controller = NewController(ControllerHooks{OnError: func(err error) { reported = append(reported, err) }})
	if err := controller.SetStrategy(current); err != nil {
		t.Fatal(err)
	}
	controller.Tick(time.Unix(1, 0))
	if controller.Strategy() != current || current.stopCount != 0 {
		t.Fatalf("failed deferred switch disturbed the current strategy: current=%s stops=%d",
			strategyName(controller.Strategy()), current.stopCount)
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrStrategyInit) {
		t.Fatalf("reported = %v, want one ErrStrategyInit", reported)
	}
	// 回调之外照旧同步返回错误。
	if err := controller.SetStrategy(failing); !errors.Is(err, ErrStrategyInit) {
		t.Fatalf("SetStrategy outside callbacks = %v, want ErrStrategyInit", err)
	}
}

type tickHookStrategy struct {
	controllerTestStrategy
	onTick func()
}

func (s *tickHookStrategy) Tick(ctx *Context, now time.Time) {
	s.controllerTestStrategy.Tick(ctx, now)
	if s.onTick != nil {
		s.onTick()
	}
}

func equalEvents(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// D22-1：没有 Now 钩子时，AI 的缺省时间是本次执行冻结的业务时间（fctx.Now）。
func TestControllerDefaultNowIsTheExecutionsFrozenBusinessTime(t *testing.T) {
	var mu sync.Mutex
	tick := time.UnixMilli(1_800_000_000_000)
	clock.SetSource(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Millisecond)
		return tick
	})
	t.Cleanup(func() { clock.SetSource(nil) })
	execution, release := fctx.NewContext()
	defer release()
	if got := NewController(ControllerHooks{}).now(); !got.Equal(execution.Now) {
		t.Fatalf("AI default now = %v, want the execution's frozen business time %v", got, execution.Now)
	}
}
