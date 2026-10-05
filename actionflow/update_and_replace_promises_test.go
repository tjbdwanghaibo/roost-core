package actionflow

import (
	"strings"
	"testing"
	"time"
)

// RR-20261005-NC-242：Update 不在回调里时把 fn 的运行期间也算作回调（executing 置位，fn 里的
// 变更延后到 fn 返回后执行，B7）。之前 fn panic 时 executing 永远不会复位：之后所有变更
// 都被当成“回调里”的延后命令，再也没有人执行——Start 返回 ID 却从不启动，排满
// MaxDeferredCommands 后一律 ErrDeferredQueueFull，runner 报废。B7 之前 Update 直接调 fn，
// panic 只是向上传播，不留状态。现在 fn 的 panic 与动作回调一样被恢复成错误返回。
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

// RR-20261005-NC-243（N10 观察 O-A4）：Start 替换当前动作时，旧动作的 Cancel panic 之前让
// applyStart 整体返回——旧动作已经结束（当前位已清、OnEnded 已发），新动作却没有启动，也
// 没有 OnEnded。回调里发起的 Start 早已把 ID 和 nil 交给调用方（B7 承诺“拿到 ID 的动作
// 一定有结论”），按 ID 等结束的 ai.TaskflowAction / PlanMission 永远等不到；直接调用时组
// 空着，排队项也没有推进。旧动作的 Cancel 失败是旧动作自己的问题：它照样被替换，新动作
// 照常启动，错误经 OnError 报告。
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
