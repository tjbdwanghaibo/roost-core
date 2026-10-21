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
	// Elapsed 是心跳 tick 时本组心跳登记距上次执行的业务时间（heartbeat.Beat.Elapsed，不追帧：掉拍后一次覆盖）。
	// 只有 BeatRunner 的心跳 tick 填它；直接调用 Tick、Start / Cancel 时为 0。按时间推进的动作（移动）用它算这一拍
	// 走多远。
	Elapsed time.Duration
}

// Action 是一个动作：Start 启动，Tick 推进并报告是否结束及结果，Cancel 取消。
//
// Start 的三种结论（D21 A7，维护者 2026-10-21 决定改签名）：
//   - err != nil：没能启动。runner 调 Cancel 收尾，以失败结束（OnEnded 失败状态），Start / Enqueue 的错误就是它；
//   - done == true：启动即完成（瞬时动作，例如传送、改朝向）。runner 不再 tick 它，立即以 result 结束（状态为空
//     时算成功），照常发 OnEnded、通知任务，再推进本组队列；不调 Cancel；
//   - 否则：进入运行，之后由 Tick 报告结束。
type Action interface {
	Kind() ActionKind
	Start(ctx *ActionContext) (done bool, result ActionResult, err error)
	Tick(ctx *ActionContext) (done bool, result ActionResult)
	Cancel(ctx *ActionContext, reason string)
}

// ReplaceDecision 是当前动作对“被同组新动作取代”的答复（Replacer）。
type ReplaceDecision uint8

const (
	// ReplaceAllow：取代（缺省，与不实现 Replacer 相同）。
	ReplaceAllow ReplaceDecision = iota
	// ReplaceKeep：保留当前动作，新动作不启动、以取消状态结束（拿到的 ID 照样有一次 OnEnded；Start 返回 nil）。
	// 用于“同一个请求不必重来”，例如同目的地的移动。
	ReplaceKeep
	// ReplaceReject：保留当前动作，拒绝新动作：直接调用时 Start 返回 ErrReplaceRejected；回调里延后的 Start 以
	// 失败状态结束。用于“手动操作不被 AI 的同类动作打断”。
	ReplaceReject
)

// Replacer 由动作可选实现（D21 A2，对照 ssr 的 CanCancelByNext / TryCancel）：同组的新动作要取代它时先问它。
// 只在 Start 取代当前动作时询问；End / EndAll / EndAction / 冻结打断 / 跨组抢占（Conflict 的 PreemptRunning）都是
// 强制结束，不问。ReplaceBy 只做判断，不改 runner（回调期间的变更照常延后），panic 按 ReplaceAllow 处理并经 OnError
// 报告。
type Replacer interface {
	ReplaceBy(ctx *ActionContext, next ActionSnapshot) ReplaceDecision
}

// TickIntervaler 由动作可选实现，告诉 BeatRunner 自己所在的组要多久 tick 一次（在动作成为当前动作时读一次）：
//   - 不实现：按 BeatConfig.Period（缺省 100ms）；
//   - 返回 0：不需要周期 tick（由别的动作或 handler 结束它），这个组不挂心跳；
//   - 返回 > 0：取 heartbeat.Periods 里不大于它的最长档（短于最短档时取最短档）。
type TickIntervaler interface {
	TickInterval() time.Duration
}

// ActionList 是业务对 ActionRunner / MissionRunner 的接线，框架的实现是 Behavior（D21 A1）。动作回调与 runner 钩子里
// 调用 CreateAction / EnqueueAction / EndCurAction / EndAction / EndAllAction / ClearNextActions 时，ActionRunner
// 把变更延后到回调返回后按序执行：返回的动作 ID 已分配、可以立即记下，结束照样以这个 ID 送达（见 ActionRunner
// 的说明）。任务回调与 MissionRunner 钩子里调用 SetMission / EndCurMission 同样延后（见 MissionRunner 的说明）：
// SetMission 返回 nil，执行时的错误经 MissionRunner 的 OnError 报告。
//
// 清场：EndAllAction 期间回调发起的动作会在它返回后启动；要什么都不再运行，先 EndCurMission，再 EndAllAction
// （Behavior.Clear 按这个顺序）。
type ActionList interface {
	Entity() entity.IThreadSafeEntity
	CreateAction(kind ActionKind, param any) (int64, error)
	EnqueueAction(kind ActionKind, param any) (int64, error)
	CurAction(group ActionGroup) Action
	EndCurAction(group ActionGroup, force bool, reason ActionReason)
	// EndAction 按 ID 结束当前或排队中的动作（D21 A4，对照 ssr EndAction）；找不到时返回 ErrActionNotFound。
	EndAction(id int64, force bool, reason ActionReason) error
	EndAllAction(force bool, reason ActionReason)
	ClearNextActions()
	UpdateAction(group ActionGroup, f func(Action) error) error
	// Freeze / Recover 是计数的冻结（D21 A5）：每次 Freeze 都要有一次 Recover，计数归零才解冻。
	Freeze(group ActionGroup, mode FreezeMode) error
	Recover(group ActionGroup) error
	Frozen(group ActionGroup) bool

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
