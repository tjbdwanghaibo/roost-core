package combatcomponent

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// 技能 Runtime 的心跳驱动（心跳只是驱动方式之一）：推进规则全部在
// skill.Stepper（gameplay/skill/stepper.go，不依赖 Nest / 心跳），这里只做心跳的接线——
//
//   - skill.Driver 的 Wake / Sleep 换成心跳登记的 Start / Stop（heartbeatDriver，buff 心跳共用）；
//   - 每拍调 Stepper.StepTo(Beat.Now, 域倍率)；挂了时间域时 Beat.Now 是域时间，时停时 tick 不动、2 倍速时走两倍；
//   - 输入侧的“现在”是本条消息冻结的 fctx.Now 或 Domain.Now（StepperConfig.Now / Rate）。
//
// 与事务：Runtime 不在 Nest 的回滚域里（guide/08-skill §4）。拍里推进出的错误交给 OnError、不让本拍事务回滚
// （Host 效果对战斗 DAO 的修改已随这一拍发生，回滚它们而 Runtime 不回滚只会更不一致）；下一拍从 Runtime 实际到达的
// tick 继续。handler 里的 Start / Input 同理：放在 handler 的最后一步（与 actionflow 的 runner 同一规则），之后还可能
// 失败、回滚了心跳的启动时，Runtime 里的施法留着、心跳没挂上——调一次 Sync（或下一次输入）补上：Stepper 的通知是
// 电平式的，heartbeatDriver 按登记的业务视角状态（Reg.Active，随回滚撤销）去重。

// DefaultRuntimeBeatName 是技能 Runtime 心跳的登记名。
const DefaultRuntimeBeatName = "skill"

// RuntimeBeatConfig 是 NewRuntimeBeat 的参数。
type RuntimeBeatConfig struct {
	// Host 是 Runtime 所属实体的心跳宿主（heartbeat.Driver.NewHost）。必填。
	Host *heartbeat.Host
	// Clock 把时刻换成技能 tick，与 buff（CombatComponent.AttachBeat）、StatusBridge.CurrentTick 用同一个。必填。
	Clock skill.TickClock
	// Name 是心跳登记名，同一实体上唯一；空取 DefaultRuntimeBeatName。
	Name string
	// Period 是心跳周期，必须是 heartbeat.Periods 之一；0 取不大于 tick 时长的最长档（tick 短于 50ms 时取 50ms，一拍
	// 推进 2～3 个 tick）。
	Period time.Duration
	// MaxCatchUp 是 skill.StepperConfig.MaxCatchUp；0 取 max(1s, Period)，与心跳 Beat.Elapsed 的上限同一口径。
	MaxCatchUp time.Duration
	// Options 传给 heartbeat.Host.Register（例如 WithDurability；缺省 async）。
	Options []heartbeat.RegOption
	// TimeDomain 非 nil 时 Runtime 在场景的时间域里：心跳挂上这个域，tick 按域时间换算，handler
	// 里的输入也取域时间——时停时 tick 不前进，2 倍速时 tick 走两倍。
	TimeDomain *timedomain.Domain
	// OnError 接收拍里推进的错误与挂不上心跳的错误；nil 时打 Warn 日志。
	OnError func(error)
}

// RuntimeBeat 是心跳驱动的 skill.Stepper：推进与输入的方法（Start / Input / Do / CatchUp / Sync / Tick / Runtime /
// Stats）都是嵌入的 Stepper 的，这里只多心跳登记。方法只能在 Runtime 所属实体的锁内调用（Nest handler、心跳回调，
// 或加载 / 新建的初始化阶段）。
type RuntimeBeat struct {
	*skill.Stepper
	reg     *heartbeat.Reg
	domain  *timedomain.Domain
	onError func(error)
}

