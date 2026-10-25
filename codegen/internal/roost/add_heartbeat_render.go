package roost

import (
	"strconv"
	"strings"
)

// 心跳 / 行为 / 时间域的生成模板（add_heartbeat.go）。占位符：@Entity@（Monster）、@entity@（monster，包名与目录）、
// @Module@、@Project@、@HeartbeatID@、@BehaviorID@、@TimeDomainID@、@Dao@、@DaoGetter@、@StartHandler@、@StrategyHandler@。

func (n heartbeatNames) replacer(extra ...string) *strings.Replacer {
	pairs := []string{
		"@Entity@", n.EntityType, "@entity@", n.Entity, "@Module@", n.Module, "@Project@", n.Project,
		"@StartHandler@", n.startHandler(), "@StrategyHandler@", n.strategyHandler(),
		"@HeartbeatDoc@", heartbeatDocURL, "@BehaviorDoc@", behaviorDocURL, "@TimeDomainDoc@", timeDomainDocURL,
	}
	return strings.NewReplacer(append(pairs, extra...)...)
}

func renderHeartbeatDriver(m Manifest) string {
	return generatedHeader + strings.NewReplacer("@Project@", m.Project.Name, "@HeartbeatDoc@", heartbeatDocURL).Replace(`
// roost add heartbeat / roost add behavior（或 roost add mod heartbeat）生成，project sync 重写。
// 运行期在 core 的 gameplay/heartbeat：@HeartbeatDoc@
package heartbeat

import coreheartbeat "github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"

// Driver 是本进程唯一的实体心跳驱动（一个 Nest 引擎一个驱动）。每个到期的登记由它投一条单目标 Nest 消息，回调在
// 实体自己的 Nest worker 上、实体锁与事务内执行；没有登记、或登记都停着的实体不占调度。
//
// 生成的 bootstrap 在装了 heartbeat Mod 的服务上用 wiring/heartbeat 把它接到 Nest（heartbeat.* 配置写在那个服务的
// 配置里）；实体组件经 Driver.NewHost 取宿主（roost add heartbeat 生成的 HeartbeatComponent）。业务时间缺省读
// fctx.Now（消息冻结的进程业务时钟，App 启动时按 time.logic_offset 设定）。
var Driver = coreheartbeat.NewDriver(coreheartbeat.Config{Name: "@Project@"})
`)
}

