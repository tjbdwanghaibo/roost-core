package ai

import (
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// RR-20261005-NC-120：Freeze 只暂停决策（Tick），不能吞掉冻结期间到达的动作 / 任务
// 结束通知。之前 OnActionEnd / OnMissionEnd 与 Tick 共用 ready()，冻结时直接丢弃：
// BehaviorStrategy 的 TaskflowAction 叶子等的那条 ActionEnd 永远不会再来，Recover 之后
// 整棵树一直停在 Running，再也不会重新决策。

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
