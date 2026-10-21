package actionflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// Agent 的接线（D21 A1）：动作结束先送任务、再送观察者；任务回调里发起的动作归属任务、随任务结束；Clear 先结束
// 任务再结束动作；上下文带实体、ActionList 与当前任务。

// agentObserver 记录观察者收到的通知；seenStep 是收到动作结束那一刻任务所在的步（验证“先任务后观察者”）。
type agentObserver struct {
	agent *Agent
	log   []string
}

func (o *agentObserver) OnActionEnd(id int64, _ ActionKind, reason ActionReason) {
	step := -1
	if m := o.agent.CurMission(); m != nil {
		step = m.MissionInfo().CurrentStep
	}
	o.log = append(o.log, fmt.Sprintf("action %d %d step %d", id, reason.ToActionResult(ActionStatusSuccess).Status, step))
}

func (o *agentObserver) OnMissionEnd(m Mission, _ ActionReason) {
	o.log = append(o.log, fmt.Sprintf("mission %d %d", m.ID(), m.Status()))
}

// ctxAction 记下 Start 看到的上下文。
type ctxAction struct {
	runnerTestAction
	seen ActionContext
}

func (a *ctxAction) Start(ctx *ActionContext) (bool, ActionResult, error) {
	a.seen = *ctx
	return a.runnerTestAction.Start(ctx)
}

func newAgentForTest(t *testing.T) (*Agent, *agentObserver, *[]error) {
	t.Helper()
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	owner := &beatEntity{entity.NewEntityBase(1<<20, entity.EntityCategoryOther, true)}
	observer := &agentObserver{}
	var reported []error
	agent, err := NewAgent(owner, AgentConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Observer:     observer,
		OnError:      func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.agent = agent
	return agent, observer, &reported
}

func TestAgentDeliversActionEndsToTheMissionBeforeTheObserver(t *testing.T) {
	agent, observer, reported := newAgentForTest(t)
	first := &ctxAction{runnerTestAction: runnerTestAction{label: "first", done: true}}
	second := &runnerTestAction{label: "second", done: true}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: first}, {Action: 1, Param: second}}}
	if err := agent.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	if first.seen.Owner != agent.Entity() || first.seen.ActionList != agent || first.seen.CurrentMission == nil {
		t.Fatalf("action context: owner %v list %v mission %v", first.seen.Owner, first.seen.ActionList, first.seen.CurrentMission)
	}
	if err := agent.Runner().Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := agent.Runner().Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 第一步结束时观察者看到任务已经推进到第 1 步；第二步结束时任务在处理这次动作结束的过程中成功结束，所以观察者
	// 先收到任务结束、再收到引起它的动作结束（“先任务后观察者”的直接结果）。
	want := []string{"action 1 2 step 1", "mission 1 2", "action 2 2 step -1"}
	if fmt.Sprint(observer.log) != fmt.Sprint(want) {
		t.Fatalf("observer saw %v, want %v", observer.log, want)
	}
	if len(*reported) != 0 {
		t.Fatalf("reported %v", *reported)
	}
}

// 任务回调里发起的动作记在任务名下，任务结束时被取消；不在任务回调里发起的（AI、handler）不受影响。
func TestAgentEndsAMissionsActionsWithTheMissionOnly(t *testing.T) {
	agent, observer, _ := newAgentForTest(t)
	step := &runnerTestAction{label: "mission step"}
	if err := agent.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: step}}}); err != nil {
		t.Fatal(err)
	}
	own := &runnerTestAction{label: "ai action"}
	ownID, err := agent.CreateAction(2, own)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := agent.Runner().CurrentSnapshot(1)
	aiSnap, _ := agent.Runner().CurrentSnapshot(2)
	if snap.MissionID != agent.Missions().CurrentMissionID() || snap.MissionID == 0 || aiSnap.MissionID != 0 {
		t.Fatalf("mission tags: step %d (mission %d) ai %d", snap.MissionID, agent.Missions().CurrentMissionID(), aiSnap.MissionID)
	}
	agent.EndCurMission(NewActionReason("abort"))
	if agent.CurAction(1) != nil || step.cancels != 1 || agent.CurAction(2) != own || own.cancels != 0 {
		t.Fatalf("after the mission ended: step running %v cancels %d; ai action running %v cancels %d",
			agent.CurAction(1) != nil, step.cancels, agent.CurAction(2) == own, own.cancels)
	}
	// 任务的动作先被取消，观察者收到它之后才收到任务结束。
	want := []string{fmt.Sprintf("action %d 4 step -1", snap.ID), "mission 1 4"}
	if fmt.Sprint(observer.log) != fmt.Sprint(want) {
		t.Fatalf("observer saw %v, want %v", observer.log, want)
	}
	if err := agent.EndAction(ownID, true, NewActionReason("done")); err != nil || agent.CurAction(2) != nil {
		t.Fatalf("EndAction: %v current %v", err, agent.CurAction(2))
	}
}

// Clear 先结束任务再结束动作：任务的 OnFail 指向下一步时，下一步不会被启动。
func TestAgentClearLeavesNothingRunning(t *testing.T) {
	agent, _, _ := newAgentForTest(t)
	second := &runnerTestAction{label: "second"}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: &runnerTestAction{label: "first"}, OnFail: NextMissionStep(1)}, {Action: 1, Param: second}}}
	if err := agent.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	other := &runnerTestAction{label: "other"}
	if _, err := agent.CreateAction(2, other); err != nil {
		t.Fatal(err)
	}
	agent.Clear(NewActionReason("leave scene"))
	if agent.CurAction(1) != nil || agent.CurAction(2) != nil || second.starts != 0 || agent.CurMission() != nil {
		t.Fatalf("after Clear: group 1 %v group 2 %v second starts %d mission %v", agent.CurAction(1), agent.CurAction(2), second.starts, agent.CurMission())
	}
}

// 冻结经 ActionList 同样是计数的；Recover 用 Agent 的时钟推进队列。
func TestAgentFreezeAndRecoverThroughTheActionList(t *testing.T) {
	agent, _, _ := newAgentForTest(t)
	var list ActionList = agent
	if err := list.Freeze(1, FreezePause); err != nil {
		t.Fatal(err)
	}
	queued := &runnerTestAction{label: "queued"}
	if _, err := list.EnqueueAction(1, queued); err != nil {
		t.Fatal(err)
	}
	if !list.Frozen(1) || queued.starts != 0 {
		t.Fatalf("frozen %v starts %d", list.Frozen(1), queued.starts)
	}
	if err := list.Recover(1); err != nil || list.CurAction(1) != queued {
		t.Fatalf("Recover: %v current %v", err, list.CurAction(1))
	}
}

func TestNewAgentRefusesABeatHostOfAnotherEntity(t *testing.T) {
	_, d, other := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1})
	owner := &beatEntity{entity.NewEntityBase(2<<20, entity.EntityCategoryOther, true)}
	if _, err := NewAgent(owner, AgentConfig{Registry: NewRegistry(), GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Beat: &BeatConfig{Host: d.NewHost(other)}}); err == nil {
		t.Fatal("an agent accepted a heartbeat host of another entity")
	}
	agent, err := NewAgent(owner, AgentConfig{Registry: NewRegistry(), GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Beat: &BeatConfig{Host: d.NewHost(owner)}})
	if err != nil || agent.Beat() == nil || agent.Beat().Runner() != agent.Runner() {
		t.Fatalf("beat agent: %v", err)
	}
}
