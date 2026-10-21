package ai

import (
	"errors"
	"fmt"
	"slices"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// AI 心跳（DESIGN-TODO D21 C4，维护者 2026-10-21 决定 7）：Controller 用实体心跳上一个独立的登记 "ai" 驱动决策，不并进
// actionflow 的组心跳——AI 的决策周期（常见 200ms～1s）与动作推进周期不同，理由同 D20 P2 的“每组一个心跳”。
//
// 周期由当前策略决定：策略可选实现 actionflow.TickIntervaler（TickInterval() time.Duration），不实现按
// BeatConfig.Period；返回 0 表示不需要周期决策（只靠动作 / 任务结束驱动），停心跳。没有策略、或冻结且不是定时冻结
// 时停；定时冻结（FreezeFor）期间照常跳，到期那一拍恢复。每次切换策略、冻结 / 恢复、以及每一拍之后按当前策略重新
// 对齐，所以策略可以随自身状态改周期（战斗中 200ms、空闲 1s）。心跳回调在实体自己的 Nest worker 上、实体锁与事务内
// 执行，Controller 仍只被持实体锁的一方调用。

// DefaultBeatName 是 AI 心跳的登记名。
const DefaultBeatName = "ai"

// DefaultBeatPeriod 是不实现 TickIntervaler 的策略的决策周期。
const DefaultBeatPeriod = 500 * time.Millisecond

// BeatConfig 是 NewBeatController 的心跳部分。
type BeatConfig struct {
	// Host 是实体的心跳宿主（与 actionflow.Behavior 等组件共用）。必填。
	Host *heartbeat.Host
	// Name 是登记名，同一实体上唯一；空取 DefaultBeatName。
	Name string
	// Period 是不实现 TickIntervaler 的策略的周期，必须是 heartbeat.Periods 之一；0 取 DefaultBeatPeriod。
	Period time.Duration
	// Options 传给 heartbeat.Host.Register。
	Options []heartbeat.RegOption
}

type controllerBeat struct {
	host    *heartbeat.Host
	name    string
	period  time.Duration
	options []heartbeat.RegOption
	reg     *heartbeat.Reg
}

// NewBeatController 构造由实体心跳驱动的 Controller（方法与 NewController 的相同，只能在实体锁内调用）。
func NewBeatController(hooks ControllerHooks, beat BeatConfig) (*Controller, error) {
	if beat.Host == nil {
		return nil, errors.New("ai: beat controller needs a heartbeat host")
	}
	if beat.Name == "" {
		beat.Name = DefaultBeatName
	}
	if beat.Period == 0 {
		beat.Period = DefaultBeatPeriod
	}
	if !slices.Contains(heartbeat.Periods[:], beat.Period) {
		return nil, fmt.Errorf("ai: beat period %v is not one of heartbeat.Periods", beat.Period)
	}
	c := NewController(hooks)
	c.beat = &controllerBeat{host: beat.Host, name: beat.Name, period: beat.Period, options: beat.Options}
	return c, nil
}

// BeatReg 返回 AI 心跳的登记；不是心跳驱动、或还从没需要过心跳时为 nil（诊断、测试用，启停由 Controller 管理）。
func (c *Controller) BeatReg() *heartbeat.Reg {
	if c == nil || c.beat == nil {
		return nil
	}
	return c.beat.reg
}

// syncBeat 让 AI 心跳与当前策略、冻结状态一致。挂不上（Remote 共享模式）经 OnError 报告，AI 不再周期决策（结束通知
// 照常送达）。
func (c *Controller) syncBeat() {
	if c == nil || c.beat == nil {
		return
	}
	interval := c.beatInterval()
	b := c.beat
	if interval == 0 {
		if b.reg != nil {
			b.reg.Stop()
		}
		return
	}
	if b.reg == nil {
		reg, err := b.host.Register(b.name, interval, c.onBeat, b.options...)
		if err != nil {
			c.report(fmt.Errorf("ai: heartbeat: %w", err))
			return
		}
		b.reg = reg
	}
	if b.reg.Period() != interval {
		if err := b.reg.SetPeriod(interval); err != nil {
			c.report(fmt.Errorf("ai: heartbeat: %w", err))
			return
		}
	}
	if err := b.reg.Start(); err != nil {
		c.report(fmt.Errorf("ai: heartbeat: %w", err))
	}
}

// beatInterval 是此刻需要的心跳档：没有策略、或无定时的冻结时 0；否则取策略的 TickInterval（不实现取
// BeatConfig.Period），换成不大于它的最长档。
func (c *Controller) beatInterval() time.Duration {
	if c.strategy == nil || (c.frozen && c.frozenUntil.IsZero()) {
		return 0
	}
	ti, ok := c.strategy.(coreflow.TickIntervaler)
	if !ok {
		return c.beat.period
	}
	return heartbeat.PeriodAtMost(ti.TickInterval())
}

// onBeat 是 AI 心跳的回调：决策一拍，之后按策略（可能已变）重新对齐周期。错误经 OnError，不让本拍事务回滚。
func (c *Controller) onBeat(beat heartbeat.Beat) error {
	c.Tick(beat.Now)
	c.syncBeat()
	return nil
}
