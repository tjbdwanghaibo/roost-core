package combatcomponent

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

// Buff 的推进（DESIGN-TODO D20 P5；维护者 2026-10-21 决定：心跳只是驱动方式之一，推进只用接口）：与驱动无关的部分
// 在这里，驱动方式经 skill.Driver 接进来——实体心跳（AttachBeat，beat.go）、Lockstep 房间的定步长循环、测试里手动
// 调用都是这一份规则。本文件不依赖心跳与时间域：驱动给时刻（StepBuffs）或 tick（StepBuffsToTick），输入侧的“现在”
// 由 BuffDriveConfig.Now 给出。
//
// 规则：
//
//  1. 有工作 = 容器里有限时 buff（DueTick != 0，NextBuffDue）或业务报告的周期工作（Periodic：周期伤害、冷却、
//     回复）。BuffsPending 回答这一点。
//  2. 改了 buff（集合或到期 tick）之后、驱动的每一步之后，按 BuffsPending 通知驱动：有工作 Wake，没有 Sleep。通知
//     是电平式的（同一状态可能连续通知多次，驱动自己去重），与 skill.Stepper 同一约定。Wake 的错误交给 OnError，
//     不让改 buff 的 mutator 失败。
//  3. 驱动的一步：TickBuffs(tick)（推进一步的原语：到期 tick 不晚于 tick 的 buff 过期），然后 OnStep，最后按 Periodic
//     的新答案通知驱动。一步在调用方的事务里：过期的移除、属性投影、派生状态通知随这一步提交或回滚；OnStep 返回错误
//     时这一步不通知驱动、错误原样返回（心跳适配层让本拍事务回滚）。
//  4. 依赖方向：combat（BuffContainer.NextDue、Tick）是纯数据，不知道驱动；skill 定义 Driver 接口与 TickClock；这里
//     （combatcomponent）把容器的事务修改与驱动接口接在一起；心跳适配（beat.go）在同一个包里，只有它 import heartbeat。

// BuffDriveConfig 是 DriveBuffs 的参数。
type BuffDriveConfig struct {
	// Clock 把时刻换成技能 tick，与 StatusBridge.CurrentTick、技能 Runtime 用同一个（DueTick 按它计）。必填。
	Clock skill.TickClock
	// Now 是输入侧的“现在”（CurrentTick、StepBuffs 以外的施加用它）。必填。心跳适配层给的是本条消息冻结的 fctx.Now
	// 或 Domain.Now；定步长循环给的是当前帧的时刻。
	Now func() time.Time
	// Driver 接收 Wake / Sleep；nil 表示不需要通知（驱动每一步都推进，例如测试里手动调用、每帧都推进的循环）。
	Driver skill.Driver
	// Periodic 报告业务的周期工作是否还在（组件本身不持有这些，由业务按自己的状态回答）；为 true 时即使没有限时
	// buff 也算有工作。业务改了它的答案之后调 SyncBuffDriver。nil 表示没有。
	Periodic func() bool
	// OnStep 在驱动的每一步 TickBuffs 之后、同一事务里调用，业务在这里做周期工作。返回错误时这一步的错误原样返回。
	OnStep func(BuffStep) error
	// OnError 接收 Wake 的错误（心跳：Remote 共享模式下的 heartbeat.ErrHeartbeatRemoteShared）；nil 时打 Warn 日志。
	OnError func(error)
	// Name 出现在 Warn 日志里，标识这个驱动（心跳适配层给的是心跳登记名）。
	Name string
}

// BuffStep 是驱动的一步。
type BuffStep struct {
	// Tick 是这一步推进到的技能 tick。
	Tick int64
	// Expired 是这一步过期的 buff（按施加顺序）。
	Expired []combat.BuffInstance
}

// buffDrive 是 DriveBuffs 装上的接线；不是状态（驱动的启停状态归驱动自己）。
type buffDrive struct {
	clock    skill.TickClock
	now      func() time.Time
	driver   skill.Driver
	periodic func() bool
	onStep   func(BuffStep) error
	onError  func(error)
	name     string
}

func newBuffDrive(config BuffDriveConfig) (*buffDrive, error) {
	if err := config.Clock.Validate(); err != nil {
		return nil, fmt.Errorf("combatcomponent: buff drive: %w", err)
	}
	if config.Now == nil {
		return nil, errors.New("combatcomponent: buff drive needs Now (the time inputs are taken at)")
	}
	return &buffDrive{clock: config.Clock, now: config.Now, driver: config.Driver, periodic: config.Periodic,
		onStep: config.OnStep, onError: config.OnError, name: config.Name}, nil
}

// DriveBuffs 装上与驱动无关的 buff 推进。在实体工厂里构造组件后调用一次（与 ProjectAttributes、BindStates 一样在
// 事务外、OnInitFinish 之前安装）；之后 OnInitFinish 与每个改 buff 的 mutator 按规则第 2 条通知驱动。心跳驱动用
// AttachBeat（它在内部调这里）。
func (component *CombatComponent) DriveBuffs(config BuffDriveConfig) error {
	if component.drive != nil {
		return errors.New("combatcomponent: buff drive already attached")
	}
	drive, err := newBuffDrive(config)
	if err != nil {
		return err
	}
	component.drive = drive
	return nil
}