func renderHeartbeatComponent(n heartbeatNames, spec HeartbeatSpec) string {
	return generatedHeader + n.replacer("@HeartbeatID@", strconv.FormatInt(spec.ID, 10)).Replace(`
// roost add heartbeat --entity @Entity@ 生成，project sync 重写；本实体自己的心跳登记写在 heartbeat_beats.go（业务文件）。
// 运行期在 core 的 gameplay/heartbeat：@HeartbeatDoc@
package @entity@

import (
	"fmt"
	"time"

	gameheartbeat "@Module@/game/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

//roost:component type=@HeartbeatID@
const CompTypeHeartbeat entity.ComponentType = @HeartbeatID@

// HeartbeatComponent 持有 @Entity@ 实例的心跳宿主（gameplay/heartbeat.Host），同一实体的其他组件（行为组件、AI、
// 业务自己的周期逻辑）共用它登记心跳。
//
//   - 心跳不持久：每次加载 / 新建后在 OnInitFinish（业务的 onHeartbeatInit）按自己的持久状态重新登记、启动；
//   - 回调在实体自己的 Nest worker 上、实体锁与事务内执行，只做内存计算与入队，不阻塞、不做 I/O、不 Dispatch / Request；
//     返回错误时本拍事务回滚；
//   - 每个登记至多一拍在途，上一拍没执行完就跳拍（不追帧），Beat.Elapsed 给出距上次执行的业务时间；
//   - 没有登记、或登记都停着的实例不占调度；卸载 / 删档时 OnDestroy 让实例永久离开驱动。
//
// 方法只能在持有实体锁时调用（Nest handler、心跳回调、加载 / 新建的初始化阶段）。
type HeartbeatComponent struct {
	entity.ComponentBase
	owner *@Entity@
	host  *heartbeat.Host
	beats map[string]*heartbeat.Reg
}

// IHeartbeatEntity 是 Nest handler 使用本组件的锁安全视图。
type IHeartbeatEntity interface {
	entity.IThreadSafeEntity
	HeartbeatComp() *HeartbeatComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeHeartbeat, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*@Entity@)
		if !ok {
			return nil, fmt.Errorf("heartbeat component: owner %T is not *@Entity@", owner)
		}
		// 宿主在构造时就建好：同一实体其他组件的 OnInitFinish 不论先后都能取到它。
		return &HeartbeatComponent{owner: typed, host: gameheartbeat.Driver.NewHost(typed)}, nil
	})
}

func (component *HeartbeatComponent) Name() string { return "heartbeat" }

// Owner 只在实体存活且被 Nest 加锁时可用。
func (component *HeartbeatComponent) Owner() *@Entity@ { return component.owner }

// Host 是本实例的心跳宿主（行为组件、AI 用它登记各自的心跳）。
func (component *HeartbeatComponent) Host() *heartbeat.Host { return component.host }

// OnInitFinish：实例发布之前交给业务登记心跳（heartbeat_beats.go 的 onHeartbeatInit）。
func (component *HeartbeatComponent) OnInitFinish(_ *entity.EntityCreateParam, isCreate bool) error {
	return component.onHeartbeatInit(isCreate)
}

// OnDestroy：卸载、删档或撤销新建时，实例的全部心跳登记永久离开驱动，之后的启停被忽略。
func (component *HeartbeatComponent) OnDestroy(entity.EntityDestroyReason) { component.host.OnDestroy() }

// Register 登记一个心跳，返回停止状态的登记（Start 之后才跳），之后可以用 Beat(name) 取回。name 在实例内唯一；
// period 必须是 heartbeat.Periods 之一（50ms～60s 十个档）。Remote 托管实体处于共享模式时返回
// heartbeat.ErrHeartbeatRemoteShared。
func (component *HeartbeatComponent) Register(name string, period time.Duration, fn heartbeat.Func, opts ...heartbeat.RegOption) (*heartbeat.Reg, error) {
	reg, err := component.host.Register(name, period, fn, opts...)
	if err != nil {
		return nil, err
	}
	if component.beats == nil {
		component.beats = make(map[string]*heartbeat.Reg)
	}
	component.beats[name] = reg
	return reg, nil
}

// Beat 返回经 Register 登记的心跳；没有时为 nil。在事务里 Start / Stop / SetPeriod 时提交后生效、回滚作废。
func (component *HeartbeatComponent) Beat(name string) *heartbeat.Reg { return component.beats[name] }
`)
}

func renderHeartbeatBeats(n heartbeatNames) string {
	return n.replacer().Replace(`package @entity@

import (
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 这个文件归业务：roost add heartbeat 只在缺失时创建，project sync 不改写。
//
// 心跳适合在线的周期推进：回复、冷却、周期效果、场景 / 房间自己的逻辑帧。持久、准点、离线也要算的事（Buff 到期写档、
// 建造完成）用 roost add timer。控制实体行为的逻辑（移动、技能、AI 决策）不在这里直接挂心跳，写成动作交给行为组件
// （roost add behavior）。

// onHeartbeatInit 在实例加载 / 新建、发布之前调用：在这里登记本实体自己的周期逻辑，按 DAO 里的持久状态决定是否启动
// （心跳不持久，每次加载都要重新登记）。返回错误时实体初始化失败。
func (component *HeartbeatComponent) onHeartbeatInit(isCreate bool) error {
	_ = isCreate
	// 示例：一个 1 秒档的登记，停止状态（不占调度）。需要时在 Nest handler 里
	// target.HeartbeatComp().Beat("example").Start()，不需要时 Stop()。
	_, err := component.Register("example", time.Second, component.onExampleBeat)
	if errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) {
		return nil // Remote 托管实体处于共享模式时不允许挂心跳；周期逻辑由负责它的服务驱动
	}
	return err
}

// onExampleBeat 是示例心跳的回调：实体锁与事务内。按 beat.Elapsed（距上次执行的业务时间）推进，不按“第几拍”算；
// beat.Now 是这一拍冻结的业务时间。
func (component *HeartbeatComponent) onExampleBeat(beat heartbeat.Beat) error {
	_ = beat
	return nil
}
`)
}

