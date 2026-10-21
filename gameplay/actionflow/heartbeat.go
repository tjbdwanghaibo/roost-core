package actionflow

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 心跳驱动的 runner（DESIGN-TODO D20 第 2 期）：一个实体的 ActionRunner（及可选的 MissionRunner）由该实体自己的
// 心跳 tick。心跳回调在实体自己的 Nest worker 上、实体锁与事务内执行，与该实体的其他消息串行，所以 runner 仍然只被
// 持实体锁的一方调用（ActionRunner 刻意无锁的前提不变）。
//
// 启停：BeatRunner 经 runner 的 OnTransition 钩子知道每个组的当前动作；有需要周期 tick 的动作（见 TickIntervaler）时
// 挂心跳，周期取其中最短的；一个都没有时停心跳，空闲实体不占调度。由事件推进的动作（TickInterval 为 0，例如移动）
// 不挂 runner 心跳，事件到达时经 ActionContext.Waker 让 runner 马上 tick 它所在的组。AI、技能等以后都用同一个 runner
// 与这一个心跳登记。

// DefaultBeatName 是 BeatRunner 的心跳登记名。
const DefaultBeatName = "actionflow"

// DefaultBeatPeriod 是不实现 TickIntervaler 的动作的 tick 周期（10Hz）。
const DefaultBeatPeriod = 100 * time.Millisecond

// BeatConfig 是 NewBeatRunner 的心跳部分。
type BeatConfig struct {
	// Host 是实体的心跳宿主（heartbeat.Driver.NewHost），与该实体的其他组件共用。必填。
	Host *heartbeat.Host
	// Name 是心跳登记名，同一实体上唯一；空取 DefaultBeatName。
	Name string
	// Period 是不实现 TickIntervaler 的动作的周期，必须是 heartbeat.Periods 之一；0 取 DefaultBeatPeriod。
	Period time.Duration
	// Missions 非 nil 时，每拍在动作之后 Tick 当前任务（有任务时）。任务本身不决定心跳启停：只有任务、没有需要 tick
	// 的动作时不跳。
	Missions *MissionRunner
	// Options 传给 heartbeat.Host.Register（例如 WithDurability）。
	Options []heartbeat.RegOption
}

// BeatRunner 是心跳驱动的 ActionRunner 宿主。它的方法与 ActionRunner 一样只能在实体锁内调用（Nest handler、心跳
// 回调，或加载 / 新建的初始化阶段）。
type BeatRunner struct {
	runner   *ActionRunner
	missions *MissionRunner
	reg      *heartbeat.Reg
	period   time.Duration
	onError  func(error)

	// active 是各组的当前动作与它要的周期（0 = 由事件推进），由 OnTransition 维护。
	active map[ActionGroup]beatAction
	// groups 是 beat 里复用的组列表，避免每拍分配。
	groups []ActionGroup
	// elapsed 只在一拍的 runner tick 期间非零，经 PopulateContext 填进 ActionContext.Elapsed。
	elapsed time.Duration

	userTransition func(ActionSnapshot, bool)
	userPopulate   func(*ActionContext, time.Time)
	userContext    func(time.Time) *ActionContext
	host           *heartbeat.Host
}

type beatAction struct {
	id       int64
	action   Action
	interval time.Duration
}

// NewBeatRunner 构造 ActionRunner 并把它接到实体心跳上。config 的 Hooks 照常生效：BeatRunner 只在 OnTransition 与
// 上下文钩子外面包一层（先记录 / 填 Owner、Waker、Elapsed，再调业务的钩子）。Context 钩子返回的上下文被复制后
// 同样补上这三项。
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
		missions:       beat.Missions,
		period:         beat.Period,
		onError:        config.Hooks.OnError,
		active:         make(map[ActionGroup]beatAction),
		userTransition: config.Hooks.OnTransition,
		userPopulate:   config.Hooks.PopulateContext,
		userContext:    config.Hooks.Context,
	}
	b.host = beat.Host
	config.Hooks.OnTransition = b.transition
	config.Hooks.PopulateContext = b.populate
	config.Hooks.Context = nil
	runner, err := NewActionRunner(config)
	if err != nil {
		return nil, err
	}
	reg, err := beat.Host.Register(beat.Name, beat.Period, b.beat, beat.Options...)
	if err != nil {
		return nil, err
	}
	b.runner, b.reg = runner, reg
	return b, nil
}

// Runner 返回被驱动的 ActionRunner；业务照常经它 Start / Enqueue / End。
func (b *BeatRunner) Runner() *ActionRunner { return b.runner }

