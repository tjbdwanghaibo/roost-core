package actionflow

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 动作指标（D21 A9，BehaviorConfig.Metrics 打开时写；缺省关闭）。标签 kind 是动作种类编号（uint8，有界），status 是
// ActionStatus.String()。
const (
	// MetricActionsStarted：成为组的当前动作的次数（含 Start 即完成的）。
	MetricActionsStarted = "actionflow.actions.started_total"
	// MetricActionsEnded：结束的动作（含从未启动就被丢弃的排队项、被拒绝的延后 Start），按结束状态。
	MetricActionsEnded = "actionflow.actions.ended_total"
	// MetricActionsRunning：此刻是某组当前动作的动作数（全部打开指标的 Behavior 合计）。
	MetricActionsRunning = "actionflow.actions.running"
)

// ErrBehaviorClosed 表示 Behavior 已 Close / OnDestroy：经它发起的动作与任务被拒绝。
var ErrBehaviorClosed = errors.New("actionflow: behavior is closed")

// ErrOwnerDestroyed 是 Behavior.OnDestroy 结束在途动作与任务的原因（实体卸载、删档或撤销新建）。
var ErrOwnerDestroyed = errors.New("actionflow: owner entity destroyed")

// Behavior 是实体的行为组件（运行时组件，不带 DAO）：框架提供的 ActionList 默认实现（DESIGN-TODO D21 A1，对照 ssr 的
// action_list 模块那一层，维护者 2026-10-21 由 Agent 更名为 Behavior，避免与 AI agent 混淆）。一个实体的
// ActionRunner（可选由心跳驱动，见 BeatRunner）、MissionRunner 与上层观察者（通常是 ai.Controller）按固定顺序接好，
// 业务不必各自再写一份胶水。接线规则：
//
//   - 动作结束：先交给当前任务（MissionRunner.OnActionEnd），再交给观察者（Observer.OnActionEnd）。任务正在自己的
//     回调里时（例如任务 Start 里发起的动作启动即失败），任务要等回调返回后才收到，观察者此时会先收到；
//   - 任务结束：先结束属于它的动作（ActionRunner.ClearMission，取消状态），再交给观察者（Observer.OnMissionEnd）；
//   - 动作归属任务：任务回调运行期间（含它同步引出的回调）经 CreateAction / EnqueueAction 发起的动作记在当前任务
//     名下（ssr HasOnProcessTag 的规则），任务结束时一并结束；其余调用方（AI、handler）发起的动作不属于任何任务；
//   - 清场（Clear）：先结束当前任务，再结束全部动作（包说明“清场顺序”）；
//   - 任务心跳（D21 A6，配置了 Beat 时）：当前任务实现 TickIntervaler 且返回正数时，Behavior 给它登记
//     "<Beat.Name>/mission"，周期取不大于它的最长档，每拍 MissionRunner.Tick(本拍时间)；任务结束或不再需要时停。
//     挂不上（Remote 共享模式）时任务以失败结束。不实现 TickIntervaler 的任务不被周期 tick（由动作结束推进）。
//
// 方法与 ActionRunner 一样只能在实体锁内调用（Nest handler、心跳回调、加载 / 新建的初始化阶段）；Behavior 本身无锁、
// 不随事务回滚（D20 决定 3：由业务保证，handler 最后一步才调用）。
type Behavior struct {
	owner    entity.IThreadSafeEntity
	runner   *ActionRunner
	beat     *BeatRunner
	missions *MissionRunner
	observer Observer
	now      func() time.Time
	onError  func(error)

	// 任务心跳（配置了 Beat 时）：host / name 是登记用的宿主与名字，mission 第一次需要时登记，之后保留。
	host        *heartbeat.Host
	beatName    string
	beatOptions []heartbeat.RegOption
	mission     *heartbeat.Reg

	// closed 由 Close / OnDestroy 置位：经 Behavior 发起的动作与任务返回 ErrBehaviorClosed。
	closed bool

	// frozen 是各组按 Behavior 时钟累计的冻结时长（FrozenFor），在 runner 的 OnFreeze 里更新；只有冻结过的组有条目。
	frozen map[ActionGroup]*frozenClock
}

