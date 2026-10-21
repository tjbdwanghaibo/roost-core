package actionflow

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
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

// --- D21 第 4 期：任务心跳与计划步骤的时限 / 重试 ---

// newBeatAgentForTest 构造配了心跳的 Agent（驱动不启动：启停立即生效，拍由测试直接调回调），时钟可控。
func newBeatAgentForTest(t *testing.T, now *time.Time) (*Agent, *agentObserver, *heartbeat.Driver) {
	t.Helper()
	d := heartbeat.NewDriver(heartbeat.Config{Name: "agent-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	owner := &beatEntity{entity.NewEntityBase(3<<20, entity.EntityCategoryOther, true)}
	observer := &agentObserver{}
	agent, err := NewAgent(owner, AgentConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Beat:         &BeatConfig{Host: d.NewHost(owner)},
		Observer:     observer,
		Now:          func() time.Time { return *now },
		OnError:      func(err error) { t.Errorf("agent error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.agent = agent
	return agent, observer, d
}

// 任务心跳（A6）只在当前步有时限时挂（周期按计划的 Period 取档）；时限（A8）到了把这一步的动作以过期结束，还有重试
// 次数就原地再来一次（新的时限从重试时算），用完走 OnFail；任务结束后心跳停。
func TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	agent, observer, d := newBeatAgentForTest(t, &now)
	slow := &runnerTestAction{label: "never ends"}
	plan := MissionPlan{Period: 200 * time.Millisecond, Steps: []MissionStep{{Action: 1, Param: slow, Timeout: 300 * time.Millisecond, Retry: 1}}}
	if err := agent.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	reg := agent.MissionReg()
	if reg == nil || !reg.Active() || reg.Period() != 200*time.Millisecond || d.Registered() != 2 {
		t.Fatalf("mission with a timed step: reg %v registered %d (want the mission's and the action group's)", reg, d.Registered())
	}
	beat := func(at time.Duration) {
		now = time.Unix(1_800_000_000, 0).Add(at)
		if err := agent.missionBeat(heartbeat.Beat{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	beat(200 * time.Millisecond)
	if slow.cancels != 0 || len(observer.log) != 0 {
		t.Fatalf("before the timeout: cancels %d log %v", slow.cancels, observer.log)
	}
	beat(300 * time.Millisecond) // 第一次超时：过期结束，重试（新时限 600ms）
	beat(500 * time.Millisecond)
	if slow.cancels != 1 || slow.starts != 2 || agent.CurMission() == nil {
		t.Fatalf("first timeout: cancels %d starts %d mission %v", slow.cancels, slow.starts, agent.CurMission())
	}
	beat(600 * time.Millisecond) // 第二次超时：没有重试了，OnFail（缺省结束，任务状态随结果为过期）
	// 超时的结束发生在任务自己的 Tick 回调里，任务要等回调返回才收到，所以观察者先看到两次动作结束（任务仍在第 0 步），
	// 再看到任务结束（KNOWN-LIMITS“行为控制”记的例外）。
	want := []string{"action 1 5 step 0", "action 2 5 step 0", "mission 1 5"}
	if fmt.Sprint(observer.log) != fmt.Sprint(want) {
		t.Fatalf("observer saw %v, want %v", observer.log, want)
	}
	if reg.Active() || d.Registered() != 0 || agent.Missions().MissionInfo().LastResult.Reason != stepTimedOut {
		t.Fatalf("after the mission failed: active %v registered %d info %+v", reg.Active(), d.Registered(), agent.Missions().MissionInfo())
	}
}

// 没有时限的步不挂任务心跳；从有时限的步走到没有时限的步，心跳停。取消的步不重试。
func TestTheMissionHeartbeatFollowsTheCurrentStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	agent, observer, _ := newBeatAgentForTest(t, &now)
	first := &runnerTestAction{label: "timed", done: true}
	second := &runnerTestAction{label: "untimed"}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: first, Timeout: time.Second, Retry: 3}, {Action: 1, Param: second, Retry: 3}}}
	if err := agent.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	if reg := agent.MissionReg(); reg == nil || !reg.Active() || reg.Period() != DefaultMissionPeriod {
		t.Fatalf("timed first step: %v", reg)
	}
	if err := agent.Runner().Tick(1, now); err != nil { // 第一步完成，进入没有时限的第二步
		t.Fatal(err)
	}
	if agent.MissionReg().Active() || agent.CurAction(1) != second {
		t.Fatalf("untimed second step: mission beat active %v current %v", agent.MissionReg().Active(), agent.CurAction(1))
	}
	if err := agent.Runner().End(1, true, NewActionReason("someone else's move")); err != nil {
		t.Fatal(err)
	}
	if second.starts != 1 || agent.CurMission() != nil || observer.log[len(observer.log)-1] != "action 2 4 step -1" {
		t.Fatalf("a canceled step was retried: starts %d mission %v log %v", second.starts, agent.CurMission(), observer.log)
	}
	if _, err := NormalizePlan(MissionPlan{Steps: []MissionStep{{Action: 1, Timeout: -time.Second}}}); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
}

// flakyAction 第一次 Start 即失败、之后即成功（计划的重试测试用）。
type flakyAction struct{ starts int }

func (*flakyAction) Kind() ActionKind { return 1 }
func (a *flakyAction) Start(*ActionContext) (bool, ActionResult, error) {
	a.starts++
	if a.starts == 1 {
		return true, ActionResult{Status: ActionStatusFailed, Reason: "flaky"}, nil
	}
	return true, ActionResult{Status: ActionStatusSuccess}, nil
}
func (*flakyAction) Tick(*ActionContext) (bool, ActionResult) { return false, ActionResult{} }
func (*flakyAction) Cancel(*ActionContext, string)            {}

// 重试次数按步计：上一步用掉的重试不算到下一步。
func TestPlanRetriesAreCountedPerStep(t *testing.T) {
	agent, _, _ := newAgentForTest(t)
	first, second := &flakyAction{}, &flakyAction{}
	if err := agent.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: first, Retry: 1}, {Action: 1, Param: second, Retry: 1}}}); err != nil {
		t.Fatal(err)
	}
	if info := agent.Missions().MissionInfo(); info.Status != MissionStatusSuccess || first.starts != 2 || second.starts != 2 {
		t.Fatalf("mission %+v, starts %d / %d (want each step retried once, then success)", info, first.starts, second.starts)
	}
}