func renderBehaviorComponent(n heartbeatNames, spec BehaviorSpec) string {
	aiImport, aiField, aiBuild, aiAccessor, observer, aiClose := "", "", "", "", "", ""
	if spec.AI {
		aiImport = "\n\t\"github.com/tjbdwanghaibo/roost-core/gameplay/ai\""
		aiField = "\n\t// ai 是 AI 决策（ai.NewBeatController）：独立的 \"ai\" 心跳，周期由当前策略决定；没有策略时不跳。\n\tai *ai.Controller"
		aiBuild = `
	component.ai, err = ai.NewBeatController(ai.ControllerHooks{
		Context: func(now time.Time) *ai.Context {
			return &ai.Context{Owner: component.owner, ActionList: component.behavior, Now: now}
		},
		// 切换策略时结束在途动作（新策略从空场开始）。
		EndActions: func(reason actionflow.ActionReason) { component.behavior.EndAllAction(true, reason) },
		OnError:    component.onBehaviorError,
	}, ai.BeatConfig{Host: host, TimeDomain: domain})
	if err != nil {
		return fmt.Errorf("behavior component: %w", err)
	}`
		aiAccessor = `

// AI 是本实例的 AI 决策；SetStrategy 设定策略后 AI 心跳才开始跳（behavior_strategy.go 有示例策略）。
func (component *BehaviorComponent) AI() *ai.Controller { return component.ai }`
		observer = "\n\t\tObserver: component.ai, // 动作 / 任务结束先交给任务，再交给 AI"
		aiClose = `
	if component.ai != nil {
		component.ai.Close("owner destroyed") // 先撤下 AI：卸下策略、撤销 "ai" 心跳，不再收结束通知、不再决策
	}`
	}
	timeImport := ""
	if spec.AI {
		timeImport = "\n\t\"time\""
	}
	return generatedHeader + n.replacer("@BehaviorID@", strconv.FormatInt(spec.ID, 10), "@AIImport@", aiImport, "@AIField@", aiField,
		"@AIBuild@", aiBuild, "@AIAccessor@", aiAccessor, "@Observer@", observer, "@TimeImport@", timeImport, "@AIClose@", aiClose).Replace(`
// roost add behavior --entity @Entity@ 生成，project sync 重写；动作种类、组、注册表与示例动作在 behavior_actions.go
// （业务文件）。运行期在 core 的 gameplay/actionflow：@BehaviorDoc@
package @entity@

import (
	"fmt"@TimeImport@

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"@AIImport@
)

//roost:component type=@BehaviorID@
const CompTypeBehavior entity.ComponentType = @BehaviorID@

// BehaviorComponent 是 @Entity@ 的行为组件：actionflow.Behavior（ActionRunner + MissionRunner）挂在实体的心跳宿主上，
// 每个动作组的当前动作按它的 TickInterval 由这个组自己的心跳 tick，组空闲时停。所有控制实体行为的逻辑（移动、技能、
// AI 决策）都经动作下发与收尾。
//
//   - 只在实体锁内调用（Nest handler、心跳回调、加载 / 新建的初始化阶段）；
//   - runner 是纯内存状态，不随 Nest 事务回滚（由业务保证）：handler 先做完全部可能失败的校验与修改，
//     最后一步才 CreateAction / EnqueueAction / EndAction；
//   - 不持久：实例重新加载后由业务按自己的 DAO 状态重新发起动作（onBehaviorReady）。
type BehaviorComponent struct {
	entity.ComponentBase
	owner    *@Entity@
	behavior *actionflow.Behavior@AIField@
}

// IBehaviorEntity 是 Nest handler 使用本组件的锁安全视图。
type IBehaviorEntity interface {
	entity.IThreadSafeEntity
	BehaviorComp() *BehaviorComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeBehavior, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*@Entity@)
		if !ok {
			return nil, fmt.Errorf("behavior component: owner %T is not *@Entity@", owner)
		}
		return &BehaviorComponent{owner: typed}, nil
	})
}

func (component *BehaviorComponent) Name() string { return "behavior" }

// Owner 只在实体存活且被 Nest 加锁时可用。
func (component *BehaviorComponent) Owner() *@Entity@ { return component.owner }

// Behavior 是本实例的动作列表：CreateAction / EnqueueAction / EndAction / Freeze / Recover / SetMission。
func (component *BehaviorComponent) Behavior() *actionflow.Behavior { return component.behavior }@AIAccessor@

// OnInitFinish：构造行为组件，接上实体的心跳宿主（HeartbeatComponent）与业务给的时间域（timeDomain），再交给业务的
// onBehaviorReady。构造本身不登记心跳：组第一次有需要 tick 的动作时才登记。
func (component *BehaviorComponent) OnInitFinish(_ *entity.EntityCreateParam, isCreate bool) error {
	host := component.owner.HeartbeatComp().Host()
	domain := component.timeDomain()
	var err error@AIBuild@
	component.behavior, err = actionflow.NewBehavior(component.owner, actionflow.BehaviorConfig{
		Registry:     behaviorRegistry,
		GroupForKind: behaviorGroupForKind,
		Conflict:     behaviorConflict,
		Beat:         &actionflow.BeatConfig{Host: host, TimeDomain: domain},@Observer@
		OnError:      component.onBehaviorError,
	})
	if err != nil {
		return fmt.Errorf("behavior component: %w", err)
	}
	return component.onBehaviorReady(isCreate)
}

// OnDestroy：卸载、删档或撤销新建时收尾：在途动作与任务以 actionflow.ErrOwnerDestroyed 结束（取消状态，按 ID 等结果的
// 一方收到结论，开了动作指标时 running 减回），之后经它发起的动作被拒绝；心跳登记随 HeartbeatComponent 的
// OnDestroy 离开驱动。不在事务里：这时的改动只做内存收尾、不落库。
func (component *BehaviorComponent) OnDestroy(entity.EntityDestroyReason) {@AIClose@
	if component.behavior != nil {
		component.behavior.OnDestroy()
	}
}
`)
}

