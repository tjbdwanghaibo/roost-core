package actionflow

import (
	"testing"
	"time"
)

// RR-20261005-NC-121：排队的动作已经发过 OnQueued、ID 也交给了调用方（Enqueue 返回值，
// ai.TaskflowAction 用 EnqueueAction 发起时就等这个 ID 的结束），被 ClearQueue / EndAll /
// ClearMission 丢弃时必须发一次 OnEnded（取消状态），与“排队动作启动失败也发 OnEnded”一致。
// 之前三处都只是把队列切片清空，等待方永远等不到结束、Hook 侧的排队视图永远残留。
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