// frozenClock 是一个组的冻结计时：total 是已结束的冻结累计，since 是本次冻结的起点（frozen 为真时有效）。
type frozenClock struct {
	total  time.Duration
	since  time.Time
	frozen bool
}

// Observer 接收 Behavior 的动作结束与任务结束，在任务处理之后调用。ai.Controller 实现它（actionflow 不导入 ai）。
type Observer interface {
	OnActionEnd(id int64, kind ActionKind, reason ActionReason)
	OnMissionEnd(mission Mission, reason ActionReason)
}

// BehaviorConfig 是 NewBehavior 的配置。
type BehaviorConfig struct {
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
	// Now 是业务时钟，nil 时取本次执行冻结的业务时间 fctx.Now（D22-1）；Beat.TimeDomain 非 nil 时取域时间
	// （Domain.Now）。动作与任务的上下文时间、计划步骤 Timeout 的起点都按它。
	Now func() time.Time
	// OnError 接收 runner 与任务的错误（回调 panic、延后命令失败等），可空。
	OnError func(error)
	// MaxDeferredCommands / MaxDeferredSteps 同 ActionRunnerConfig / MissionRunnerConfig，两个 runner 共用。
	MaxDeferredCommands int
	MaxDeferredSteps    int
	// Metrics 为 true 时写动作指标（MetricActionsStarted / Ended / Running）。每个动作开始、结束各写一次注册表，
	// 缺省关闭（D21 A9，是否默认开启待维护者决定）。
	Metrics bool
}

