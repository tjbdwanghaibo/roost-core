package actionflow

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrMissionPlanInvalid = errors.New("actionflow: mission plan is invalid")
	ErrMissionStepInvalid = errors.New("actionflow: mission step is invalid")
)

// PlanMission 按 MissionPlan 逐步执行动作的通用任务。每步可以有时限（Timeout）与重试次数（Retry，D21 A8）：时限
// 由任务心跳检查（实现 TickIntervaler，只在当前步有时限时要 tick），到时把这一步的动作以过期结束；失败或过期且还有
// 重试次数时原地再发起这一步，否则按 OnFail 走。更复杂的分支（按条件、按结果原因）留给行为树，计划里不加表达式。
type PlanMission struct {
	kind            MissionKind
	id              int64
	manager         MissionManager
	plan            MissionPlan
	status          MissionStatus
	currentStep     int
	currentAction   ActionKind
	currentActionID int64
	lastResult      ActionResult
	// attempts 是当前步已经重试的次数；deadline 是当前尝试的时限（零值表示没有）。
	attempts int
	deadline time.Time
}

// NewPlanMission 构造一个尚未开始的计划任务。
func NewPlanMission(kind MissionKind) *PlanMission {
	return &PlanMission{kind: kind, status: MissionStatusIdle, currentStep: -1}
}

func (m *PlanMission) SetRuntime(id int64, manager MissionManager) {
	m.id, m.manager = id, manager
}
func (m *PlanMission) ID() int64 {
	if m == nil {
		return 0
	}
	return m.id
}
func (m *PlanMission) Kind() MissionKind {
	if m == nil {
		return 0
	}
	return m.kind
}
func (m *PlanMission) Status() MissionStatus {
	if m == nil {
		return MissionStatusIdle
	}
	return m.status
}

func (m *PlanMission) Start(ctx *MissionContext, param any) error {
	if m == nil || ctx == nil || ctx.ActionList == nil {
		return ErrMissionPlanInvalid
	}
	plan, err := PlanFrom(param)
	if err != nil {
		return err
	}
	m.plan = plan
	m.status = MissionStatusRunning
	m.currentStep = plan.Start
	m.currentAction = 0
	m.currentActionID = 0
	m.lastResult = ActionResult{}
	return m.startStep(ctx, m.currentStep)
}

// stepTimedOut 是步骤超时时结束动作用的原因（过期状态）。
const stepTimedOut = "mission step timed out"

// Tick 检查当前尝试的时限：到时以过期状态结束这一步的动作（结论经 OnActionEnd 回来，按失败处理）。
func (m *PlanMission) Tick(ctx *MissionContext, now time.Time) {
	if m == nil || m.status != MissionStatusRunning || m.currentActionID == 0 || m.deadline.IsZero() || now.Before(m.deadline) {
		return
	}
	m.deadline = time.Time{}
	if ctx == nil || ctx.ActionList == nil {
		return
	}
	// 找不到说明动作已经结束、结论还在路上（延后送达），不是错误。
	_ = ctx.ActionList.EndAction(m.currentActionID, true, ActionReason{Message: stepTimedOut,
		Result: ActionResult{Status: ActionStatusExpired, Reason: stepTimedOut}})
}

// TickInterval 让 Behavior 只在当前步有时限时给任务挂心跳（周期 MissionPlan.Period，缺省 DefaultMissionPeriod）。
func (m *PlanMission) TickInterval() time.Duration {
	if m == nil || m.status != MissionStatusRunning || m.currentStep < 0 || m.currentStep >= len(m.plan.Steps) ||
		m.plan.Steps[m.currentStep].Timeout <= 0 {
		return 0
	}
	if m.plan.Period > 0 {
		return m.plan.Period
	}
	return DefaultMissionPeriod
}

func (m *PlanMission) OnActionEnd(ctx *MissionContext, actionID int64, _ ActionKind, reason ActionReason) {
	if m == nil || m.status != MissionStatusRunning || actionID == 0 || actionID != m.currentActionID {
		return
	}
	result := reason.ToActionResult(ActionStatusSuccess)
	if result.Status == ActionStatusIdle {
		result.Status = ActionStatusSuccess
	}
	m.lastResult = result
	if m.retryable(result) {
		m.attempts++
		err := m.launch(ctx, m.currentStep)
		if err == nil {
			return
		}
		m.lastResult = ActionResult{Status: ActionStatusFailed, Reason: err.Error()}
		result = m.lastResult
	}
	if err := m.apply(ctx, result); err != nil {
		m.lastResult = ActionResult{Status: ActionStatusFailed, Reason: err.Error()}
		m.status = MissionStatusFailed
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(NewActionResultReason(m.lastResult))
		}
	}
}
func (m *PlanMission) End(_ *MissionContext, reason ActionReason) {
	if m == nil {
		return
	}
	if m.status == MissionStatusRunning || m.status == MissionStatusIdle {
		result := reason.ToActionResult(ActionStatusCanceled)
		m.lastResult = result
		m.status = missionStatus(result)
		if result.Status == ActionStatusSuccess {
			m.status = MissionStatusSuccess
		}
	}
	m.currentStep, m.currentAction, m.currentActionID = -1, 0, 0
	m.attempts, m.deadline = 0, time.Time{}
}
func (m *PlanMission) CanReplaceBy(MissionKind, any) bool { return true }
func (m *PlanMission) MissionInfo() MissionInfo {
	if m == nil {
		return MissionInfo{CurrentStep: -1}
	}
	return MissionInfo{Status: m.status, CurrentStep: m.currentStep, CurrentAction: m.currentAction, LastResult: m.lastResult}
}

