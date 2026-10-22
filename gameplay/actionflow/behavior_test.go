package actionflow

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Behavior 的接线：动作结束先送任务、再送观察者；任务回调里发起的动作归属任务、随任务结束；Clear 先结束
// 任务再结束动作；上下文带实体、ActionList 与当前任务。

// behaviorObserver 记录观察者收到的通知；seenStep 是收到动作结束那一刻任务所在的步（验证“先任务后观察者”）。
type behaviorObserver struct {
	behavior *Behavior
	log      []string
	// reasons 非 nil 时按动作 ID 记下结束原因。
	reasons map[int64]ActionReason
}

func (o *behaviorObserver) OnActionEnd(id int64, _ ActionKind, reason ActionReason) {
	step := -1
	if m := o.behavior.CurMission(); m != nil {
		step = m.MissionInfo().CurrentStep
	}
	o.log = append(o.log, fmt.Sprintf("action %d %d step %d", id, reason.ToActionResult(ActionStatusSuccess).Status, step))
	if o.reasons != nil {
		o.reasons[id] = reason
	}
}

func (o *behaviorObserver) OnMissionEnd(m Mission, _ ActionReason) {
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

func newBehaviorForTest(t *testing.T) (*Behavior, *behaviorObserver, *[]error) {
	t.Helper()
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	owner := &beatEntity{entity.NewEntityBase(1<<20, entity.EntityCategoryOther, true)}
	observer := &behaviorObserver{}
	var reported []error
	behavior, err := NewBehavior(owner, BehaviorConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Observer:     observer,
		OnError:      func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.behavior = behavior
	return behavior, observer, &reported
}

func TestBehaviorDeliversActionEndsToTheMissionBeforeTheObserver(t *testing.T) {
	behavior, observer, reported := newBehaviorForTest(t)
	first := &ctxAction{runnerTestAction: runnerTestAction{label: "first", done: true}}
	second := &runnerTestAction{label: "second", done: true}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: first}, {Action: 1, Param: second}}}
	if err := behavior.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	if first.seen.Owner != behavior.Entity() || first.seen.ActionList != behavior || first.seen.CurrentMission == nil {
		t.Fatalf("action context: owner %v list %v mission %v", first.seen.Owner, first.seen.ActionList, first.seen.CurrentMission)
	}
	if err := behavior.Runner().Tick(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := behavior.Runner().Tick(1, time.Now()); err != nil {
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
func TestBehaviorEndsAMissionsActionsWithTheMissionOnly(t *testing.T) {
	behavior, observer, _ := newBehaviorForTest(t)
	step := &runnerTestAction{label: "mission step"}
	if err := behavior.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: step}}}); err != nil {
		t.Fatal(err)
	}
	own := &runnerTestAction{label: "ai action"}
	ownID, err := behavior.CreateAction(2, own)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := behavior.Runner().CurrentSnapshot(1)
	aiSnap, _ := behavior.Runner().CurrentSnapshot(2)
	if snap.MissionID != behavior.Missions().CurrentMissionID() || snap.MissionID == 0 || aiSnap.MissionID != 0 {
		t.Fatalf("mission tags: step %d (mission %d) ai %d", snap.MissionID, behavior.Missions().CurrentMissionID(), aiSnap.MissionID)
	}
	behavior.EndCurMission(NewActionReason("abort"))
	if behavior.CurAction(1) != nil || step.cancels != 1 || behavior.CurAction(2) != own || own.cancels != 0 {
		t.Fatalf("after the mission ended: step running %v cancels %d; ai action running %v cancels %d",
			behavior.CurAction(1) != nil, step.cancels, behavior.CurAction(2) == own, own.cancels)
	}
	// 任务的动作先被取消，观察者收到它之后才收到任务结束。
	want := []string{fmt.Sprintf("action %d 4 step -1", snap.ID), "mission 1 4"}
	if fmt.Sprint(observer.log) != fmt.Sprint(want) {
		t.Fatalf("observer saw %v, want %v", observer.log, want)
	}
	if err := behavior.EndAction(ownID, true, NewActionReason("done")); err != nil || behavior.CurAction(2) != nil {
		t.Fatalf("EndAction: %v current %v", err, behavior.CurAction(2))
	}
}

