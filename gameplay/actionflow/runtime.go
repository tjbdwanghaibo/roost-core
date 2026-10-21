package actionflow

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// ActionContext 只在回调期间有效。实现不得在 Start、Tick 或 Cancel 返回后保留它的指针；runner 可能复用这块存储。
type ActionContext struct {
	Owner          entity.IThreadSafeEntity
	ActionList     ActionList
	CurrentMission Mission
	Now            time.Time
	// Elapsed 是心跳 tick 时本 runner 的心跳登记距上次执行的业务时间（heartbeat.Beat.Elapsed，不追帧：掉拍后一次
	// 覆盖）。只有 BeatRunner 的心跳 tick 填它；直接调用、Wake 推进、Start / Cancel 时为 0。
	Elapsed time.Duration
	// Waker 由 BeatRunner 填入：由事件推进、不需要周期 tick 的动作（TickInterval 返回 0）保存它，事件到达时调用
	// Wake 让 runner 马上 tick 自己所在的组。不经 BeatRunner 时为 nil。
	Waker Waker
}

// Action 是一个动作：Start 启动，Tick 推进并报告是否结束及结果，Cancel 取消。
type Action interface {
	Kind() ActionKind
	Start(ctx *ActionContext) error
	Tick(ctx *ActionContext) (done bool, result ActionResult)
	Cancel(ctx *ActionContext, reason string)
}

// TickIntervaler 由动作可选实现，告诉 BeatRunner 自己要多久 tick 一次：
//   - 不实现：按 BeatConfig.Period（缺省 100ms）；
//   - 返回 0：动作由事件推进（例如移动由 Move 组件自己的心跳推进、到达时 Wake），runner 不为它挂心跳；
//   - 返回 > 0：取 heartbeat.Periods 里不大于它的最长档（短于最短档时取最短档）。
//
// runner 的心跳周期是当前全部组里需要 tick 的动作中最短的那个；没有需要 tick 的动作时心跳停止。
type TickIntervaler interface {
	TickInterval() time.Duration
}

// Waker 让由事件推进的动作在事件到达时请求马上 tick 自己所在的组（BeatRunner 实现）。只在实体锁内调用。
type Waker interface {
	Wake(Action) error
}

// ActionList 是业务对 ActionRunner / MissionRunner 的接线。动作回调与 runner 钩子里调用
// CreateAction / EnqueueAction / EndCurAction / EndAllAction / ClearNextActions 时，ActionRunner
// 把变更延后到回调返回后按序执行：返回的动作 ID 已分配、可以立即记下，结束照样以这个 ID
// 送达（见 ActionRunner 的说明）。任务回调与 MissionRunner 钩子里调用 SetMission /
// EndCurMission 同样延后（见 MissionRunner 的说明）：SetMission 返回 nil，执行时的错误经
// MissionRunner 的 OnError 报告。
//
// 清场：EndAllAction 期间回调发起的动作会在它返回后启动；要什么都不再运行，先
// EndCurMission，再 EndAllAction。
type ActionList interface {
	Entity() entity.IThreadSafeEntity
	CreateAction(kind ActionKind, param any) (int64, error)
	EnqueueAction(kind ActionKind, param any) (int64, error)
	CurAction(group ActionGroup) Action
	EndCurAction(group ActionGroup, force bool, reason ActionReason)
	EndAllAction(force bool, reason ActionReason)
	ClearNextActions()
	UpdateAction(group ActionGroup, f func(Action) error) error
	Frozen(group ActionGroup)
	Recover(group ActionGroup)
	GetFrozen(group ActionGroup) bool

	MissionManager() MissionManager
	SetMission(kind MissionKind, param any) error
	CurMission() Mission
	EndCurMission(reason ActionReason)
}

// MissionContext 是任务回调的上下文。
type MissionContext struct {
	Owner      entity.IThreadSafeEntity
	ActionList ActionList
	Manager    MissionManager
	Now        time.Time
}

// Mission 是一个任务：编排一系列动作，并在动作结束时决定下一步。
type Mission interface {
	ID() int64
	Kind() MissionKind
	Status() MissionStatus
	Start(ctx *MissionContext, param any) error
	Tick(ctx *MissionContext, now time.Time)
	OnActionEnd(ctx *MissionContext, actionID int64, kind ActionKind, reason ActionReason)
	End(ctx *MissionContext, reason ActionReason)
	CanReplaceBy(kind MissionKind, param any) bool
	MissionInfo() MissionInfo
}

// MissionManager 是任务回调看到的任务管理接口（MissionRunner 实现）。
type MissionManager interface {
	StartMission(kind MissionKind, param any) error
	CancelMission(reason string) error
	InMission() bool
	CurrentAction() ActionKind
	CurMission() Mission
	MissionInfo() MissionInfo
	OnActionEnd(actionID int64, kind ActionKind, reason ActionReason)
	EndCurMission(reason ActionReason)
}

// MissionRuntimeSetter 由需要知道自己 ID 与管理者的任务实现，runner 在启动前调用。
type MissionRuntimeSetter interface {
	SetRuntime(id int64, manager MissionManager)
}