// retryable 报告这次结果能否原地重试：失败或过期、且当前步还有重试次数。取消不重试（有人有意结束了它）。
func (m *PlanMission) retryable(result ActionResult) bool {
	if result.Status != ActionStatusFailed && result.Status != ActionStatusExpired {
		return false
	}
	return m.attempts < m.plan.Steps[m.currentStep].Retry
}

// startStep 进入一步（重试次数清零）并发起它。
func (m *PlanMission) startStep(ctx *MissionContext, step int) error {
	m.attempts = 0
	return m.launch(ctx, step)
}

// launch 发起一步的一次尝试：创建动作，按 Timeout 记下这次尝试的时限（起点是任务上下文的时间）。
func (m *PlanMission) launch(ctx *MissionContext, step int) error {
	if step < 0 || step >= len(m.plan.Steps) {
		return fmt.Errorf("%w: index %d", ErrMissionStepInvalid, step)
	}
	item := m.plan.Steps[step]
	m.deadline = time.Time{}
	actionID, err := ctx.ActionList.CreateAction(item.Action, item.Param)
	if err != nil {
		return err
	}
	m.currentStep, m.currentAction, m.currentActionID = step, item.Action, actionID
	if item.Timeout > 0 {
		m.deadline = ctx.Now.Add(item.Timeout)
	}
	return nil
}
func (m *PlanMission) apply(ctx *MissionContext, result ActionResult) error {
	step := m.plan.Steps[m.currentStep]
	next := step.OnFail
	if result.Status == ActionStatusSuccess {
		next = step.OnSuccess
	}
	if next.Mode == MissionNextUnset {
		next = defaultNext(m.currentStep, len(m.plan.Steps), result.Status)
	}
	m.currentAction, m.currentActionID, m.deadline = 0, 0, time.Time{}
	switch next.Mode {
	case MissionNextStep:
		return m.startStep(ctx, next.Step)
	case MissionNextSuccessEnd:
		m.status = MissionStatusSuccess
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(NewActionResultReason(result))
		}
	case MissionNextFailedEnd:
		m.status = missionStatus(result)
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(NewActionResultReason(result))
		}
	default:
		return ErrMissionStepInvalid
	}
	return nil
}

// PlanFrom 从任务参数取出 MissionPlan（值或非 nil 指针）并规范化。
func PlanFrom(param any) (MissionPlan, error) {
	switch plan := param.(type) {
	case MissionPlan:
		return NormalizePlan(plan)
	case *MissionPlan:
		if plan == nil {
			return MissionPlan{}, ErrMissionPlanInvalid
		}
		return NormalizePlan(*plan)
	default:
		return MissionPlan{}, fmt.Errorf("%w: %T", ErrMissionPlanInvalid, param)
	}
}

// NormalizePlan 校验计划（步骤非空、起始步与去向在范围内、动作非空）并返回规范化的副本：未设置的成功去向取下一步
// （最后一步则成功结束），未设置的失败去向取失败结束。
func NormalizePlan(plan MissionPlan) (MissionPlan, error) {
	if len(plan.Steps) == 0 || plan.Start < 0 || plan.Start >= len(plan.Steps) || plan.Period < 0 {
		return MissionPlan{}, ErrMissionPlanInvalid
	}
	plan.Steps = append([]MissionStep(nil), plan.Steps...)
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.Action == 0 {
			return MissionPlan{}, fmt.Errorf("%w: action none at %d", ErrMissionStepInvalid, index)
		}
		if step.Timeout < 0 || step.Retry < 0 {
			return MissionPlan{}, fmt.Errorf("%w: negative timeout or retry at %d", ErrMissionStepInvalid, index)
		}
		if err := validateNext(step.OnSuccess, len(plan.Steps)); err != nil {
			return MissionPlan{}, err
		}
		if err := validateNext(step.OnFail, len(plan.Steps)); err != nil {
			return MissionPlan{}, err
		}
		if step.OnSuccess.Mode == MissionNextUnset {
			step.OnSuccess = defaultNext(index, len(plan.Steps), ActionStatusSuccess)
		}
		if step.OnFail.Mode == MissionNextUnset {
			step.OnFail = MissionFailedEnd()
		}
	}
	return plan, nil
}
func validateNext(next MissionNext, count int) error {
	if next.Mode == MissionNextStep && (next.Step < 0 || next.Step >= count) {
		return fmt.Errorf("%w: next step %d", ErrMissionStepInvalid, next.Step)
	}
	return nil
}
func defaultNext(step, count int, status ActionStatus) MissionNext {
	if status == ActionStatusSuccess && step+1 < count {
		return NextMissionStep(step + 1)
	}
	if status == ActionStatusSuccess {
		return MissionSuccessEnd()
	}
	return MissionFailedEnd()
}
func missionStatus(result ActionResult) MissionStatus {
	switch result.Status {
	case ActionStatusCanceled:
		return MissionStatusCanceled
	case ActionStatusExpired:
		return MissionStatusExpired
	default:
		return MissionStatusFailed
	}
}

var (
	_ Mission        = (*PlanMission)(nil)
	_ TickIntervaler = (*PlanMission)(nil)
)
