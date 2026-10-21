package actionflow

import (
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
)

// Agent 是框架提供的 ActionList 默认装配（DESIGN-TODO D21 A1，对照 ssr 的 action_list 模块那一层）：一个实体的
// ActionRunner（可选由心跳驱动，见 BeatRunner）、MissionRunner 与上层观察者（通常是 ai.Controller）按固定顺序接好，
// 业务不必各自再写一份胶水。接线规则：
//
//   - 动作结束：先交给当前任务（MissionRunner.OnActionEnd），再交给观察者（Observer.OnActionEnd）。任务正在自己的
//     回调里时（例如任务 Start 里发起的动作启动即失败），任务要等回调返回后才收到，观察者此时会先收到；
//   - 任务结束：先结束属于它的动作（ActionRunner.ClearMission，取消状态），再交给观察者（Observer.OnMissionEnd）；
//   - 动作归属任务：任务回调运行期间（含它同步引出的回调）经 CreateAction / EnqueueAction 发起的动作记在当前任务
//     名下（ssr HasOnProcessTag 的规则），任务结束时一并结束；其余调用方（AI、handler）发起的动作不属于任何任务；
//   - 清场（Clear）：先结束当前任务，再结束全部动作（包说明“清场顺序”）。
//
// 方法与 ActionRunner 一样只能在实体锁内调用（Nest handler、心跳回调、加载 / 新建的初始化阶段）；Agent 本身无锁、
// 不随事务回滚（D20 决定 3：由业务保证，handler 最后一步才调用）。
type Agent struct {
	owner    entity.IThreadSafeEntity
	runner   *ActionRunner
	beat     *BeatRunner
	missions *MissionRunner
	observer Observer
	now      func() time.Time
	onError  func(error)
}

// Observer 接收 Agent 的动作结束与任务结束，在任务处理之后调用。ai.Controller 实现它（actionflow 不导入 ai）。
type Observer interface {
	OnActionEnd(id int64, kind ActionKind, reason ActionReason)
	OnMissionEnd(mission Mission, reason ActionReason)
}

// AgentConfig 是 NewAgent 的配置。
type AgentConfig struct {
	// Registry 提供动作与任务构造器，GroupForKind 把动作种类映射到组；两者必填。
	Registry     *Registry
	GroupForKind func(ActionKind) (ActionGroup, bool)
	// Conflict 是可选的跨组冲突规则（ActionRunnerConfig.Conflict）。
	Conflict func(running, incoming ActionSnapshot) Conflict
	// DefaultMission 是 SetMission(0, …) 用的任务种类。
	DefaultMission MissionKind
	// Beat 非空时 runner 由实体心跳驱动（NewBeatRunner）；为空时不挂心跳，由调用方 Tick（测试、手动驱动）。
	// Beat.Host 必须属于 owner。
	Beat *BeatConfig
	// Observer 可空。
	Observer Observer
	// Now 是业务时钟，nil 取 clock.Now（D-L3）。
	Now func() time.Time
	// OnError 接收 runner 与任务的错误（回调 panic、延后命令失败等），可空。
	OnError func(error)
	// MaxDeferredCommands / MaxDeferredSteps 同 ActionRunnerConfig / MissionRunnerConfig，两个 runner 共用。
	MaxDeferredCommands int
	MaxDeferredSteps    int
}

