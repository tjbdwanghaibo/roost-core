package actionflow

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 心跳驱动的 runner（DESIGN-TODO D20 第 2 期）：一个实体的 ActionRunner 由该实体自己的心跳 tick。所有控制实体行为的
// 逻辑（移动、技能、AI 决策……）都经 ActionRunner 下发与收尾，不绕过动作直接驱动组件（维护者 2026-10-21）：组件
// 只提供推进接口（例如 movement.Move.Advance），由动作在自己的 Tick 里调用。
//
// 模型参考 ssr 的 action_list：每个组的当前动作有自己的心跳登记（"actionflow/<组号>"），周期取动作的 TickInterval；
// 组空闲时停，空闲实体不占调度。一拍只 tick 这个组，ActionContext.Elapsed 是这一拍的 Beat.Elapsed（不追帧）。心跳
// 回调在实体自己的 Nest worker 上、实体锁与事务内执行，所以 runner 仍然只被持实体锁的一方调用（ActionRunner 刻意
// 无锁的前提不变），动作在 Tick 里对组件（DAO）的修改随这一拍的事务提交或回滚。
//
// 事务与 runner（维护者 2026-10-21 决定：由业务层保证，框架不处理）：ActionRunner / MissionRunner 是纯内存状态，
// 不随 Nest 事务回滚。handler 先做完全部可能失败的校验与修改，最后一步才调用 Start / Enqueue / End 等；之后若还可能
// 失败，回滚后 runner 里会残留这次的变更（例如一个已启动的动作），而心跳登记的启停随回滚恢复。心跳那一拍里，
// 动作 Tick 返回的结论（结束、下一个动作启动）同样不随这一拍的事务回滚；这一拍只有提交失败才会回滚（心跳回调
// 本身不返回错误），框架不处理。

// DefaultBeatName 是 BeatRunner 心跳登记名的前缀，每个组一个登记："actionflow/<组号>"。
const DefaultBeatName = "actionflow"

// DefaultBeatPeriod 是不实现 TickIntervaler 的动作的 tick 周期（10Hz）。
const DefaultBeatPeriod = 100 * time.Millisecond

// BeatConfig 是 NewBeatRunner 的心跳部分。
type BeatConfig struct {
	// Host 是实体的心跳宿主（heartbeat.Driver.NewHost），与该实体的其他组件共用。必填。
	Host *heartbeat.Host
	// Name 是心跳登记名的前缀，同一实体上唯一；空取 DefaultBeatName。
	Name string
	// Period 是不实现 TickIntervaler 的动作的周期，必须是 heartbeat.Periods 之一；0 取 DefaultBeatPeriod。
	Period time.Duration
	// Options 传给 heartbeat.Host.Register（例如 WithDurability；缺省 async）。
	Options []heartbeat.RegOption
}

// BeatRunner 是心跳驱动的 ActionRunner 宿主。它的方法与 ActionRunner 一样只能在实体锁内调用（Nest handler、心跳
// 回调，或加载 / 新建的初始化阶段）。
type BeatRunner struct {
	runner  *ActionRunner
	host    *heartbeat.Host
	name    string
	period  time.Duration
	options []heartbeat.RegOption
	onError func(error)

	// groups 是各组的心跳登记与当前动作，组第一次有动作时登记、之后保留。
	groups map[ActionGroup]*groupBeat
	// elapsed 只在某个组的一拍 tick 期间非零，经 PopulateContext 填进 ActionContext.Elapsed。
	elapsed time.Duration

	userTransition func(ActionSnapshot, bool)
	userFreeze     func(ActionGroup, bool)
	userPopulate   func(*ActionContext, time.Time)
	userContext    func(time.Time) *ActionContext
}

// groupBeat 是一个组的心跳：当前动作的 ID 与它要的周期（0 = 不需要周期 tick）。
type groupBeat struct {
	reg      *heartbeat.Reg
	current  int64
	interval time.Duration
}