func renderBehaviorActions(n heartbeatNames, spec BehaviorSpec) string {
	ready := `
// onBehaviorReady 在行为组件构造之后、实例发布之前调用（加载 / 新建的初始化阶段，实体锁内）：按 DAO 里的持久状态
// 重新发起需要的动作（行为不持久）。返回错误时实体初始化失败。
func (component *BehaviorComponent) onBehaviorReady(isCreate bool) error {
	_ = isCreate
	return nil
}`
	if spec.AI {
		ready = `
// onBehaviorReady 在行为组件与 AI 构造之后、实例发布之前调用（加载 / 新建的初始化阶段，实体锁内）：按 DAO 里的持久
// 状态重新发起需要的动作（行为不持久）。AI 要随实体一起启动时在这里设定策略，例如
// return component.ai.SetStrategy(NewExampleStrategy())；不设定时 AI 不占调度。返回错误时实体初始化失败。
func (component *BehaviorComponent) onBehaviorReady(isCreate bool) error {
	_ = isCreate
	return nil
}`
	}
	return n.replacer("@Ready@", ready).Replace(`package @entity@

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
)

// 这个文件归业务：roost add behavior 只在缺失时创建，project sync 不改写。用法见 @BehaviorDoc@

// @Entity@ 的动作种类与动作组。同组同时只有一个当前动作（新的取代旧的，当前动作可实现 actionflow.Replacer 改规则），
// 不同组并行、各挂各的心跳。种类与组号只在进程内使用（行为不持久），改号不影响存档。
const (
	// ActionExample 是示例动作（ExampleAction），换成自己的种类（从 1 开始）。
	ActionExample actionflow.ActionKind = 1

	// GroupMain 是示例动作所在的组。
	GroupMain actionflow.ActionGroup = 1
)

// behaviorRegistry 是 @Entity@ 全部动作 / 任务的构造器：包初始化时登记并封存，之后不能再登记。
var behaviorRegistry = newBehaviorRegistry()

func newBehaviorRegistry() *actionflow.Registry {
	registry := actionflow.NewRegistry()
	if err := registry.RegisterAction(ActionExample, NewExampleAction); err != nil {
		panic(err)
	}
	registry.Seal()
	return registry
}

// behaviorGroupForKind 把动作种类映射到组；不认识的种类返回 false（Start 拒绝）。
func behaviorGroupForKind(kind actionflow.ActionKind) (actionflow.ActionGroup, bool) {
	switch kind {
	case ActionExample:
		return GroupMain, true
	}
	return 0, false
}

// behaviorConflict 是跨组冲突规则：新动作要启动时，对其他每个有当前动作的组调用一次。NoConflict 互不影响；
// BlockIncoming 拒绝新动作；PreemptRunning 先取消那一组的当前动作。
func behaviorConflict(running, incoming actionflow.ActionSnapshot) actionflow.Conflict {
	_, _ = running, incoming
	return actionflow.NoConflict
}

// timeDomain 是本实体所在场景的时间域（roost add timedomain），nil 表示按业务时间（fctx.Now）计时。域在构造时
// 定下、运行中不能换：实体要换场景时重建行为组件，或不挂域、由业务自己传时间。
func (component *BehaviorComponent) timeDomain() *timedomain.Domain { return nil }
@Ready@

// onBehaviorError 接收行为组件与 AI 的错误（回调 panic、延后命令失败、心跳挂不上等）。
func (component *BehaviorComponent) onBehaviorError(err error) {
	slog.Warn("behavior", "entity", component.owner.ID(), "err", err)
}

// ExampleParam 是 ActionExample 的参数：持续 Duration（业务时间；挂了时间域时是域时间）。
type ExampleParam struct {
	Duration time.Duration
}

// ExampleAction 是示例动作：每拍按 ctx.Elapsed 累计，满 Duration 成功结束。它演示动作的三件事：Start 校验（返回错误 =
// 没能启动）、Tick 按经过时间推进（不按“第几拍”，掉拍时下一拍一次覆盖）、Cancel 收尾。动作自己的状态只在内存里；
// 要改实体状态，经 ctx.Owner 取到实体、写 DAO（随这一拍的事务提交或回滚）。
type ExampleAction struct {
	param  ExampleParam
	waited time.Duration
	ticks  int
}

// NewExampleAction 是 ActionExample 的构造器（behaviorRegistry 登记）。
func NewExampleAction(param any) (actionflow.Action, error) {
	p, ok := param.(ExampleParam)
	if !ok {
		return nil, fmt.Errorf("example action: param %T is not ExampleParam", param)
	}
	if p.Duration <= 0 {
		return nil, errors.New("example action: duration must be positive")
	}
	return &ExampleAction{param: p}, nil
}

func (a *ExampleAction) Kind() actionflow.ActionKind { return ActionExample }

// TickInterval 是本组心跳的周期（取不大于它的档位）；不实现取 100ms，返回 0 表示不需要周期 tick。
func (a *ExampleAction) TickInterval() time.Duration { return 100 * time.Millisecond }

func (a *ExampleAction) Start(*actionflow.ActionContext) (bool, actionflow.ActionResult, error) {
	return false, actionflow.ActionResult{}, nil
}

func (a *ExampleAction) Tick(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult) {
	a.ticks++
	a.waited += ctx.Elapsed
	if a.waited >= a.param.Duration {
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusSuccess, Reason: "example done"}
	}
	return false, actionflow.ActionResult{}
}

func (a *ExampleAction) Cancel(*actionflow.ActionContext, string) {}

// Ticks 与 Waited 是示例的进度（实体锁内读）：被心跳 tick 的次数与累计的经过时间。
func (a *ExampleAction) Ticks() int            { return a.ticks }
func (a *ExampleAction) Waited() time.Duration { return a.waited }
`)
}

