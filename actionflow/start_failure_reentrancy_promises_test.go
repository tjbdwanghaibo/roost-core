package actionflow

import (
	"errors"
	"testing"
	"time"
)

// RR-20261005-NC-122：动作 Start 失败后 runner 调它的 Cancel；Cancel 里重入启动了别的
// 动作时，外层必须像 finish 一样识别重入（ErrReentrantMutation），不能再把 unit.cur 清空。
// 之前启动失败分支无条件 `unit.cur = nil`：重入装上的新动作已经 Start、发过激活切换，
// 却从当前位上被抹掉——不再 Tick、不会被结束 / 取消，成了孤儿；失败的动作还会重复发
// “离开”切换，排队路径上还会重复 OnEnded 并越过孤儿去启动下一个排队项。

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
	if !errors.Is(err, errStartRefused) || !errors.Is(err, ErrReentrantMutation) {
		t.Fatalf("Start = %v, want the start error joined with ErrReentrantMutation", err)
	}
	if next.starts != 1 || next.cancels != 0 {
		t.Fatalf("next starts=%d cancels=%d", next.starts, next.cancels)
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
	if !errors.Is(err, errStartRefused) || !errors.Is(err, ErrReentrantMutation) {
		t.Fatalf("Recover = %v, want the start error joined with ErrReentrantMutation", err)
	}
	if after.starts != 0 || runner.QueueLength(1) != 1 {
		t.Fatalf("the queue ran past the installed action: after starts=%d queue=%d", after.starts, runner.QueueLength(1))
	}
	if trace.ended[failingID] != 1 || trace.leaves[failingID] != 1 {
		t.Fatalf("failing queued action ended %d / left %d time(s), want 1 / 1", trace.ended[failingID], trace.leaves[failingID])
	}
}
