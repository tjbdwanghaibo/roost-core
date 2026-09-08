package actionflow

import (
	"errors"
	"fmt"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

var (
	ErrMissionPlanInvalid = errors.New("taskflow: mission plan is invalid")
	ErrMissionStepInvalid = errors.New("taskflow: mission step is invalid")
)

type PlanMission struct {
	kind            coreflow.MissionKind
	id              int64
	manager         coreflow.MissionManager
	plan            coreflow.MissionPlan
	status          coreflow.MissionStatus
	currentStep     int
	currentAction   coreflow.ActionKind
	currentActionID int64
	lastResult      coreflow.ActionResult
}

func NewPlanMission(kind coreflow.MissionKind) *PlanMission {
	return &PlanMission{kind: kind, status: coreflow.MissionStatusIdle, currentStep: -1}
}

func (m *PlanMission) SetRuntime(id int64, manager coreflow.MissionManager) {
	m.id, m.manager = id, manager
}
func (m *PlanMission) ID() int64 {
	if m == nil {
		return 0
	}
	return m.id
}
func (m *PlanMission) Kind() coreflow.MissionKind {
	if m == nil {
		return 0
	}
	return m.kind
}
func (m *PlanMission) Status() coreflow.MissionStatus {
	if m == nil {
		return coreflow.MissionStatusIdle
	}
	return m.status
}

func (m *PlanMission) Start(ctx *coreflow.MissionContext, param any) error {
	if m == nil || ctx == nil || ctx.ActionList == nil {
		return ErrMissionPlanInvalid
	}
	plan, err := PlanFrom(param)
	if err != nil {
		return err
	}
	m.plan = plan
	m.status = coreflow.MissionStatusRunning
	m.currentStep = plan.Start
	m.currentAction = 0
	m.currentActionID = 0
	m.lastResult = coreflow.ActionResult{}
	return m.startStep(ctx, m.currentStep)
}
func (m *PlanMission) Tick(*coreflow.MissionContext, time.Time) {}
func (m *PlanMission) OnActionEnd(ctx *coreflow.MissionContext, actionID int64, _ coreflow.ActionKind, reason coreflow.ActionReason) {
	if m == nil || m.status != coreflow.MissionStatusRunning || actionID == 0 || actionID != m.currentActionID {
		return
	}
	result := reason.ToActionResult(coreflow.ActionStatusSuccess)
	if result.Status == coreflow.ActionStatusIdle {
		result.Status = coreflow.ActionStatusSuccess
	}
	m.lastResult = result
	if err := m.apply(ctx, result); err != nil {
		m.lastResult = coreflow.ActionResult{Status: coreflow.ActionStatusFailed, Reason: err.Error()}
		m.status = coreflow.MissionStatusFailed
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(coreflow.NewActionResultReason(m.lastResult))
		}
	}
}
func (m *PlanMission) End(_ *coreflow.MissionContext, reason coreflow.ActionReason) {
	if m == nil {
		return
	}
	if m.status == coreflow.MissionStatusRunning || m.status == coreflow.MissionStatusIdle {
		result := reason.ToActionResult(coreflow.ActionStatusCanceled)
		m.lastResult = result
		m.status = missionStatus(result)
		if result.Status == coreflow.ActionStatusSuccess {
			m.status = coreflow.MissionStatusSuccess
		}
	}
	m.currentStep, m.currentAction, m.currentActionID = -1, 0, 0
}
func (m *PlanMission) CanReplaceBy(coreflow.MissionKind, any) bool { return true }
func (m *PlanMission) MissionInfo() coreflow.MissionInfo {
	if m == nil {
		return coreflow.MissionInfo{CurrentStep: -1}
	}
	return coreflow.MissionInfo{Status: m.status, CurrentStep: m.currentStep, CurrentAction: m.currentAction, LastResult: m.lastResult}
}