// NewBeatRunner 构造 ActionRunner 并把它接到实体心跳上。config 的 Hooks 照常生效：BeatRunner 只在 OnTransition、
// OnFreeze 与上下文钩子外面包一层（先启停组的心跳 / 填 Owner 与 Elapsed，再调业务的钩子）。冻结的组停心跳（暂停的
// 当前动作不被 tick，没必要占调度），解冻后按当前动作重新挂上；解冻后的第一拍 Elapsed 从重新启动算起，暂停期间的
// 时间不计入。Context 钩子返回的上下文被复制后
// 同样补上这两项。
func NewBeatRunner(config ActionRunnerConfig, beat BeatConfig) (*BeatRunner, error) {
	if beat.Host == nil {
		return nil, errors.New("actionflow: beat runner needs a heartbeat host")
	}
	if beat.Name == "" {
		beat.Name = DefaultBeatName
	}
	if beat.Period == 0 {
		beat.Period = DefaultBeatPeriod
	}
	if !slices.Contains(heartbeat.Periods[:], beat.Period) {
		return nil, fmt.Errorf("actionflow: beat period %v is not one of heartbeat.Periods", beat.Period)
	}
	b := &BeatRunner{
		host:           beat.Host,
		name:           beat.Name,
		period:         beat.Period,
		options:        beat.Options,
		onError:        config.Hooks.OnError,
		groups:         make(map[ActionGroup]*groupBeat),
		userTransition: config.Hooks.OnTransition,
		userFreeze:     config.Hooks.OnFreeze,
		userPopulate:   config.Hooks.PopulateContext,
		userContext:    config.Hooks.Context,
	}
	config.Hooks.OnTransition = b.transition
	config.Hooks.OnFreeze = b.freeze
	config.Hooks.PopulateContext = b.populate
	config.Hooks.Context = nil
	runner, err := NewActionRunner(config)
	if err != nil {
		return nil, err
	}
	b.runner = runner
	return b, nil
}

// Runner 返回被驱动的 ActionRunner；业务照常经它 Start / Enqueue / End（在 handler 的最后一步，见包说明）。
func (b *BeatRunner) Runner() *ActionRunner { return b.runner }

// Reg 返回组的心跳登记；组从未有过动作时为 nil（诊断、测试用；启停由 BeatRunner 管理，业务不要直接 Start / Stop）。
func (b *BeatRunner) Reg(group ActionGroup) *heartbeat.Reg {
	if gb := b.groups[group]; gb != nil {
		return gb.reg
	}
	return nil
}

// Ticking 报告是否有组的当前动作需要周期 tick（业务视角）。
func (b *BeatRunner) Ticking() bool {
	for _, gb := range b.groups {
		if gb.current != 0 && gb.interval > 0 {
			return true
		}
	}
	return false
}

// Sync 按各组的当前动作重新挂上心跳（需要周期 tick 而心跳没在跳的组），返回挂不上的错误。业务在 Remote 托管实体
// 回到独占之后调用：运行中进入共享模式时框架停掉了心跳（共享模式不允许挂心跳，回到独占不自动恢复），这期间动作
// 留在 runner 里、不推进。也可以在一个随后回滚的 handler 改过 runner 之后用它让心跳与 runner 重新一致（见包说明）。
func (b *BeatRunner) Sync() error {
	var errs []error
	for _, group := range b.sortedGroups() {
		gb := b.groups[group]
		if gb.current != 0 && gb.interval > 0 && !gb.reg.Active() && !b.runner.Frozen(group) {
			errs = append(errs, b.startGroup(gb))
		}
	}
	return errors.Join(errs...)
}

// transition 在组的当前动作切换时启停这个组的心跳：进入需要周期 tick 的动作时换到它的档并启动，离开时停止。
// 心跳挂不上（Remote 托管实体处于共享模式：heartbeat.ErrHeartbeatRemoteShared）时把这个动作以失败结束——需要周期
// tick 的动作没有心跳就永远不会推进。结束经 runner 的延后队列，在本次切换的回调返回后执行。
func (b *BeatRunner) transition(s ActionSnapshot, entering bool) {
	gb, err := b.group(s.Group)
	switch {
	case err != nil:
		b.report(err)
		if entering {
			b.failCurrent(s.Group, err)
		}
	case entering:
		gb.current, gb.interval = s.ID, b.intervalOf(s.Action)
		if gb.interval == 0 || b.runner.Frozen(s.Group) {
			gb.reg.Stop()
		} else if err := b.startGroup(gb); err != nil {
			b.report(err)
			b.failCurrent(s.Group, err)
		}
	case gb.current == s.ID:
		gb.current, gb.interval = 0, 0
		gb.reg.Stop()
	}
	if b.userTransition != nil {
		b.userTransition(s, entering)
	}
}

