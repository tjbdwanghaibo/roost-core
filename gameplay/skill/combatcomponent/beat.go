package combatcomponent

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// Buff 的心跳驱动（DESIGN-TODO D20 P5；维护者 2026-10-21 决定：心跳只是驱动方式之一）：推进规则在 buff_stepper.go
// （DriveBuffs，与驱动无关），这里只做心跳的接线——Wake / Sleep 换成心跳登记的 Start / Stop（heartbeatDriver，与
// 技能 Runtime 共用），每拍调一步（TickBuffs 到本拍时刻对应的 tick，再 OnBeat）。空闲的战斗单位不占调度。
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
	// Periodic 是 BuffDriveConfig.Periodic：为 true 时即使没有限时 buff 也保持心跳；业务改了答案之后调
	// SyncBuffDriver。nil 表示没有。
	Periodic func() bool
	// OnBeat 在每拍 TickBuffs 之后、同一事务里调用，业务在这里做周期工作。返回错误时本拍事务回滚（含这一拍的过期）。
	OnBeat func(BuffBeat) error
	// OnError 接收挂不上心跳的错误（Remote 共享模式：heartbeat.ErrHeartbeatRemoteShared）；nil 时打 Warn 日志。
	OnError func(error)
}

// BuffBeat 是交给 BeatConfig.OnBeat 的一拍：心跳的拍加上这一步（Tick 是 Beat.Now 换算出的技能 tick）。
type BuffBeat struct {
	heartbeat.Beat
	BuffStep
}

// AttachBeat 把 buff 推进接到实体心跳上（心跳驱动的 DriveBuffs）。在实体工厂里构造组件后调用一次（与
// ProjectAttributes、BindStates 一样在事务外、OnInitFinish 之前安装）：登记一个停止状态的心跳，之后由组件按 buff 与
// Periodic 自动启停。Remote 托管实体处于共享模式时启动返回 heartbeat.ErrHeartbeatRemoteShared（经 OnError）。
func (component *CombatComponent) AttachBeat(config BeatConfig) error {
	if component.drive != nil {
		return errors.New("combatcomponent: buff drive already attached")
	}
	if config.Host == nil {
		return errors.New("combatcomponent: beat needs a heartbeat host")
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
	domain := config.TimeDomain
	driver := &heartbeatDriver{}
	drive, err := newBuffDrive(BuffDriveConfig{
		Clock: config.Clock, Now: func() time.Time { return messageNow(domain) }, Driver: driver,
		Periodic: config.Periodic, OnError: config.OnError, Name: config.Name,
	})
	if err != nil {
		return err
	}
	options := config.Options
	if domain != nil {
		options = append(slices.Clone(options), heartbeat.WithTimeDomain(domain))
	}
	onBeat := config.OnBeat
	reg, err := config.Host.Register(config.Name, heartbeat.PeriodAtMost(config.Granularity), func(beat heartbeat.Beat) error {
		return component.runBeat(beat, onBeat)
	}, options...)
	if err != nil {
		return err
	}
	driver.reg = reg
	component.drive = drive
	return nil
}

// BeatReg 返回 buff 心跳的登记；没有 AttachBeat（没装驱动，或装的是别的驱动）时为 nil（诊断、测试用；启停由组件
// 管理，业务不要直接 Start / Stop）。
func (component *CombatComponent) BeatReg() *heartbeat.Reg {
	if component.drive == nil {
		return nil
	}
	if driver, ok := component.drive.driver.(*heartbeatDriver); ok {
		return driver.reg
	}
	return nil
}

// messageNow 是心跳驱动下输入侧的“现在”：本条消息冻结的业务时间，挂了时间域时是域时间。
func messageNow(domain *timedomain.Domain) time.Time {
	if domain != nil {
		return domain.Now()
	}
	return fctx.Now()
}

// runBeat 是心跳回调：一步推进到本拍的 tick，OnBeat 收到心跳的拍与这一步；OnBeat 的错误让本拍事务回滚。
func (component *CombatComponent) runBeat(beat heartbeat.Beat, onBeat func(BuffBeat) error) error {
	var hook func(BuffStep) error
	if onBeat != nil {
		hook = func(step BuffStep) error { return onBeat(BuffBeat{Beat: beat, BuffStep: step}) }
	}
	return component.stepBuffs(int64(component.drive.clock.TickAt(beat.Now)), hook)
}
