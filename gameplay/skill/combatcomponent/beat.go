package combatcomponent

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// Buff 的心跳（DESIGN-TODO D20 P5）：快池上的周期逻辑一律以实体心跳为底层，buff 到期不再由业务自己写周期消息调
// TickBuffs。组件在有限时 buff（DueTick != 0）或业务报告的周期工作（Periodic：周期伤害、冷却、回复）时挂一个心跳
// 登记，每拍把 buff 推进到本拍时刻对应的技能 tick（TickBuffs），全部清空后停；空闲的战斗单位不占调度。
//
// 与事务：启停走心跳登记的事务语义（handler 里提交后生效、回滚作废），所以 buff 集合与心跳启停总是一致——施加
// 限时 buff 的 handler 回滚了，心跳也不会启动。拍本身在实体锁与事务内，过期的移除、属性投影、派生状态通知随这一拍
// 提交或回滚。
//
// 加载：OnInitFinish 按加载出的 buff 对齐心跳（不在事务里，立即生效），所以停服期间已经到期的 buff 在加载后的第一拍
// 清掉（不是在 OnInitFinish 里：那里不在事务里，改 DAO 会 panic）。第一拍之前读到的 ActiveBuffs 仍含这些过期记录。
//
// 精度：一拍到来时才过期，到期时刻最多晚一个心跳周期（Granularity 取档后的周期），再加 tick 取整（TickClock 向下
// 取整）。需要离线也准点、且结果要落档的长时 buff（例如 24 小时的增益到点发奖励）用 entitytimer，不用这里。

// DefaultBeatName 是 buff 心跳的登记名。
const DefaultBeatName = "combat"

// DefaultBeatGranularity 是 buff 心跳的缺省粒度（1Hz）。
const DefaultBeatGranularity = time.Second

// BeatConfig 是 AttachBeat 的参数。
type BeatConfig struct {
	// Host 是实体的心跳宿主（heartbeat.Driver.NewHost），与该实体的其他组件共用。必填。
	Host *heartbeat.Host
	// Clock 把时刻换成技能 tick，与 StatusBridge.CurrentTick、Runtime 用同一个（DueTick 按它计）。必填。
	Clock skill.TickClock
	// Name 是心跳登记名，同一实体上唯一；空取 DefaultBeatName。
	Name string
	// Granularity 是 buff 到期允许的最大延迟；心跳周期取不大于它的最长档（heartbeat.PeriodAtMost），0 取
	// DefaultBeatGranularity（1Hz）。短 buff 多（0.5 秒的眩晕）时调小，代价是每个带限时 buff 的实体每秒多几拍。
	Granularity time.Duration
	// Options 传给 heartbeat.Host.Register（例如 WithDurability；缺省 async）。
	Options []heartbeat.RegOption
	// TimeDomain 非 nil 时实体在场景的时间域里（DESIGN-TODO D22）：心跳挂上这个域（heartbeat.WithTimeDomain），
	// 拍里的 tick 按域时间换算，CurrentTick 也取域时间——buff 时长随场景变速，时停时不过期。
	TimeDomain *timedomain.Domain
	// Periodic 报告业务的周期工作是否还在（周期伤害、冷却、回复……组件本身不持有这些，由业务按自己的状态回答）；
	// 为 true 时即使没有限时 buff 也保持心跳。业务改了它的答案之后调 SyncBeat。nil 表示没有。
	Periodic func() bool
	// OnBeat 在每拍 TickBuffs 之后、同一事务里调用，业务在这里做周期工作。返回错误时本拍事务回滚（含这一拍的过期）。
	OnBeat func(BuffBeat) error
	// OnError 接收挂不上心跳的错误（Remote 共享模式：heartbeat.ErrHeartbeatRemoteShared）；nil 时打 Warn 日志。
	OnError func(error)
}

// BuffBeat 是交给 BeatConfig.OnBeat 的一拍。
type BuffBeat struct {
	heartbeat.Beat
	// Tick 是本拍时刻（Beat.Now，挂了时间域时是域时间）换算出的技能 tick。
	Tick int64
	// Expired 是本拍过期的 buff（按施加顺序）。
	Expired []combat.BuffInstance
}

// buffBeat 是 AttachBeat 装上的接线；不是状态（心跳登记的启停由 heartbeat 按事务管理）。
type buffBeat struct {
	reg      *heartbeat.Reg
	clock    skill.TickClock
	domain   *timedomain.Domain
	periodic func() bool
	onBeat   func(BuffBeat) error
	onError  func(error)
}