// freeze 在组冻结时停这个组的心跳，解冻时按当前动作重新挂上（挂不上同 transition，以失败结束当前动作）。
func (b *BeatRunner) freeze(group ActionGroup, frozen bool) {
	if gb := b.groups[group]; gb != nil {
		switch {
		case frozen:
			gb.reg.Stop()
		case gb.current != 0 && gb.interval > 0:
			if err := b.startGroup(gb); err != nil {
				b.report(err)
				b.failCurrent(group, err)
			}
		}
	}
	if b.userFreeze != nil {
		b.userFreeze(group, frozen)
	}
}

func (b *BeatRunner) failCurrent(group ActionGroup, err error) {
	if endErr := b.runner.End(group, true, NewActionErrorReason(err)); endErr != nil {
		b.report(endErr)
	}
}

// group 取（第一次时登记）组的心跳。登记是停止状态，名字 "<Name>/<组号>"。
func (b *BeatRunner) group(group ActionGroup) (*groupBeat, error) {
	if gb := b.groups[group]; gb != nil {
		return gb, nil
	}
	reg, err := b.host.Register(fmt.Sprintf("%s/%d", b.name, group), b.period, func(beat heartbeat.Beat) error {
		return b.beat(group, beat)
	}, b.options...)
	if err != nil {
		return nil, err
	}
	gb := &groupBeat{reg: reg}
	b.groups[group] = gb
	return gb, nil
}

func (b *BeatRunner) startGroup(gb *groupBeat) error {
	if gb.reg.Period() != gb.interval {
		if err := gb.reg.SetPeriod(gb.interval); err != nil {
			return err
		}
	}
	return gb.reg.Start()
}

// beat 是一个组的心跳回调：tick 这个组的当前动作，ActionContext.Elapsed 为本拍的 Beat.Elapsed。runner 的错误经
// OnError 报告，不让本拍事务回滚（见包说明）。
func (b *BeatRunner) beat(group ActionGroup, beat heartbeat.Beat) error {
	gb := b.groups[group]
	if gb == nil || gb.current == 0 || gb.interval == 0 {
		// 锁序上更早提交的切换已经让它空闲（Stop 生效前到达的拍）。
		return nil
	}
	b.elapsed = beat.Elapsed
	defer func() { b.elapsed = 0 }()
	b.report(b.runner.Tick(group, beat.Now))
	return nil
}

// populate 填 Owner（业务没填时取心跳宿主的实体）与本拍的 Elapsed，再交给业务的上下文钩子。
func (b *BeatRunner) populate(ctx *ActionContext, now time.Time) {
	switch {
	case b.userPopulate != nil:
		b.userPopulate(ctx, now)
	case b.userContext != nil:
		if candidate := b.userContext(now); candidate != nil {
			*ctx = *candidate
		}
	}
	if ctx.Owner == nil {
		ctx.Owner = b.host.Owner()
	}
	ctx.Elapsed = b.elapsed
}

// intervalOf 把动作要的间隔换成档位（见 TickIntervaler）；0 表示不需要周期 tick。
func (b *BeatRunner) intervalOf(a Action) time.Duration {
	ti, ok := a.(TickIntervaler)
	if !ok {
		return b.period
	}
	want := ti.TickInterval()
	if want <= 0 {
		return 0
	}
	period := heartbeat.Periods[0]
	for _, p := range heartbeat.Periods {
		if p <= want {
			period = p
		}
	}
	return period
}

func (b *BeatRunner) sortedGroups() []ActionGroup {
	groups := make([]ActionGroup, 0, len(b.groups))
	for group := range b.groups {
		groups = append(groups, group)
	}
	slices.Sort(groups)
	return groups
}

func (b *BeatRunner) report(err error) {
	if err != nil && b.onError != nil {
		defer func() { _ = recover() }()
		b.onError(err)
	}
}