// Clear 先结束任务再结束动作：任务的 OnFail 指向下一步时，下一步不会被启动。
func TestBehaviorClearLeavesNothingRunning(t *testing.T) {
	behavior, _, _ := newBehaviorForTest(t)
	second := &runnerTestAction{label: "second"}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: &runnerTestAction{label: "first"}, OnFail: NextMissionStep(1)}, {Action: 1, Param: second}}}
	if err := behavior.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	other := &runnerTestAction{label: "other"}
	if _, err := behavior.CreateAction(2, other); err != nil {
		t.Fatal(err)
	}
	behavior.Clear(NewActionReason("leave scene"))
	if behavior.CurAction(1) != nil || behavior.CurAction(2) != nil || second.starts != 0 || behavior.CurMission() != nil {
		t.Fatalf("after Clear: group 1 %v group 2 %v second starts %d mission %v", behavior.CurAction(1), behavior.CurAction(2), second.starts, behavior.CurMission())
	}
}

// 冻结经 ActionList 同样是计数的；Recover 用 Behavior 的时钟推进队列。
func TestBehaviorFreezeAndRecoverThroughTheActionList(t *testing.T) {
	behavior, _, _ := newBehaviorForTest(t)
	var list ActionList = behavior
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

func TestNewBehaviorRefusesABeatHostOfAnotherEntity(t *testing.T) {
	_, d, other := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1})
	owner := &beatEntity{entity.NewEntityBase(2<<20, entity.EntityCategoryOther, true)}
	if _, err := NewBehavior(owner, BehaviorConfig{Registry: NewRegistry(), GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Beat: &BeatConfig{Host: d.NewHost(other)}}); err == nil {
		t.Fatal("an behavior accepted a heartbeat host of another entity")
	}
	behavior, err := NewBehavior(owner, BehaviorConfig{Registry: NewRegistry(), GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true },
		Beat: &BeatConfig{Host: d.NewHost(owner)}})
	if err != nil || behavior.Beat() == nil || behavior.Beat().Runner() != behavior.Runner() {
		t.Fatalf("beat behavior: %v", err)
	}
}

// --- 任务心跳与计划步骤的时限 / 重试 ---