// AttachBeat 把 buff 推进接到实体心跳上。在实体工厂里构造组件后调用一次（与 ProjectAttributes、BindStates 一样在
// 事务外、OnInitFinish 之前安装）：登记一个停止状态的心跳，之后由组件按 buff 与 Periodic 自动启停。Remote 托管实体
// 处于共享模式时返回 heartbeat.ErrHeartbeatRemoteShared（不允许挂载心跳）。
func (component *CombatComponent) AttachBeat(config BeatConfig) error {
	if component.beat != nil {
		return errors.New("combatcomponent: beat already attached")
	}
	if config.Host == nil {
		return errors.New("combatcomponent: beat needs a heartbeat host")
	}
	if err := config.Clock.Validate(); err != nil {
		return fmt.Errorf("combatcomponent: beat: %w", err)
	}
	if config.Name == "" {
		config.Name = DefaultBeatName
	}
	if config.Granularity == 0 {
		config.Granularity = DefaultBeatGranularity
	}
	if config.Granularity < 0 {
		return fmt.Errorf("combatcomponent: beat granularity %v is negative", config.Granularity)
	}
	options := config.Options
	if config.TimeDomain != nil {
		options = append(slices.Clone(options), heartbeat.WithTimeDomain(config.TimeDomain))
	}
	b := &buffBeat{clock: config.Clock, domain: config.TimeDomain, periodic: config.Periodic, onBeat: config.OnBeat, onError: config.OnError}
	reg, err := config.Host.Register(config.Name, heartbeat.PeriodAtMost(config.Granularity), component.runBeat, options...)
	if err != nil {
		return err
	}
	b.reg = reg
	component.beat = b
	return nil
}

// BeatReg 返回 buff 心跳的登记；没有 AttachBeat 时为 nil（诊断、测试用；启停由组件管理，业务不要直接 Start / Stop）。
func (component *CombatComponent) BeatReg() *heartbeat.Reg {
	if component.beat == nil {
		return nil
	}
	return component.beat.reg
}

// CurrentTick 返回本条消息的技能 tick：本条消息冻结的业务时间（fctx.Now；挂了时间域时是域时间）经 BeatConfig.Clock
// 换算。handler 里施加 buff 用它作 ApplyBuff 的 tick，StatusBridge.CurrentTick 也接它，buff 的时长因此与心跳的到期
// 判定同一把尺子。没有 AttachBeat 时 panic（接线错误）。
func (component *CombatComponent) CurrentTick() int64 {
	b := component.beat
	if b == nil {
		panic("combatcomponent: CurrentTick needs AttachBeat")
	}
	return int64(b.clock.TickAt(b.now()))
}

func (b *buffBeat) now() time.Time {
	if b.domain != nil {
		return b.domain.Now()
	}
	return fctx.Now()
}

// SyncBeat 按当前的 buff 与 Periodic 重新对齐心跳，返回挂不上的错误。业务在 Periodic 的答案变了之后调用；Remote
// 托管实体回到独占之后也要调用：运行中进入共享模式时框架停掉了心跳，回到独占不自动恢复。实体锁内调用。
func (component *CombatComponent) SyncBeat() error {
	if component.beat == nil {
		return nil
	}
	return component.beat.align(component.wantsBeat())
}

// alignBeat 在改了 buff（集合或到期 tick）之后对齐心跳；挂不上的错误交给 OnError。
func (component *CombatComponent) alignBeat() {
	b := component.beat
	if b == nil {
		return
	}
	if err := b.align(component.wantsBeat()); err != nil {
		b.report(err)
	}
}

// wantsBeat：有限时 buff，或业务的周期工作还在。
func (component *CombatComponent) wantsBeat() bool {
	if _, ok := component.dao.buffs.NextDue(); ok {
		return true
	}
	return component.beat.periodic != nil && component.beat.periodic()
}

// align 只在业务视角的状态不同时启停（Reg 自己也会忽略重复的启停，这里少一次事务登记）。
func (b *buffBeat) align(want bool) error {
	switch {
	case want && !b.reg.Active():
		return b.reg.Start()
	case !want && b.reg.Active():
		b.reg.Stop()
	}
	return nil
}

func (b *buffBeat) report(err error) {
	if b.onError != nil {
		b.onError(err)
		return
	}
	slog.Default().Warn("combatcomponent: buff heartbeat not started; timed buffs will not expire until SyncBeat succeeds",
		"registration", b.reg.Name(), "error", err)
}

// runBeat 是心跳回调：把 buff 推进到本拍的 tick（TickBuffs 自己会在全部清空后停心跳），再交给业务的 OnBeat，最后按
// Periodic 的新答案对齐。
func (component *CombatComponent) runBeat(beat heartbeat.Beat) error {
	b := component.beat
	tick := int64(b.clock.TickAt(beat.Now))
	expired := component.TickBuffs(tick)
	if b.onBeat != nil {
		if err := b.onBeat(BuffBeat{Beat: beat, Tick: tick, Expired: expired}); err != nil {
			return err
		}
	}
	component.alignBeat()
	return nil
}