// NewRuntimeBeat 把 runtime 接到实体心跳上：登记一个停止状态的心跳，runtime 有工作时由 Stepper 经 Wake 启动。在实体
// 工厂里或加载 / 新建的初始化阶段构造；恢复出的 Runtime（RestoreRuntime）还有工作时构造后调一次 Sync。
func NewRuntimeBeat(runtime *skill.Runtime, config RuntimeBeatConfig) (*RuntimeBeat, error) {
	if runtime == nil {
		return nil, errors.New("combatcomponent: runtime beat needs a runtime")
	}
	if config.Host == nil {
		return nil, errors.New("combatcomponent: runtime beat needs a heartbeat host")
	}
	if err := config.Clock.Validate(); err != nil {
		return nil, fmt.Errorf("combatcomponent: runtime beat: %w", err)
	}
	if config.Name == "" {
		config.Name = DefaultRuntimeBeatName
	}
	if config.Period == 0 {
		config.Period = heartbeat.PeriodAtMost(config.Clock.Length)
	}
	if !slices.Contains(heartbeat.Periods[:], config.Period) {
		return nil, fmt.Errorf("combatcomponent: runtime beat period %v is not one of heartbeat.Periods", config.Period)
	}
	if config.MaxCatchUp == 0 {
		config.MaxCatchUp = max(time.Second, config.Period)
	}
	b := &RuntimeBeat{domain: config.TimeDomain, onError: config.OnError}
	driver := &heartbeatDriver{}
	stepper, err := skill.NewStepper(runtime, skill.StepperConfig{
		Clock: config.Clock, Now: b.messageNow, Rate: b.messageRate, Driver: driver, MaxCatchUp: config.MaxCatchUp, Name: config.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("combatcomponent: runtime beat: %w", err)
	}
	options := config.Options
	if config.TimeDomain != nil {
		options = append(slices.Clone(options), heartbeat.WithTimeDomain(config.TimeDomain))
	}
	reg, err := config.Host.Register(config.Name, config.Period, b.beat, options...)
	if err != nil {
		return nil, err
	}
	driver.reg, b.reg, b.Stepper = reg, reg, stepper
	return b, nil
}

// Reg 返回心跳登记（诊断、测试用；启停由 Stepper 经 Wake / Sleep 管理，业务不要直接 Start / Stop）。
func (b *RuntimeBeat) Reg() *heartbeat.Reg { return b.reg }

// Close 撤销技能心跳登记（heartbeat.Reg.Close：立即离开驱动、释放名字，同一宿主上可以马上按同名重建）。之后输入
// （Start / Input / Do）在 Runtime 有工作时返回 heartbeat.ErrRegClosed，拍不再到来；Runtime 本身不动（它归业务，
// 可以 checkpoint 或交给新的推进器）。持有 RuntimeBeat 的组件在 OnDestroy 里调用；成员换场景（换时间域）时撤掉旧的、
// 用同一个 Runtime 按新域 NewRuntimeBeat 重建，之后只用新的。与 Reg.Close 一样立即生效、不随事务回滚；可重复调用；
// 只能在实体锁内调用。
func (b *RuntimeBeat) Close() { b.reg.Close() }

// Closed 报告是否已 Close。
func (b *RuntimeBeat) Closed() bool { return b.reg.Closed() }

// messageNow 是输入侧的“现在”：本条消息冻结的业务时间，挂了时间域时是域时间。
func (b *RuntimeBeat) messageNow() time.Time {
	if b.domain != nil {
		return b.domain.Now()
	}
	return fctx.Now()
}

func (b *RuntimeBeat) messageRate() float64 {
	if b.domain != nil {
		return b.domain.Rate()
	}
	return 1
}

// beat 是心跳回调：把本拍的时刻与倍率交给 Stepper。错误交给 OnError，不让本拍事务回滚（见文件头“与事务”）。
func (b *RuntimeBeat) beat(beat heartbeat.Beat) error {
	rate := 1.0
	if b.domain != nil {
		rate = b.domain.Rate() // 与 Beat.Now 同一次冻结（heartbeat 读的是同一条消息的 Domain.Read）
	}
	if err := b.StepTo(beat.Now, rate); err != nil {
		b.report(fmt.Errorf("combatcomponent: skill runtime heartbeat: %w", err))
	}
	return nil
}

func (b *RuntimeBeat) report(err error) {
	if b.onError != nil {
		b.onError(err)
		return
	}
	slog.Default().Warn("combatcomponent: skill runtime heartbeat", "registration", b.reg.Name(), "error", err)
}

// heartbeatDriver 是 skill.Driver 的心跳实现：Wake / Sleep 换成心跳登记的 Start / Stop。只在业务视角的状态
// （Reg.Active：事务里提交后生效、回滚撤销；Remote 共享模式时框架停掉）不同时启停，所以 Stepper 与 buff 推进的
// 电平式通知不会每拍多一次事务登记，被外部改掉的状态也能在下一次通知时补上。
type heartbeatDriver struct {
	reg *heartbeat.Reg
}

// Wake 启动心跳；Remote 共享模式时返回 heartbeat.ErrHeartbeatRemoteShared。
func (d *heartbeatDriver) Wake() error {
	if d.reg.Active() {
		return nil
	}
	return d.reg.Start()
}

// Sleep 停心跳。
func (d *heartbeatDriver) Sleep() {
	if d.reg.Active() {
		d.reg.Stop()
	}
}

// close 撤销登记（CombatComponent.CloseBuffDriver 用）。
func (d *heartbeatDriver) close() { d.reg.Close() }