// newBeatBehaviorForTest 构造配了心跳的 Behavior（驱动不启动：启停立即生效，拍由测试直接调回调），时钟可控。
func newBeatBehaviorForTest(t *testing.T, now *time.Time) (*Behavior, *behaviorObserver, *heartbeat.Driver) {
	t.Helper()
	d := heartbeat.NewDriver(heartbeat.Config{Name: "behavior-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	owner := &beatEntity{entity.NewEntityBase(3<<20, entity.EntityCategoryOther, true)}
	observer := &behaviorObserver{}
	behavior, err := NewBehavior(owner, BehaviorConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Beat:         &BeatConfig{Host: d.NewHost(owner)},
		Observer:     observer,
		Now:          func() time.Time { return *now },
		OnError:      func(err error) { t.Errorf("behavior error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.behavior = behavior
	return behavior, observer, d
}

// 任务心跳（A6）只在当前步有时限时挂（周期按计划的 Period 取档）；时限（A8）到了把这一步的动作以过期结束，还有重试
// 次数就原地再来一次（新的时限从重试时算），用完走 OnFail；任务结束后心跳停。
func TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	behavior, observer, d := newBeatBehaviorForTest(t, &now)
	slow := &runnerTestAction{label: "never ends"}
	plan := MissionPlan{Period: 200 * time.Millisecond, Steps: []MissionStep{{Action: 1, Param: slow, Timeout: 300 * time.Millisecond, Retry: 1}}}
	if err := behavior.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	reg := behavior.MissionReg()
	if reg == nil || !reg.Active() || reg.Period() != 200*time.Millisecond || d.Registered() != 2 {
		t.Fatalf("mission with a timed step: reg %v registered %d (want the mission's and the action group's)", reg, d.Registered())
	}
	beat := func(at time.Duration) {
		now = time.Unix(1_800_000_000, 0).Add(at)
		if err := behavior.missionBeat(heartbeat.Beat{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	beat(200 * time.Millisecond)
	if slow.cancels != 0 || len(observer.log) != 0 {
		t.Fatalf("before the timeout: cancels %d log %v", slow.cancels, observer.log)
	}
	beat(300 * time.Millisecond) // 第一次超时：过期结束，重试（新时限 600ms）
	beat(500 * time.Millisecond)
	if slow.cancels != 1 || slow.starts != 2 || behavior.CurMission() == nil {
		t.Fatalf("first timeout: cancels %d starts %d mission %v", slow.cancels, slow.starts, behavior.CurMission())
	}
	beat(600 * time.Millisecond) // 第二次超时：没有重试了，OnFail（缺省结束，任务状态随结果为过期）
	// 超时的结束发生在任务自己的 Tick 回调里，任务要等回调返回才收到，所以观察者先看到两次动作结束（任务仍在第 0 步），
	// 再看到任务结束（KNOWN-LIMITS“行为控制”记的例外）。
	want := []string{"action 1 5 step 0", "action 2 5 step 0", "mission 1 5"}
	if fmt.Sprint(observer.log) != fmt.Sprint(want) {
		t.Fatalf("observer saw %v, want %v", observer.log, want)
	}
	if reg.Active() || d.Registered() != 0 || behavior.Missions().MissionInfo().LastResult.Reason != stepTimedOut {
		t.Fatalf("after the mission failed: active %v registered %d info %+v", reg.Active(), d.Registered(), behavior.Missions().MissionInfo())
	}
}

// Remote 托管实体运行中进入共享模式时框架停掉任务心跳（这里直接 Stop 模拟），回到独占后业务经 Beat().Sync() 恢复：
// 动作组与任务心跳一起重新挂上，任务没有被结束（真实 Remote 下的同一流程见 movement 的 shared_test.go）。
func TestBeatSyncResumesAStoppedMissionHeartbeat(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	behavior, _, d := newBeatBehaviorForTest(t, &now)
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: &runnerTestAction{label: "never ends"}, Timeout: time.Minute}}}
	if err := behavior.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	behavior.MissionReg().Stop()
	behavior.Beat().Reg(1).Stop()
	if err := behavior.Beat().Sync(); err != nil {
		t.Fatal(err)
	}
	if !behavior.MissionReg().Active() || !behavior.Beat().Reg(1).Active() || d.Registered() != 2 || behavior.CurMission() == nil {
		t.Fatalf("after Sync: mission beat %v group beat %v registered %d mission %v",
			behavior.MissionReg().Active(), behavior.Beat().Reg(1).Active(), d.Registered(), behavior.CurMission())
	}
}

// 没有时限的步不挂任务心跳；从有时限的步走到没有时限的步，心跳停。取消的步不重试。
func TestTheMissionHeartbeatFollowsTheCurrentStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	behavior, observer, _ := newBeatBehaviorForTest(t, &now)
	first := &runnerTestAction{label: "timed", done: true}
	second := &runnerTestAction{label: "untimed"}
	plan := MissionPlan{Steps: []MissionStep{{Action: 1, Param: first, Timeout: time.Second, Retry: 3}, {Action: 1, Param: second, Retry: 3}}}
	if err := behavior.SetMission(1, plan); err != nil {
		t.Fatal(err)
	}
	if reg := behavior.MissionReg(); reg == nil || !reg.Active() || reg.Period() != DefaultMissionPeriod {
		t.Fatalf("timed first step: %v", reg)
	}
	if err := behavior.Runner().Tick(1, now); err != nil { // 第一步完成，进入没有时限的第二步
		t.Fatal(err)
	}
	if behavior.MissionReg().Active() || behavior.CurAction(1) != second {
		t.Fatalf("untimed second step: mission beat active %v current %v", behavior.MissionReg().Active(), behavior.CurAction(1))
	}
	if err := behavior.Runner().End(1, true, NewActionReason("someone else's move")); err != nil {
		t.Fatal(err)
	}
	if second.starts != 1 || behavior.CurMission() != nil || observer.log[len(observer.log)-1] != "action 2 4 step -1" {
		t.Fatalf("a canceled step was retried: starts %d mission %v log %v", second.starts, behavior.CurMission(), observer.log)
	}
	if _, err := NormalizePlan(MissionPlan{Steps: []MissionStep{{Action: 1, Timeout: -time.Second}}}); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
}

// 步骤动作所在的组被冻结期间，这一步的时限不计时（定身长于时限时护送 / 巡逻不会直接失败），解冻后
// 按剩余时长继续；多次冻结累加。计时按 Behavior 的时钟。FreezeCancel 打断时这一步照现有规则结束（取消、不重试、走
// OnFail），不受影响。
func TestAFrozenGroupPausesItsStepTimeout(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	now := start
	behavior, observer, _ := newBeatBehaviorForTest(t, &now)
	escort := &runnerTestAction{label: "escort"}
	if err := behavior.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: escort, Timeout: time.Second}}}); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) { now = start.Add(d) }
	beat := func(d time.Duration) {
		at(d)
		if err := behavior.missionBeat(heartbeat.Beat{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	running := func(when string) {
		t.Helper()
		if escort.cancels != 0 || behavior.CurMission() == nil {
			t.Fatalf("%s: cancels %d mission %v log %v (the step timed out while its group was frozen)", when, escort.cancels, behavior.CurMission(), observer.log)
		}
	}
	at(400 * time.Millisecond) // 已用 400ms
	if err := behavior.Freeze(1, FreezePause); err != nil {
		t.Fatal(err)
	}
	beat(3 * time.Second) // 定身 2.6s，长于时限
	running("frozen 2.6s after 400ms of a 1s timeout")
	if err := behavior.Recover(1); err != nil {
		t.Fatal(err)
	}
	beat(3300 * time.Millisecond) // 已用 700ms
	running("700ms used")
	if err := behavior.Freeze(1, FreezePause); err != nil { // 第二次定身：累加
		t.Fatal(err)
	}
	beat(5 * time.Second)
	running("frozen again")
	if err := behavior.Recover(1); err != nil {
		t.Fatal(err)
	}
	beat(5200 * time.Millisecond) // 已用 900ms
	running("900ms used")
	beat(5300 * time.Millisecond) // 已用 1s：到时
	if escort.cancels != 1 || behavior.CurMission() != nil || behavior.Missions().MissionInfo().LastResult.Reason != stepTimedOut {
		t.Fatalf("1s of unfrozen time: cancels %d mission %v info %+v", escort.cancels, behavior.CurMission(), behavior.Missions().MissionInfo())
	}

	// FreezeCancel：打断即结束这一步（取消不重试），任务按 OnFail 结束，与时限无关。
	interrupted := &runnerTestAction{label: "patrol"}
	if err := behavior.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: interrupted, Timeout: time.Second, Retry: 3}}}); err != nil {
		t.Fatal(err)
	}
	if err := behavior.Freeze(1, FreezeCancel); err != nil {
		t.Fatal(err)
	}
	if info := behavior.Missions().MissionInfo(); interrupted.starts != 1 || interrupted.cancels != 1 || behavior.CurMission() != nil || info.Status != MissionStatusCanceled {
		t.Fatalf("FreezeCancel: starts %d cancels %d mission %v info %+v", interrupted.starts, interrupted.cancels, behavior.CurMission(), info)
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
	behavior, _, _ := newBehaviorForTest(t)
	first, second := &flakyAction{}, &flakyAction{}
	if err := behavior.SetMission(1, MissionPlan{Steps: []MissionStep{{Action: 1, Param: first, Retry: 1}, {Action: 1, Param: second, Retry: 1}}}); err != nil {
		t.Fatal(err)
	}
	if info := behavior.Missions().MissionInfo(); info.Status != MissionStatusSuccess || first.starts != 2 || second.starts != 2 {
		t.Fatalf("mission %+v, starts %d / %d (want each step retried once, then success)", info, first.starts, second.starts)
	}
}