func renderBehaviorStrategy(n heartbeatNames) string {
	return n.replacer().Replace(`package @entity@

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai"
)

// 这个文件归业务：roost add behavior --ai 只在缺失时创建，project sync 不改写。

// ExampleStrategy 是示例 AI 策略：每次决策看主组是否空闲，空闲就发起一个 ExampleAction。决策在 AI 自己的心跳（"ai"）
// 上、实体锁与事务内执行；它下发的动作由动作组的心跳推进。复杂决策用行为树（ai.NewBehaviorStrategy / ai.ParseTree）。
type ExampleStrategy struct {
	started int
}

func NewExampleStrategy() *ExampleStrategy { return &ExampleStrategy{} }

func (s *ExampleStrategy) Name() string { return "example" }

// Init 里不要发起动作（切换策略先 Init、成功后才结束现有动作）；在第一次 Tick 里发起。
func (s *ExampleStrategy) Init(*ai.Context) error { return nil }

// TickInterval 是决策周期（取不大于它的心跳档）；不实现取 500ms，返回 0 表示只靠动作 / 任务结束驱动、不周期决策。
func (s *ExampleStrategy) TickInterval() time.Duration { return 500 * time.Millisecond }

func (s *ExampleStrategy) Tick(ctx *ai.Context, now time.Time) {
	_ = now
	if ctx.ActionList == nil || ctx.ActionList.CurAction(GroupMain) != nil {
		return
	}
	if _, err := ctx.ActionList.CreateAction(ActionExample, ExampleParam{Duration: time.Second}); err == nil {
		s.started++
	}
}

func (s *ExampleStrategy) OnActionEnd(*ai.Context, int64, actionflow.ActionKind, actionflow.ActionReason) {}

func (s *ExampleStrategy) OnMissionEnd(*ai.Context, actionflow.Mission, actionflow.ActionReason) {}

func (s *ExampleStrategy) CanStopByNext(ai.Strategy) bool { return true }

// Started 是这个策略发起过的示例动作数（实体锁内读）。
func (s *ExampleStrategy) Started() int { return s.started }
`)
}

