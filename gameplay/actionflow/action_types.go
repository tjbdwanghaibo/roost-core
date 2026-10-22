// Package actionflow 定义游戏运行时使用的稳定动作与任务契约。实现必须由拥有它的实体在外部串行调用；本包刻意不再
// 引入第二个锁域。
//
// 回调里的变更延后执行：ActionRunner 与 MissionRunner 在
// 动作 / 任务回调和 runner 钩子运行期间收到的变更不立即生效，进各自的延后命令队列，最外层
// 调用返回前按发起顺序执行；Deferring() 报告此刻是否会被延后。队列与执行步数有界
// （ErrDeferredQueueFull / ErrDeferredRunaway），回调 panic 一律恢复成错误。
//
// 心跳驱动：实体的 runner 由 BeatRunner 接到实体自己的心跳上，每个组的当前动作按它的 TickInterval 被 tick（见
// heartbeat.go）。所有控制实体行为的逻辑都经 runner 下发与收尾。
//
// 事务（业务约束，维护者 2026-10-21：由业务层保证，框架不处理）：runner 是纯内存状态，不随 Nest 事务回滚。handler
// 先做完全部可能失败的校验与修改，最后一步才调用 Start / Enqueue / End 等；之后若还可能失败，回滚后 runner 里会
// 残留这次的变更。
//
// 清场顺序：ActionRunner.EndAll 期间回调发起的动作（例如 PlanMission 在 OnFail 里推进到
// 下一步）会在 EndAll 返回后启动，EndAll 本身不是清场。要让实体什么都不再运行，先结束当前
// 任务（ActionList.EndCurMission / MissionRunner.EndCurMission——任务结束后送来的动作结束
// 被忽略，不再推进），再调 EndAll（EndAllAction）。
package actionflow

import "time"

// ActionKind 是动作种类，ActionGroup 是动作所属的互斥组，MissionKind 是任务种类。
type ActionKind uint8
type ActionGroup uint8
type MissionKind uint16

// ActionStatus 是动作的状态。
type ActionStatus uint8

const (
	ActionStatusIdle ActionStatus = iota
	ActionStatusRunning
	ActionStatusSuccess
	ActionStatusFailed
	ActionStatusCanceled
	ActionStatusExpired
)

// String 返回状态名（指标标签用）：idle / running / success / failed / canceled / expired。
func (s ActionStatus) String() string {
	switch s {
	case ActionStatusIdle:
		return "idle"
	case ActionStatusRunning:
		return "running"
	case ActionStatusSuccess:
		return "success"
	case ActionStatusFailed:
		return "failed"
	case ActionStatusCanceled:
		return "canceled"
	case ActionStatusExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// MissionStatus 是任务的状态。
type MissionStatus uint8

const (
	MissionStatusIdle MissionStatus = iota
	MissionStatusRunning
	MissionStatusSuccess
	MissionStatusFailed
	MissionStatusCanceled
	MissionStatusExpired
)

// ActionResult 是动作的结果状态与原因。
type ActionResult struct {
	Status ActionStatus
	Reason string
}

// Terminal 报告结果是否是终态（成功、失败、取消或过期）。
func (r ActionResult) Terminal() bool {
	switch r.Status {
	case ActionStatusSuccess, ActionStatusFailed, ActionStatusCanceled, ActionStatusExpired:
		return true
	default:
		return false
	}
}

// ActionReason 是动作结束的原因：文字、错误与结构化结果。
type ActionReason struct {
	Message string
	Err     error
	Result  ActionResult
}

// NewActionReason 用文字构造原因。
func NewActionReason(message string) ActionReason { return ActionReason{Message: message} }

// NewActionErrorReason 用错误构造失败原因；err 为 nil 时返回零值。
func NewActionErrorReason(err error) ActionReason {
	if err == nil {
		return ActionReason{}
	}
	return ActionReason{Message: err.Error(), Err: err, Result: ActionResult{Status: ActionStatusFailed, Reason: err.Error()}}
}

// NewActionResultReason 用结构化结果构造原因；结果没有原因文字时补 "action done"。
func NewActionResultReason(result ActionResult) ActionReason {
	if result.Reason == "" {
		result.Reason = "action done"
	}
	return ActionReason{Message: result.Reason, Result: result}
}

// ToActionResult 返回原因里的结构化结果；没有结构化结果时用 fallback 状态与原因文字组成一个。
func (r ActionReason) ToActionResult(fallback ActionStatus) ActionResult {
	if r.Result.Status != ActionStatusIdle {
		if r.Result.Reason == "" {
			r.Result.Reason = r.Message
		}
		return r.Result
	}
	reason := r.Message
	if reason == "" && r.Err != nil {
		reason = r.Err.Error()
	}
	return ActionResult{Status: fallback, Reason: reason}
}

// MissionNextMode 是计划任务一步结束后的去向：下一步、成功结束或失败结束。
type MissionNextMode uint8

const (
	MissionNextUnset MissionNextMode = iota
	MissionNextStep
	MissionNextSuccessEnd
	MissionNextFailedEnd
)

// MissionNext 是一步结束后的去向；Mode 为 MissionNextStep 时 Step 是目标步。
type MissionNext struct {
	Mode MissionNextMode
	Step int
}

// NextMissionStep、MissionSuccessEnd 与 MissionFailedEnd 构造对应的去向。
func NextMissionStep(step int) MissionNext { return MissionNext{Mode: MissionNextStep, Step: step} }
func MissionSuccessEnd() MissionNext       { return MissionNext{Mode: MissionNextSuccessEnd} }
func MissionFailedEnd() MissionNext        { return MissionNext{Mode: MissionNextFailedEnd} }

// MissionStep 是计划里的一步：执行的动作、参数，以及成功 / 失败后的去向。
type MissionStep struct {
	Action    ActionKind
	Param     any
	OnSuccess MissionNext
	OnFail    MissionNext
	// Timeout 为正时是这一步每次尝试的时限（D21 A8）：到时还没结束，任务把这一步的动作以过期状态（ActionStatusExpired）
	// 结束，按失败处理（重试或 OnFail）。需要任务心跳（Behavior 配了 Beat），检查粒度是 MissionPlan.Period。动作所在的
	// 组被冻结期间不计时（定身时护送 / 巡逻不因此失败，解冻后按剩余时长继续；维护者 2026-10-22），按 Behavior 的时钟算；
	// FreezeCancel 打断时动作以取消结束，这一步照常走 OnFail。
	Timeout time.Duration
	// Retry 是这一步失败（Failed / Expired）后原地重试的次数，用完才走 OnFail；被取消（Canceled，例如被别的动作
	// 取代、被业务结束）不重试，直接走 OnFail。
	Retry int
}

// DefaultMissionPeriod 是计划任务检查步骤时限的缺省周期。
const DefaultMissionPeriod = 100 * time.Millisecond

// MissionPlan 是计划任务：步骤、起始步与任务参数。
type MissionPlan struct {
	Steps []MissionStep
	Start int
	Param any
	// Period 是当前步骤有 Timeout 时任务心跳的周期（取不大于它的心跳档），0 取 DefaultMissionPeriod。
	Period time.Duration
}

// MissionInfo 是任务当前的状态、步骤、动作与上一个结果。
type MissionInfo struct {
	Status        MissionStatus
	CurrentStep   int
	CurrentAction ActionKind
	LastResult    ActionResult
}