// --- 动作指标（可选） ---

// 打开 Metrics 时：动作成为当前动作记一次 started（按种类）、结束记一次 ended（按种类与结束状态，含从未启动就被丢弃的
// 排队项），running 是此刻的当前动作数；不打开时什么都不写。
func TestBehaviorMetricsCountStartsEndsAndRunningActions(t *testing.T) {
	registry := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	build := func(enabled bool) *Behavior {
		owner := &beatEntity{entity.NewEntityBase(4<<20, entity.EntityCategoryOther, true)}
		behavior, err := NewBehavior(owner, BehaviorConfig{Registry: newActionOnlyRegistry(t), GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true }, Metrics: enabled})
		if err != nil {
			t.Fatal(err)
		}
		return behavior
	}
	quiet := build(false)
	if _, err := quiet.CreateAction(1, &runnerTestAction{}); err != nil {
		t.Fatal(err)
	}
	if got := len(registry.Snapshot()); got != 0 {
		t.Fatalf("an behavior without metrics wrote %d series", got)
	}

	behavior := build(true)
	if _, err := behavior.CreateAction(1, &runnerTestAction{label: "walk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := behavior.EnqueueAction(1, &runnerTestAction{label: "queued"}); err != nil {
		t.Fatal(err)
	}
	if _, err := behavior.CreateAction(2, &runnerTestAction{instant: true}); err != nil {
		t.Fatal(err)
	}
	behavior.ClearNextActions()
	if err := behavior.Runner().End(1, true, NewActionReason("stop")); err != nil {
		t.Fatal(err)
	}
	if _, err := behavior.CreateAction(1, &runnerTestAction{label: "again"}); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range registry.Snapshot() {
		key := m.Name
		if kind := m.Labels["kind"]; kind != "" {
			key += " " + kind
		}
		if status := m.Labels["status"]; status != "" {
			key += " " + status
		}
		got[key] = m.Value
	}
	want := map[string]int64{
		MetricActionsStarted + " 1": 2, MetricActionsStarted + " 2": 1,
		MetricActionsEnded + " 1 canceled": 2, MetricActionsEnded + " 2 success": 1,
		MetricActionsRunning: 1,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("metrics %v, want %v", got, want)
	}
}

