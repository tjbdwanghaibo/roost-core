package actionflow

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrMissionPlanInvalid = errors.New("taskflow: mission plan is invalid")
	ErrMissionStepInvalid = errors.New("taskflow: mission step is invalid")
)

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
}

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
func (m *PlanMission) Tick(*MissionContext, time.Time) {}
func (m *PlanMission) OnActionEnd(ctx *MissionContext, actionID int64, _ ActionKind, reason ActionReason) {
	if m == nil || m.status != MissionStatusRunning || actionID == 0 || actionID != m.currentActionID {
		return
	}
	result := reason.ToActionResult(ActionStatusSuccess)
	if result.Status == ActionStatusIdle {
		result.Status = ActionStatusSuccess
	}
	m.lastResult = result
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
}
func (m *PlanMission) CanReplaceBy(MissionKind, any) bool { return true }
func (m *PlanMission) MissionInfo() MissionInfo {
	if m == nil {
		return MissionInfo{CurrentStep: -1}
	}
	return MissionInfo{Status: m.status, CurrentStep: m.currentStep, CurrentAction: m.currentAction, LastResult: m.lastResult}
}

func (m *PlanMission) startStep(ctx *MissionContext, step int) error {
	if step < 0 || step >= len(m.plan.Steps) {
		return fmt.Errorf("%w: index %d", ErrMissionStepInvalid, step)
	}
	item := m.plan.Steps[step]
	actionID, err := ctx.ActionList.CreateAction(item.Action, item.Param)
	if err != nil {
		return err
	}
	m.currentStep, m.currentAction, m.currentActionID = step, item.Action, actionID
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
	m.currentAction, m.currentActionID = 0, 0
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
func NormalizePlan(plan MissionPlan) (MissionPlan, error) {
	if len(plan.Steps) == 0 || plan.Start < 0 || plan.Start >= len(plan.Steps) {
		return MissionPlan{}, ErrMissionPlanInvalid
	}
	plan.Steps = append([]MissionStep(nil), plan.Steps...)
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.Action == 0 {
			return MissionPlan{}, fmt.Errorf("%w: action none at %d", ErrMissionStepInvalid, index)
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

var _ Mission = (*PlanMission)(nil)
