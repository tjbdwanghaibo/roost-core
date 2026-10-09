package ai

import (
	"errors"
	"testing"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// B7 兼容：actionflow.ActionRunner 把回调里的变更延后执行。TaskflowAction 的 Launch 若在
// runner 回调里被调用（这里：业务在 OnEnded 里立刻把结束交给 AI 并再 Tick 一次），Start
// 返回已分配的 ID 与 nil，叶子照常进入 Running 等这个 ID；延后的动作随后真正启动，结束时
// 同一个 ID 送达，树照常完成。叶子不需要知道自己是否被延后。

type doneAction struct{ starts int }

func (*doneAction) Kind() coreflow.ActionKind { return 1 }
func (a *doneAction) Start(*coreflow.ActionContext) error {
	a.starts++
	return nil
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