// Reg 返回 runner 的心跳登记（诊断、测试用；启停由 BeatRunner 管理，业务不要直接 Start / Stop）。
func (b *BeatRunner) Reg() *heartbeat.Reg { return b.reg }

// Ticking 报告当前是否有需要周期 tick 的动作（业务视角，与心跳登记的 Active 一致，除非刚发生过回滚，见 Sync）。
func (b *BeatRunner) Ticking() bool { return b.wantPeriod() > 0 }

// Wake 马上 tick 动作 a 所在的组（a 必须是某个组的当前动作，否则返回错误、什么也不做）。由事件推进的动作在事件
// 到达时调用（经 ActionContext.Waker）；在 runner 回调里调用时按 ActionRunner 的规则延后到回调返回后执行。
func (b *BeatRunner) Wake(a Action) error {
	for group, current := range b.active {
		if sameAction(current.action, a) {
			return b.runner.Tick(group, time.Time{})
		}
	}
	return errors.New("actionflow: woken action is not current in any group")
}

// Sync 按当前动作重算心跳的启停与档位。动作切换与每拍结束时自动调用（错误经 OnError 报告）；业务在两种情况下手动
// 调用：
//   - 在一个随后回滚的事务里启停过动作：runner 的状态不随事务回滚（ActionRunner 是纯内存状态），心跳登记却随回滚
//     恢复，两者可能不一致，直到下一次动作切换；
//   - Remote 托管实体运行中进入共享模式，框架停掉了心跳（共享模式不允许挂心跳，回到独占不自动恢复）：回到独占后
//     调用 Sync 重新挂上。共享期间需要 tick 时返回 heartbeat.ErrHeartbeatRemoteShared，动作留在 runner 里不推进。
func (b *BeatRunner) Sync() error {
	want := b.wantPeriod()
	if want == 0 {
		b.reg.Stop()
		return nil
	}
	if b.reg.Period() != want {
		if err := b.reg.SetPeriod(want); err != nil {
			return err
		}
	}
	return b.reg.Start()
}

// beat 是心跳回调：按组号顺序 tick 每个有需要周期 tick 的当前动作的组，再 tick 任务，最后按结果重算启停。
// runner 与任务的错误经 OnError 报告，不让本拍事务回滚：动作的推进是 runner 的内存状态，回滚不了，回滚只会让
// 同一拍里动作写的 DAO 与 runner 不一致。
func (b *BeatRunner) beat(beat heartbeat.Beat) error {
	b.elapsed = beat.Elapsed
	defer func() { b.elapsed = 0 }()
	b.groups = b.groups[:0]
	for group, current := range b.active {
		if current.interval > 0 {
			b.groups = append(b.groups, group)
		}
	}
	slices.Sort(b.groups)
	var errs []error
	for _, group := range b.groups {
		// 前面的组的回调可能已经换掉了这个组的动作：只 tick 仍需要周期 tick 的。
		if current, ok := b.active[group]; !ok || current.interval == 0 {
			continue
		}
		errs = append(errs, b.runner.Tick(group, beat.Now))
	}
	if b.missions != nil && b.missions.InMission() {
		b.missions.Tick(beat.Now)
	}
	errs = append(errs, b.Sync())
	b.report(errors.Join(errs...))
	return nil
}

// transition 记录每个组的当前动作及它要的周期，并重算心跳。
func (b *BeatRunner) transition(s ActionSnapshot, entering bool) {
	if entering {
		b.active[s.Group] = beatAction{id: s.ID, action: s.Action, interval: b.intervalOf(s.Action)}
	} else if current, ok := b.active[s.Group]; ok && current.id == s.ID {
		delete(b.active, s.Group)
	}
	b.report(b.Sync())
	if b.userTransition != nil {
		b.userTransition(s, entering)
	}
}

// populate 填 Owner（业务没填时取心跳宿主的实体）、Waker 与本拍的 Elapsed，再交给业务的上下文钩子。
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
	ctx.Waker = b
	ctx.Elapsed = b.elapsed
}

func (b *BeatRunner) wantPeriod() time.Duration {
	var want time.Duration
	for _, current := range b.active {
		if current.interval > 0 && (want == 0 || current.interval < want) {
			want = current.interval
		}
	}
	return want
}

// intervalOf 把动作要的间隔换成档位（见 TickIntervaler）；0 表示由事件推进。
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

func (b *BeatRunner) report(err error) {
	if err != nil && b.onError != nil {
		defer func() { _ = recover() }()
		b.onError(err)
	}
}

// sameAction 比较两个动作是否同一个；动态类型不可比较（例如值类型里带切片）时按不同处理，不 panic。
func sameAction(x, y Action) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return x == y
}