// 没配 Now 时，Behavior 的业务时间（动作的 ActionContext.Now、任务的 MissionContext.Now，计划步骤 Timeout 的
// 起点）是本次执行冻结的 fctx.Now，与 handler 里读的同一时刻。
func TestBehaviorDefaultNowIsTheExecutionsFrozenBusinessTime(t *testing.T) {
	steppingBusinessClock(t)
	execution, release := fctx.NewContext()
	defer release()
	behavior, _, _ := newBehaviorForTest(t)
	action := &ctxAction{runnerTestAction: runnerTestAction{label: "now"}}
	if _, err := behavior.CreateAction(1, action); err != nil {
		t.Fatal(err)
	}
	if !action.seen.Now.Equal(execution.Now) {
		t.Fatalf("Behavior action context Now = %v, want the execution's frozen business time %v", action.seen.Now, execution.Now)
	}
}

// fixedDomainFields 是一个早已改过倍率的场景时间域（域时间与业务时间差得很远），测试用。
type fixedDomainFields struct{ state timedomain.State }

func (f *fixedDomainFields) TimeDomain() timedomain.State     { return f.state }
func (f *fixedDomainFields) SetTimeDomain(s timedomain.State) { f.state = s }

// Beat.TimeDomain 挂了场景的时间域时，Behavior 没配 Now 就取域时间（动作与任务的上下文、计划步骤 Timeout 的起点），
// 心跳之外经 BeatRunner 直接 Start 的动作 ActionContext.Now 也是域时间。
func TestBehaviorOnATimeDomainUsesDomainTime(t *testing.T) {
	domain, err := timedomain.New(&fixedDomainFields{state: timedomain.State{AnchorBusinessNano: 1, AnchorDomainNano: 5e17, Rate: 1}}, timedomain.Config{})
	if err != nil {
		t.Fatal(err)
	}
	execution, release := fctx.NewContext()
	defer release()
	d := heartbeat.NewDriver(heartbeat.Config{Name: "behavior-domain-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	owner := &beatEntity{entity.NewEntityBase(5<<20, entity.EntityCategoryOther, true)}
	behavior, err := NewBehavior(owner, BehaviorConfig{
		Registry:     newActionOnlyRegistry(t),
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Beat:         &BeatConfig{Host: d.NewHost(owner), TimeDomain: domain},
		OnError:      func(err error) { t.Errorf("behavior error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Now()
	if want.Equal(execution.Now) {
		t.Fatal("premise: the domain time equals the business time")
	}
	viaBehavior := &ctxAction{runnerTestAction: runnerTestAction{label: "behavior"}}
	if _, err := behavior.CreateAction(1, viaBehavior); err != nil {
		t.Fatal(err)
	}
	viaRunner := &ctxAction{runnerTestAction: runnerTestAction{label: "runner"}}
	if _, err := behavior.Beat().Runner().Start(2, viaRunner, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// 任务上下文的时间（计划步骤 Timeout 的起点）走 Behavior 自己的时钟，不经 BeatRunner。
	if got := behavior.now(); !got.Equal(want) {
		t.Fatalf("on a time domain the Behavior's clock (missions, step timeouts) reads %v, want the domain time %v", got, want)
	}
	if !viaBehavior.seen.Now.Equal(want) || !viaRunner.seen.Now.Equal(want) {
		t.Fatalf("on a time domain: Behavior action Now %v, runner action Now %v; want the domain time %v (business %v)",
			viaBehavior.seen.Now, viaRunner.seen.Now, want, execution.Now)
	}
}

// 实体卸载（删档、撤销新建同样）：组件在实体的 OnDestroy 里调 Behavior.OnDestroy，在途动作以“实体已销毁”结束（取消
// 状态，观察者收到），排队项丢弃，任务结束，动作指标的 running 减回；之后经 Behavior 发起的动作被拒绝。
func TestBehaviorOnDestroyEndsWhatIsRunning(t *testing.T) {
	registry := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	d := heartbeat.NewDriver(heartbeat.Config{Name: "destroy-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	owner := &beatEntity{entity.NewEntityBase(5<<20, entity.EntityCategoryOther, true)}
	host := d.NewHost(owner)
	observer := &behaviorObserver{reasons: map[int64]ActionReason{}}
	behavior, err := NewBehavior(owner, BehaviorConfig{
		Registry:     newActionOnlyRegistry(t),
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Beat:         &BeatConfig{Host: host},
		Observer:     observer,
		Metrics:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.behavior = behavior
	walk := &runnerTestAction{label: "walk"}
	walkID, err := behavior.CreateAction(1, walk)
	if err != nil {
		t.Fatal(err)
	}
	queuedID, err := behavior.EnqueueAction(1, &runnerTestAction{label: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	running := func() int64 {
		for _, m := range registry.Snapshot() {
			if m.Name == MetricActionsRunning {
				return m.Value
			}
		}
		return 0
	}
	if running() != 1 || d.Registered() != 1 {
		t.Fatalf("before destroy: running %d registered %d", running(), d.Registered())
	}
	// 实体的 OnDestroy：组件依次收尾，最后撤销心跳宿主。
	behavior.OnDestroy()
	host.OnDestroy()
	want := fmt.Sprintf("[action %d %d step -1 action %d %d step -1]", walkID, ActionStatusCanceled, queuedID, ActionStatusCanceled)
	if running() != 0 || fmt.Sprint(observer.log) != want || walk.cancels != 1 || d.Registered() != 0 {
		t.Fatalf("after destroy: running %d observer %v (want %s) walk cancels %d registered %d",
			running(), observer.log, want, walk.cancels, d.Registered())
	}
	if reason := observer.reasons[walkID]; !errors.Is(reason.Err, ErrOwnerDestroyed) {
		t.Fatalf("walk ended with %+v, want ErrOwnerDestroyed", reason)
	}
	if _, err := behavior.CreateAction(1, &runnerTestAction{}); !errors.Is(err, ErrBehaviorClosed) {
		t.Fatalf("CreateAction after OnDestroy = %v", err)
	}
	if err := behavior.SetMission(1, MissionPlan{}); !errors.Is(err, ErrBehaviorClosed) {
		t.Fatalf("SetMission after OnDestroy = %v", err)
	}
}

// 换场景（换时间域）：Close 旧 Behavior（任务与动作以给定原因结束，任务心跳与组心跳离开驱动、释放名字），在同一心跳
// 宿主上按新的域重建一个；新 Behavior 的组心跳与 "<Name>/mission" 都挂得上。
func TestABehaviorCanBeRebuiltOnTheSameHostAfterClose(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old, observer, d := newBeatBehaviorForTest(t, &now)
	plan := func() MissionPlan {
		return MissionPlan{Steps: []MissionStep{{Action: 1, Param: &runnerTestAction{label: "patrol"}, Timeout: time.Minute}}}
	}
	if err := old.SetMission(1, plan()); err != nil {
		t.Fatal(err)
	}
	if d.Registered() != 2 {
		t.Fatalf("before close: registered %d", d.Registered())
	}
	host := old.Beat().host
	old.Close(NewActionReason("scene change"))
	if d.Registered() != 0 || old.CurMission() != nil || old.CurAction(1) != nil || len(observer.log) != 2 {
		t.Fatalf("after close: registered %d mission %v action %v observer %v", d.Registered(), old.CurMission(), old.CurAction(1), observer.log)
	}
	registry := newActionOnlyRegistry(t)
	if err := registry.RegisterMission(1, func() Mission { return NewPlanMission(1) }); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewBehavior(host.Owner(), BehaviorConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { return ActionGroup(kind), true },
		Beat:         &BeatConfig{Host: host},
		Now:          func() time.Time { return now },
		OnError:      func(err error) { t.Errorf("rebuilt behavior error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.SetMission(1, plan()); err != nil {
		t.Fatal(err)
	}
	if d.Registered() != 2 || !rebuilt.MissionReg().Active() || !rebuilt.Beat().Reg(1).Active() {
		t.Fatalf("rebuilt behavior: registered %d", d.Registered())
	}
	if _, err := old.CreateAction(1, &runnerTestAction{}); !errors.Is(err, ErrBehaviorClosed) {
		t.Fatalf("CreateAction on a closed behavior = %v", err)
	}
}