func renderStates(n heartbeatNames) string {
	return n.replacer().Replace(`package @entity@

import "github.com/tjbdwanghaibo/roost-core/gameplay/state"

// 这个文件归业务：roost add behavior --state 只在缺失时创建，project sync 不改写。用法见 @BehaviorDoc@
//
// 状态位是一组各自独立的“是否生效”事实（定身、缴械、无敌、保护期……），不是状态机。Catalog 在启动时声明一次；
// 实体上按它建 state.Set（state.NewStateDao + state.NewSet），再用 state.Gate 把状态接到行为组件的动作组上冻结 /
// 打断，例如 state.Rule{State: StateExample, Group: GroupMain, Mode: actionflow.FreezeCancel}。声明了 Persist 的状态、
// 或在 rollback=state 的 handler 里改状态，都要把 StateDao 登记进实体的 DaoManager（随 DAO 落库、随快照回滚；没登记时
// rollback=state 下改状态返回 state.ErrRollbackNotCaptured）；生成器只生成这份声明，Set / Gate 的接线由业务写。

const (
	// StateExample 是示例状态，换成自己的（ID 从 1 开始；持久状态的键是 Name，改名等于丢弃旧数据）。
	StateExample state.ID = 1
)

// stateCatalog 是 @Entity@ 的状态目录。Sync: true 的状态随同步下发，Persist: true 的进持久文档，缺省都不。
var stateCatalog = mustStateCatalog(
	state.Def{ID: StateExample, Name: "example"},
)

func mustStateCatalog(defs ...state.Def) *state.Catalog {
	catalog, err := state.NewCatalog(defs...)
	if err != nil {
		panic(err)
	}
	return catalog
}
`)
}

func renderBehaviorHandlers(n heartbeatNames, spec BehaviorSpec) string {
	strategy := ""
	if spec.AI {
		strategy = `

// handler@StrategyHandler@ 给 @Entity@ 设定示例 AI 策略（roost add behavior --ai 生成的示例，归业务）：之后 AI 心跳
// 每 500ms 决策一次，主组空闲时发起示例动作。返回设定后的策略名。
//
//roost:nest rollback=undo
func handler@StrategyHandler@(target @entity@.IBehaviorEntity) (string, error) {
	if err := target.BehaviorComp().AI().SetStrategy(@entity@.NewExampleStrategy()); err != nil {
		return "", err
	}
	return target.BehaviorComp().AI().Strategy().Name(), nil
}`
	}
	return n.replacer("@Strategy@", n.replacer().Replace(strategy)).Replace(`package handler

import (
	"time"

	@entity@ "@Module@/game/entities/@entity@"
)

// 这个文件归业务：roost add behavior 只在缺失时创建，project sync 不改写。

// handler@StartHandler@ 在 @Entity@ 上发起一个示例动作，持续 millis 毫秒，返回动作 ID（roost add behavior 生成的示例，
// 归业务）。动作由主组的心跳推进（100ms 一拍），到时成功结束。
//
// runner 不随事务回滚：先做完会失败的校验与修改，发起动作是 handler 的最后一步。
//
//roost:nest rollback=undo
func handler@StartHandler@(target @entity@.IBehaviorEntity, millis int64) (int64, error) {
	return target.BehaviorComp().Behavior().CreateAction(@entity@.ActionExample, @entity@.ExampleParam{Duration: time.Duration(millis) * time.Millisecond})
}@Strategy@
`)
}