// CurrentTick 返回输入侧“现在”的技能 tick：BuffDriveConfig.Now（心跳驱动下是本条消息冻结的业务时间，挂了时间域时
// 是域时间）经 Clock 换算。handler 里施加 buff 用它作 ApplyBuff 的 tick，StatusBridge.CurrentTick 也接它，buff 的
// 时长因此与驱动的到期判定同一把尺子。没有 DriveBuffs / AttachBeat 时 panic（接线错误）。
func (component *CombatComponent) CurrentTick() int64 {
	drive := component.drive
	if drive == nil {
		panic("combatcomponent: CurrentTick needs DriveBuffs or AttachBeat")
	}
	return int64(drive.clock.TickAt(drive.now()))
}

// NextBuffDue 返回生效中 buff 里最早的到期 tick；没有限时 buff 时 ok 为 false。按 tick 排程的驱动（例如只在到期帧
// 推进的循环）用它。
func (component *CombatComponent) NextBuffDue() (int64, bool) {
	return component.dao.buffs.NextDue()
}

// BuffsPending 报告 buff 推进还有没有工作（规则第 1 条）：有限时 buff，或业务的周期工作还在。
func (component *CombatComponent) BuffsPending() bool {
	if _, ok := component.dao.buffs.NextDue(); ok {
		return true
	}
	drive := component.drive
	return drive != nil && drive.periodic != nil && drive.periodic()
}

// StepBuffs 是驱动的一步（规则第 3 条），时刻经 Clock 换成 tick。在事务里调用。没有 DriveBuffs / AttachBeat 时返回
// 错误。
func (component *CombatComponent) StepBuffs(now time.Time) error {
	drive := component.drive
	if drive == nil {
		return errors.New("combatcomponent: StepBuffs needs DriveBuffs or AttachBeat")
	}
	return component.stepBuffs(int64(drive.clock.TickAt(now)), drive.onStep)
}

// StepBuffsToTick 同 StepBuffs，目标直接给 tick（定步长循环按帧号推进时用）。
func (component *CombatComponent) StepBuffsToTick(tick int64) error {
	drive := component.drive
	if drive == nil {
		return errors.New("combatcomponent: StepBuffsToTick needs DriveBuffs or AttachBeat")
	}
	return component.stepBuffs(tick, drive.onStep)
}

// stepBuffs 是一步的唯一实现；hook 是这一步的业务回调（DriveBuffs 的 OnStep，或心跳适配层包了 heartbeat.Beat 的
// OnBeat）。TickBuffs 有过期时自己会通知驱动（buffsChanged），末尾再按 Periodic 的新答案通知一次。
func (component *CombatComponent) stepBuffs(tick int64, hook func(BuffStep) error) error {
	expired := component.TickBuffs(tick)
	if hook != nil {
		if err := hook(BuffStep{Tick: tick, Expired: expired}); err != nil {
			return err
		}
	}
	component.alignBuffDriver()
	return nil
}

// SyncBuffDriver 按当前的 buff 与 Periodic 重新通知驱动，返回 Wake 的错误。业务在 Periodic 的答案变了之后调用；心跳
// 驱动下 Remote 托管实体回到独占之后也要调用：运行中进入共享模式时框架停掉了心跳，回到独占不自动恢复。没有
// DriveBuffs / AttachBeat 时什么都不做。
func (component *CombatComponent) SyncBuffDriver() error {
	drive := component.drive
	if drive == nil || drive.driver == nil {
		return nil
	}
	if component.BuffsPending() {
		return drive.driver.Wake()
	}
	drive.driver.Sleep()
	return nil
}

// CloseBuffDriver 卸下 buff 的驱动：心跳驱动（AttachBeat）撤销心跳登记（heartbeat.Reg.Close：立即离开驱动、释放
// 名字），其他驱动收到一次 Sleep。之后组件不再通知任何驱动、CurrentTick 与 StepBuffs 要求重新装驱动；可以在同一心跳
// 宿主上重新 DriveBuffs / AttachBeat（例如成员换场景、换时间域时按新域重建），重建后有限时 buff 的话调一次
// SyncBuffDriver。实体卸载时由 OnDestroy 调用。与 Reg.Close 一样立即生效、不随事务回滚；可重复调用；只能在实体锁内
// 调用。
func (component *CombatComponent) CloseBuffDriver() {
	drive := component.drive
	if drive == nil {
		return
	}
	component.drive = nil
	switch driver := drive.driver.(type) {
	case nil:
	case interface{ close() }:
		driver.close()
	default:
		driver.Sleep()
	}
}

// alignBuffDriver 在改了 buff（集合或到期 tick）之后、每一步末尾通知驱动；Wake 的错误交给 OnError。
func (component *CombatComponent) alignBuffDriver() {
	if err := component.SyncBuffDriver(); err != nil {
		component.drive.report(err)
	}
}

func (drive *buffDrive) report(err error) {
	if drive.onError != nil {
		drive.onError(err)
		return
	}
	slog.Default().Warn("combatcomponent: buff driver did not wake; timed buffs will not expire until SyncBuffDriver succeeds",
		"driver", drive.name, "error", err)
}
