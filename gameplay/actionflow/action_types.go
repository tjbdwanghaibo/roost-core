// Package actionflow 定义游戏运行时使用的稳定动作与任务契约。实现必须由拥有它的实体在外部串行调用；本包刻意不再
// 引入第二个锁域。
//
// 回调里的变更延后执行：ActionRunner 与 MissionRunner 在
// 动作 / 任务回调和 runner 钩子运行期间收到的变更不立即生效，进各自的延后命令队列，最外层
// 调用返回前按发起顺序执行；Deferring() 报告此刻是否会被延后。队列与执行步数有界
// （ErrDeferredQueueFull / ErrDeferredRunaway），回调 panic 一律恢复成错误。
//
// 清场顺序：ActionRunner.EndAll 期间回调发起的动作（例如 PlanMission 在 OnFail 里推进到
// 下一步）会在 EndAll 返回后启动，EndAll 本身不是清场。要让实体什么都不再运行，先结束当前
// 任务（ActionList.EndCurMission / MissionRunner.EndCurMission——任务结束后送来的动作结束
// 被忽略，不再推进），再调 EndAll（EndAllAction）。
package actionflow

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
}

// MissionPlan 是计划任务：步骤、起始步与任务参数。
type MissionPlan struct {
	Steps []MissionStep
	Start int
	Param any
}

// MissionInfo 是任务当前的状态、步骤、动作与上一个结果。
type MissionInfo struct {
	Status        MissionStatus
	CurrentStep   int
	CurrentAction ActionKind
	LastResult    ActionResult
}