func renderTimeDomainComponent(n heartbeatNames, spec TimeDomainSpec) string {
	getter := timerDaoGetter(n.EntityType, spec.Dao)
	return generatedHeader + n.replacer("@TimeDomainID@", strconv.FormatInt(spec.ID, 10), "@Dao@", spec.Dao, "@DaoGetter@", getter,
		"@var@", n.Var).Replace(`
// roost add timedomain --entity @Entity@ 生成，project sync 重写；倍率范围在 timedomain_config.go（业务文件）。
// 运行期在 core 的 gameplay/timedomain：@TimeDomainDoc@
package @entity@

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
)

//roost:component type=@TimeDomainID@
const CompTypeTimeDomain entity.ComponentType = @TimeDomainID@

// TimeDomainComponent 是 @Entity@（场景）的时间域：副本倍速、慢放、时停这类“一个场景里的时间流速”。域时间是业务时间
// 的分段线性映射，参数存在 @Dao@ 的三个字段里（TimeAnchorBusiness / TimeAnchorDomain / TimeRate，nopersist,sync：随
// 同步下发给客户端外推）。
//
//   - SetRate 只在本实体（场景）的锁内调用：从本条消息冻结的业务时间起生效，写字段（随事务回滚），提交后才发布给成员；
//   - 成员（任意实体、任意 goroutine）读 Domain().Now()：本条消息的域时间，同一条消息里不变；
//   - 成员的行为组件 / AI 在构造时挂上域（behavior_actions.go 的 timeDomain），心跳按域计时；
//   - 域实例随场景实例：场景卸载重载后是新的 Domain，成员要换成新的。
type TimeDomainComponent struct {
	entity.ComponentBase
	owner  *@Entity@
	domain *timedomain.Domain
}

// ITimeDomainEntity 是 Nest handler 使用本组件的锁安全视图。
type ITimeDomainEntity interface {
	entity.IThreadSafeEntity
	TimeDomainComp() *TimeDomainComponent
}

func init() {
	entity.RegisterComponentFactory(CompTypeTimeDomain, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*@Entity@)
		if !ok {
			return nil, fmt.Errorf("time domain component: owner %T is not *@Entity@", owner)
		}
		return &TimeDomainComponent{owner: typed}, nil
	})
}

func (component *TimeDomainComponent) Name() string { return "time_domain" }

// Owner 只在实体存活且被 Nest 加锁时可用。
func (component *TimeDomainComponent) Owner() *@Entity@ { return component.owner }

// Domain 是本场景的时间域（成员读 Now / Rate，场景自己的 handler 里 SetRate）。
func (component *TimeDomainComponent) Domain() *timedomain.Domain { return component.domain }

// OnInitFinish：读出存下的参数构造域并发布；存下的倍率不在 timeDomainConfig 的范围内时实体初始化失败。
func (component *TimeDomainComponent) OnInitFinish(*entity.EntityCreateParam, bool) error {
	domain, err := timedomain.New(@var@TimeDomainFields{owner: component.owner}, timeDomainConfig())
	if err != nil {
		return fmt.Errorf("time domain component: %w", err)
	}
	component.domain = domain
	return nil
}

func (component *TimeDomainComponent) OnDestroy(entity.EntityDestroyReason) {}

// @var@TimeDomainFields 把 @Dao@ 的三个字段适配成 timedomain.Fields；写入走生成的 mutator，随事务回滚、标同步脏。
type @var@TimeDomainFields struct{ owner *@Entity@ }

func (f @var@TimeDomainFields) TimeDomain() timedomain.State {
	dao := f.owner.@DaoGetter@()
	return timedomain.State{AnchorBusinessNano: dao.GetTimeAnchorBusiness(), AnchorDomainNano: dao.GetTimeAnchorDomain(), Rate: dao.GetTimeRate()}
}

func (f @var@TimeDomainFields) SetTimeDomain(s timedomain.State) {
	dao := f.owner.@DaoGetter@()
	dao.SetTimeAnchorBusiness(s.AnchorBusinessNano)
	dao.SetTimeAnchorDomain(s.AnchorDomainNano)
	dao.SetTimeRate(s.Rate)
}
`)
}

func renderTimeDomainConfig(n heartbeatNames) string {
	return n.replacer().Replace(`package @entity@

import "github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"

// 这个文件归业务：roost add timedomain 只在缺失时创建，project sync 不改写。用法见 @TimeDomainDoc@

// timeDomainConfig 是 @Entity@ 时间域允许的倍率范围（框架上限 [0, 100]，0 是时停）。SetRate 越界返回
// timedomain.ErrRateOutOfRange、什么都不改。
func timeDomainConfig() timedomain.Config {
	return timedomain.Config{MinRate: 0, MaxRate: 4}
}
`)
}