// NewAgent 为 owner 构造并接好 ActionRunner（或 BeatRunner）与 MissionRunner。
func NewAgent(owner entity.IThreadSafeEntity, config AgentConfig) (*Agent, error) {
	if owner == nil {
		return nil, errors.New("actionflow: agent needs an owner entity")
	}
	if config.Beat != nil && config.Beat.Host != nil && config.Beat.Host.Owner() != owner {
		return nil, errors.New("actionflow: agent beat host belongs to another entity")
	}
	a := &Agent{owner: owner, observer: config.Observer, now: config.Now, onError: config.OnError}
	if a.now == nil {
		a.now = clock.Now
	}
	runnerConfig := ActionRunnerConfig{
		Registry:     config.Registry,
		GroupForKind: config.GroupForKind,
		Conflict:     config.Conflict,
		Hooks: ActionRunnerHooks{
			PopulateContext: a.populate,
			OnEnded:         a.actionEnded,
			OnError:         a.report,
		},
		MaxDeferredCommands: config.MaxDeferredCommands,
		MaxDeferredSteps:    config.MaxDeferredSteps,
	}
	var err error
	if config.Beat != nil {
		a.beat, err = NewBeatRunner(runnerConfig, *config.Beat)
		if a.beat != nil {
			a.runner = a.beat.Runner()
		}
	} else {
		a.runner, err = NewActionRunner(runnerConfig)
	}
	if err != nil {
		return nil, err
	}
	a.missions, err = NewMissionRunner(MissionRunnerConfig{
		Registry:    config.Registry,
		DefaultKind: config.DefaultMission,
		Hooks: MissionRunnerHooks{
			Now:          a.now,
			Context:      a.missionContext,
			ClearActions: a.clearMissionActions,
			OnEnded:      a.missionEnded,
			OnError:      a.report,
		},
		MaxDeferredCommands: config.MaxDeferredCommands,
		MaxDeferredSteps:    config.MaxDeferredSteps,
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Runner 返回动作 runner；Beat 返回心跳驱动的宿主（未配置 Beat 时为 nil）；Missions 返回任务 runner。
func (a *Agent) Runner() *ActionRunner    { return a.runner }
func (a *Agent) Beat() *BeatRunner        { return a.beat }
func (a *Agent) Missions() *MissionRunner { return a.missions }

// Clear 清场：先结束当前任务（之后送来的动作结束不再推进任务），再强制结束全部动作、丢弃排队项。
func (a *Agent) Clear(reason ActionReason) {
	a.missions.EndCurMission(reason)
	a.report(a.runner.EndAll(true, reason))
}

// --- ActionList ---

func (a *Agent) Entity() entity.IThreadSafeEntity { return a.owner }

// CreateAction 启动动作，取代同组当前动作（Replacer / Conflict 规则见 ActionRunner）。
func (a *Agent) CreateAction(kind ActionKind, param any) (int64, error) {
	return a.runner.Start(kind, param, a.missionTag(), a.now())
}

// EnqueueAction 把动作排到组队列末尾。
func (a *Agent) EnqueueAction(kind ActionKind, param any) (int64, error) {
	return a.runner.Enqueue(kind, param, a.missionTag())
}

func (a *Agent) CurAction(group ActionGroup) Action { return a.runner.Current(group) }

func (a *Agent) EndCurAction(group ActionGroup, force bool, reason ActionReason) {
	a.report(a.runner.End(group, force, reason))
}

func (a *Agent) EndAction(id int64, force bool, reason ActionReason) error {
	return a.runner.EndAction(id, force, reason)
}

func (a *Agent) EndAllAction(force bool, reason ActionReason) {
	a.report(a.runner.EndAll(force, reason))
}

func (a *Agent) ClearNextActions() { a.runner.ClearQueue() }

func (a *Agent) UpdateAction(group ActionGroup, f func(Action) error) error {
	return a.runner.Update(group, f)
}

func (a *Agent) Freeze(group ActionGroup, mode FreezeMode) error { return a.runner.Freeze(group, mode) }
func (a *Agent) Recover(group ActionGroup) error                 { return a.runner.Recover(group, a.now()) }
func (a *Agent) Frozen(group ActionGroup) bool                   { return a.runner.Frozen(group) }

func (a *Agent) MissionManager() MissionManager { return a.missions }

func (a *Agent) SetMission(kind MissionKind, param any) error {
	return a.missions.StartMission(kind, param)
}

func (a *Agent) CurMission() Mission { return a.missions.CurMission() }

func (a *Agent) EndCurMission(reason ActionReason) { a.missions.EndCurMission(reason) }

// --- 接线 ---

// missionTag 是新动作的归属任务：任务回调运行期间（MissionRunner 正在执行）发起的动作属于当前任务。
func (a *Agent) missionTag() int64 {
	if a.missions.Deferring() {
		return a.missions.CurrentMissionID()
	}
	return 0
}

func (a *Agent) populate(ctx *ActionContext, _ time.Time) {
	ctx.Owner = a.owner
	ctx.ActionList = a
	ctx.CurrentMission = a.missions.CurMission()
}

func (a *Agent) missionContext(now time.Time) *MissionContext {
	return &MissionContext{Owner: a.owner, ActionList: a, Now: now}
}

// actionEnded：先任务、后观察者。
func (a *Agent) actionEnded(s ActionSnapshot, reason ActionReason) {
	a.missions.OnActionEnd(s.ID, s.Kind, reason)
	if a.observer != nil {
		a.observer.OnActionEnd(s.ID, s.Kind, reason)
	}
}

func (a *Agent) clearMissionActions(missionID int64, reason ActionReason) error {
	return a.runner.ClearMission(missionID, true, reason)
}

func (a *Agent) missionEnded(m Mission, reason ActionReason) {
	if a.observer != nil {
		a.observer.OnMissionEnd(m, reason)
	}
}

func (a *Agent) report(err error) {
	if err != nil && a.onError != nil {
		defer func() { _ = recover() }()
		a.onError(err)
	}
}

var _ ActionList = (*Agent)(nil)
