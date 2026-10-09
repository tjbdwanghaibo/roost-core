package ai

import (
	"errors"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// RR-20261005-NC-241：策略回调（Tick / OnActionEnd / OnMissionEnd）里调用 SetStrategy 或
// Shutdown 时，之前切换立即执行：EndActions 结束现有动作、Stop 旧策略（BehaviorStrategy 在
// 这里 Reset 整棵树），然后控制权回到仍在执行的旧 Tick——树从被 Reset 的状态接着跑，后面的
// 叶子还能发起新动作。这些动作在 EndActions 之后才出现，没有被切换结束，结束通知送给了新
// 策略，旧树的叶子永远收不到，成为孤儿。现在回调里的切换延后到最外层回调返回后执行，
// 旧策略这一轮发起的动作都在 EndActions 之前。

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