func (m *PlanMission) startStep(ctx *coreflow.MissionContext, step int) error {
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
func (m *PlanMission) apply(ctx *coreflow.MissionContext, result coreflow.ActionResult) error {
	step := m.plan.Steps[m.currentStep]
	next := step.OnFail
	if result.Status == coreflow.ActionStatusSuccess {
		next = step.OnSuccess
	}
	if next.Mode == coreflow.MissionNextUnset {
		next = defaultNext(m.currentStep, len(m.plan.Steps), result.Status)
	}
	m.currentAction, m.currentActionID = 0, 0
	switch next.Mode {
	case coreflow.MissionNextStep:
		return m.startStep(ctx, next.Step)
	case coreflow.MissionNextSuccessEnd:
		m.status = coreflow.MissionStatusSuccess
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(coreflow.NewActionResultReason(result))
		}
	case coreflow.MissionNextFailedEnd:
		m.status = missionStatus(result)
		if ctx != nil && ctx.Manager != nil {
			ctx.Manager.EndCurMission(coreflow.NewActionResultReason(result))
		}
	default:
		return ErrMissionStepInvalid
	}
	return nil
}

func PlanFrom(param any) (coreflow.MissionPlan, error) {
	switch plan := param.(type) {
	case coreflow.MissionPlan:
		return NormalizePlan(plan)
	case *coreflow.MissionPlan:
		if plan == nil {
			return coreflow.MissionPlan{}, ErrMissionPlanInvalid
		}
		return NormalizePlan(*plan)
	default:
		return coreflow.MissionPlan{}, fmt.Errorf("%w: %T", ErrMissionPlanInvalid, param)
	}
}
func NormalizePlan(plan coreflow.MissionPlan) (coreflow.MissionPlan, error) {
	if len(plan.Steps) == 0 || plan.Start < 0 || plan.Start >= len(plan.Steps) {
		return coreflow.MissionPlan{}, ErrMissionPlanInvalid
	}
	plan.Steps = append([]coreflow.MissionStep(nil), plan.Steps...)
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.Action == 0 {
			return coreflow.MissionPlan{}, fmt.Errorf("%w: action none at %d", ErrMissionStepInvalid, index)
		}
		if err := validateNext(step.OnSuccess, len(plan.Steps)); err != nil {
			return coreflow.MissionPlan{}, err
		}
		if err := validateNext(step.OnFail, len(plan.Steps)); err != nil {
			return coreflow.MissionPlan{}, err
		}
		if step.OnSuccess.Mode == coreflow.MissionNextUnset {
			step.OnSuccess = defaultNext(index, len(plan.Steps), coreflow.ActionStatusSuccess)
		}
		if step.OnFail.Mode == coreflow.MissionNextUnset {
			step.OnFail = coreflow.MissionFailedEnd()
		}
	}
	return plan, nil
}
func validateNext(next coreflow.MissionNext, count int) error {
	if next.Mode == coreflow.MissionNextStep && (next.Step < 0 || next.Step >= count) {
		return fmt.Errorf("%w: next step %d", ErrMissionStepInvalid, next.Step)
	}
	return nil
}
func defaultNext(step, count int, status coreflow.ActionStatus) coreflow.MissionNext {
	if status == coreflow.ActionStatusSuccess && step+1 < count {
		return coreflow.NextMissionStep(step + 1)
	}
	if status == coreflow.ActionStatusSuccess {
		return coreflow.MissionSuccessEnd()
	}
	return coreflow.MissionFailedEnd()
}
func missionStatus(result coreflow.ActionResult) coreflow.MissionStatus {
	switch result.Status {
	case coreflow.ActionStatusCanceled:
		return coreflow.MissionStatusCanceled
	case coreflow.ActionStatusExpired:
		return coreflow.MissionStatusExpired
	default:
		return coreflow.MissionStatusFailed
	}
}

var _ coreflow.Mission = (*PlanMission)(nil)