// NewBehavior 为 owner 构造并接好 ActionRunner（或 BeatRunner）与 MissionRunner。
func NewBehavior(owner entity.IThreadSafeEntity, config BehaviorConfig) (*Behavior, error) {
	if owner == nil {
		return nil, errors.New("actionflow: behavior needs an owner entity")
	}
	if config.Beat != nil && config.Beat.Host != nil && config.Beat.Host.Owner() != owner {
		return nil, errors.New("actionflow: behavior beat host belongs to another entity")
	}
	b := &Behavior{owner: owner, observer: config.Observer, now: config.Now, onError: config.OnError}
	if config.Beat != nil {
		b.host, b.beatName, b.beatOptions = config.Beat.Host, config.Beat.Name, config.Beat.registrationOptions()
		if b.beatName == "" {
			b.beatName = DefaultBeatName
		}
	}
	if b.now == nil {
		b.now = fctx.Now
		if config.Beat != nil && config.Beat.TimeDomain != nil {
			b.now = config.Beat.TimeDomain.Now
		}
	}
	runnerConfig := ActionRunnerConfig{
		Registry:     config.Registry,
		GroupForKind: config.GroupForKind,
		Conflict:     config.Conflict,
		Hooks: ActionRunnerHooks{
			PopulateContext: b.populate,
			OnEnded:         b.actionEnded,
			OnFreeze:        b.freezeChanged,
			OnError:         b.report,
		},
		MaxDeferredCommands: config.MaxDeferredCommands,
		MaxDeferredSteps:    config.MaxDeferredSteps,
	}
	if config.Metrics {
		runnerConfig.Hooks.OnTransition = countTransition
		runnerConfig.Hooks.OnEnded = func(s ActionSnapshot, reason ActionReason) {
			metrics.IncCounter(MetricActionsEnded, metrics.Labels{"kind": strconv.Itoa(int(s.Kind)),
				"status": reason.ToActionResult(ActionStatusSuccess).Status.String()}, 1)
			b.actionEnded(s, reason)
		}
	}
	var err error
	if config.Beat != nil {
		b.beat, err = NewBeatRunner(runnerConfig, *config.Beat)
		if b.beat != nil {
			b.runner = b.beat.Runner()
		}
	} else {
		b.runner, err = NewActionRunner(runnerConfig)
	}
	if err != nil {
		return nil, err
	}
	if b.beat != nil {
		b.beat.syncExtra = b.resyncMissionBeat
	}
	b.missions, err = NewMissionRunner(MissionRunnerConfig{
		Registry:    config.Registry,
		DefaultKind: config.DefaultMission,
		Hooks: MissionRunnerHooks{
			Now:          b.now,
			Context:      b.missionContext,
			ClearActions: b.clearMissionActions,
			OnState:      func(bool) { b.syncMissionBeat() },
			OnChanged:    func(MissionInfo) { b.syncMissionBeat() },
			OnEnded:      b.missionEnded,
			OnError:      b.report,
		},
		MaxDeferredCommands: config.MaxDeferredCommands,
		MaxDeferredSteps:    config.MaxDeferredSteps,
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// MissionReg 返回任务心跳的登记；从没有任务需要周期 tick、或没有配置 Beat 时为 nil（诊断、测试用，启停由 Behavior 管理）。
func (b *Behavior) MissionReg() *heartbeat.Reg { return b.mission }

// Runner 返回动作 runner；Beat 返回心跳驱动的宿主（未配置 Beat 时为 nil）；Missions 返回任务 runner。
func (b *Behavior) Runner() *ActionRunner    { return b.runner }
func (b *Behavior) Beat() *BeatRunner        { return b.beat }
func (b *Behavior) Missions() *MissionRunner { return b.missions }

// Clear 清场：先结束当前任务（之后送来的动作结束不再推进任务），再强制结束全部动作、丢弃排队项。
func (b *Behavior) Clear(reason ActionReason) {
	b.missions.EndCurMission(reason)
	b.report(b.runner.EndAll(true, reason))
}

// Close 撤销 Behavior：先拒绝新的动作与任务（观察者、任务在收尾通知里再发起的也被拒绝，返回 ErrBehaviorClosed），
// 再清场（Clear：任务与动作以 reason 结束，OnEnded / 观察者照常送达，动作指标的 running 随之减回），最后撤销任务心跳与
// 各组的心跳登记（heartbeat.Reg.Close：立即离开驱动、释放登记名）。成员换场景（换时间域）时用它撤掉旧的、在同一心跳
// 宿主上按新的 BeatConfig.TimeDomain 重建（DESIGN-TODO“D20 与 D21 审查修复”：重建而不是 SetTimeDomain）。可重复调用；只能在
// 实体锁内调用，在 runner 或任务的回调里调用时结束延后到回调返回后。直接经 Runner() 发起的动作不受 Behavior 的拒绝
// 约束，但挂不上心跳（ErrBeatRunnerClosed）。
func (b *Behavior) Close(reason ActionReason) {
	if b.closed {
		return
	}
	b.closed = true
	b.Clear(reason)
	if b.mission != nil {
		b.mission.Close()
		b.mission = nil
	}
	if b.beat != nil {
		b.beat.closeRegs()
	}
}

// OnDestroy 由实体的 OnDestroy 调用（卸载、删档、撤销新建；组件按装配顺序收尾，心跳宿主最后 Host.OnDestroy）：以
// ErrOwnerDestroyed（取消状态）Close，在途动作与任务各收到一次结束，开了 Metrics 时 running 减回。不在事务里执行：
// 动作的 Cancel 与观察者这时改的持久字段不落库（与卸载丢弃未提交的内存改动相同），只做内存收尾。实体移除之后不再
// 有别的消息执行，所以这里虽在实体锁外也不与 handler 并发。
func (b *Behavior) OnDestroy() {
	b.Close(ActionReason{Message: "owner destroyed", Err: ErrOwnerDestroyed,
		Result: ActionResult{Status: ActionStatusCanceled, Reason: "owner destroyed"}})
}

// Closed 报告是否已 Close / OnDestroy。
func (b *Behavior) Closed() bool { return b.closed }

// --- ActionList ---

func (b *Behavior) Entity() entity.IThreadSafeEntity { return b.owner }

// CreateAction 启动动作，取代同组当前动作（Replacer / Conflict 规则见 ActionRunner）。
func (b *Behavior) CreateAction(kind ActionKind, param any) (int64, error) {
	if b.closed {
		return 0, ErrBehaviorClosed
	}
	return b.runner.Start(kind, param, b.missionTag(), b.now())
}

// EnqueueAction 把动作排到组队列末尾。
func (b *Behavior) EnqueueAction(kind ActionKind, param any) (int64, error) {
	if b.closed {
		return 0, ErrBehaviorClosed
	}
	return b.runner.Enqueue(kind, param, b.missionTag())
}

func (b *Behavior) CurAction(group ActionGroup) Action { return b.runner.Current(group) }

func (b *Behavior) EndCurAction(group ActionGroup, force bool, reason ActionReason) {
	b.report(b.runner.End(group, force, reason))
}

func (b *Behavior) EndAction(id int64, force bool, reason ActionReason) error {
	return b.runner.EndAction(id, force, reason)
}

func (b *Behavior) EndAllAction(force bool, reason ActionReason) {
	b.report(b.runner.EndAll(force, reason))
}

func (b *Behavior) ClearNextActions() { b.runner.ClearQueue() }

func (b *Behavior) UpdateAction(group ActionGroup, f func(Action) error) error {
	return b.runner.Update(group, f)
}

func (b *Behavior) Freeze(group ActionGroup, mode FreezeMode) error {
	return b.runner.Freeze(group, mode)
}
func (b *Behavior) Recover(group ActionGroup) error { return b.runner.Recover(group, b.now()) }
func (b *Behavior) Frozen(group ActionGroup) bool   { return b.runner.Frozen(group) }

func (b *Behavior) MissionManager() MissionManager { return b.missions }

// FrozenFor 实现 FrozenTimer：kind 所在的组截至 now（Behavior 的时钟）累计被冻结的时长。种类不属于任何组时为 0。
func (b *Behavior) FrozenFor(kind ActionKind, now time.Time) time.Duration {
	group, ok, err := b.runner.resolveGroup(kind)
	if err != nil || !ok {
		return 0
	}
	c := b.frozen[group]
	if c == nil {
		return 0
	}
	total := c.total
	if c.frozen && now.After(c.since) {
		total += now.Sub(c.since)
	}
	return total
}

func (b *Behavior) SetMission(kind MissionKind, param any) error {
	if b.closed {
		return ErrBehaviorClosed
	}
	return b.missions.StartMission(kind, param)
}

func (b *Behavior) CurMission() Mission { return b.missions.CurMission() }

func (b *Behavior) EndCurMission(reason ActionReason) { b.missions.EndCurMission(reason) }

// --- 接线 ---

// freezeChanged 是 runner 的 OnFreeze（冻结计数 0→1、→0 时调用）：按 Behavior 的时钟记下冻结的起止，供 FrozenFor。
// Freeze 的计数立即生效、钩子当场调用；Recover 在回调里延后时，钩子在延后命令执行时调用，时钟仍是同一次执行的时间。
func (b *Behavior) freezeChanged(group ActionGroup, frozen bool) {
	if b.frozen == nil {
		b.frozen = make(map[ActionGroup]*frozenClock)
	}
	c := b.frozen[group]
	if c == nil {
		c = &frozenClock{}
		b.frozen[group] = c
	}
	now := b.now()
	switch {
	case frozen && !c.frozen:
		c.since, c.frozen = now, true
	case !frozen && c.frozen:
		if now.After(c.since) {
			c.total += now.Sub(c.since)
		}
		c.frozen = false
	}
}

// countTransition 记动作成为 / 不再是组的当前动作（打开指标时的 OnTransition）。
func countTransition(s ActionSnapshot, entering bool) {
	if entering {
		metrics.IncCounter(MetricActionsStarted, metrics.Labels{"kind": strconv.Itoa(int(s.Kind))}, 1)
		metrics.AddGauge(MetricActionsRunning, nil, 1)
		return
	}
	metrics.AddGauge(MetricActionsRunning, nil, -1)
}

// missionTag 是新动作的归属任务：任务回调运行期间（MissionRunner 正在执行）发起的动作属于当前任务。
func (b *Behavior) missionTag() int64 {
	if b.missions.Deferring() {
		return b.missions.CurrentMissionID()
	}
	return 0
}

func (b *Behavior) populate(ctx *ActionContext, _ time.Time) {
	ctx.Owner = b.owner
	ctx.ActionList = b
	ctx.CurrentMission = b.missions.CurMission()
}

func (b *Behavior) missionContext(now time.Time) *MissionContext {
	return &MissionContext{Owner: b.owner, ActionList: b, Now: now}
}

// actionEnded：先任务、后观察者。
func (b *Behavior) actionEnded(s ActionSnapshot, reason ActionReason) {
	b.missions.OnActionEnd(s.ID, s.Kind, reason)
	if b.observer != nil {
		b.observer.OnActionEnd(s.ID, s.Kind, reason)
	}
}

func (b *Behavior) clearMissionActions(missionID int64, reason ActionReason) error {
	return b.runner.ClearMission(missionID, true, reason)
}

func (b *Behavior) missionEnded(m Mission, reason ActionReason) {
	if b.observer != nil {
		b.observer.OnMissionEnd(m, reason)
	}
}

// syncMissionBeat 让任务心跳与当前任务一致（MissionRunner 的 OnState / OnChanged 里调用：任务开始前后、每次推进、
// 结束时）：有正在运行、要周期 tick 的任务就按它的周期启动，否则停。挂不上时以失败结束任务（在任务 runner 的回调里，
// EndCurMission 延后到回调返回后执行）。
func (b *Behavior) syncMissionBeat() {
	if b.host == nil {
		return
	}
	interval := time.Duration(0)
	if m := b.missions.CurMission(); m != nil && m.Status() == MissionStatusRunning {
		interval = missionPeriod(m)
	}
	if interval == 0 {
		if b.mission != nil {
			b.mission.Stop()
		}
		return
	}
	if b.closed {
		// 已撤销（Close 前已排进任务 runner 延后队列的 StartMission）：没有心跳，任务不会被周期推进。
		b.failMission(ErrBehaviorClosed)
		return
	}
	if b.mission == nil {
		reg, err := b.host.Register(b.beatName+"/mission", interval, b.missionBeat, b.beatOptions...)
		if err != nil {
			b.failMission(err)
			return
		}
		b.mission = reg
	}
	if b.mission.Period() != interval {
		if err := b.mission.SetPeriod(interval); err != nil {
			b.failMission(err)
			return
		}
	}
	if err := b.mission.Start(); err != nil {
		b.failMission(err)
	}
}

// resyncMissionBeat 是 BeatRunner.Sync 里的任务心跳对齐（Remote 托管实体回到独占之后，业务经 Beat().Sync() 调用）：
// 运行中进入共享模式时框架停掉了任务心跳，任务留在 runner 里不被 tick（步骤时限不检查）。当前任务在运行、需要周期
// tick 而心跳没在跳时按它的周期重新启动；挂不上只返回错误，不结束任务（与动作组的 Sync 相同），不同于任务推进时的
// syncMissionBeat（那时挂不上以失败结束任务）。
func (b *Behavior) resyncMissionBeat() error {
	m := b.missions.CurMission()
	if b.mission == nil || b.mission.Active() || m == nil || m.Status() != MissionStatusRunning {
		return nil
	}
	interval := missionPeriod(m)
	if interval == 0 {
		return nil
	}
	if b.mission.Period() != interval {
		if err := b.mission.SetPeriod(interval); err != nil {
			return fmt.Errorf("actionflow: mission heartbeat: %w", err)
		}
	}
	if err := b.mission.Start(); err != nil {
		return fmt.Errorf("actionflow: mission heartbeat: %w", err)
	}
	return nil
}

func (b *Behavior) failMission(err error) {
	err = fmt.Errorf("actionflow: mission heartbeat: %w", err)
	b.report(err)
	b.missions.EndCurMission(NewActionErrorReason(err))
}

// missionBeat 是任务心跳的回调：tick 当前任务，时间取本拍的业务时间。
func (b *Behavior) missionBeat(beat heartbeat.Beat) error {
	if b.missions.CurMission() == nil {
		return nil
	}
	b.missions.Tick(beat.Now)
	return nil
}

// missionPeriod 把任务要的间隔换成心跳档（不实现 TickIntervaler 或返回 0：不需要周期 tick）。
func missionPeriod(m Mission) time.Duration {
	ti, ok := m.(TickIntervaler)
	if !ok {
		return 0
	}
	return heartbeat.PeriodAtMost(ti.TickInterval())
}

func (b *Behavior) report(err error) {
	if err != nil && b.onError != nil {
		defer func() { _ = recover() }()
		b.onError(err)
	}
}

var (
	_ ActionList  = (*Behavior)(nil)
	_ FrozenTimer = (*Behavior)(nil)
)
