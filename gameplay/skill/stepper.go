package skill

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"
)

// 技能 Runtime 的推进器（DESIGN-TODO D20；维护者 2026-10-21 决定：心跳只是技能 / Buff 的驱动方式之一，skill 只用
// 接口）。Stepper 持有推进规则，驱动方式经 Driver 接进来：实体心跳（combatcomponent.NewRuntimeBeat）、Lockstep 房间
// 的定步长循环、测试里手动调用，规则都是这一份。本文件不依赖 Nest、心跳、时间域：时刻由驱动（StepTo 的参数）与
// 输入侧的 StepperConfig.Now 给出，“本条消息冻结的时刻”是适配层的事。
//
// 推进规则（确定性的前提，impl/10-time §2G）：
//
//  1. tick = TickClock.TickAt(时刻)。驱动的一步用驱动给的时刻（心跳是本拍 Beat.Now，挂了时间域时是域时间），输入用
//     StepperConfig.Now（心跳适配层给的是本条消息冻结的 fctx.Now 或 Domain.Now），同一把尺子；直接给 tick 的驱动用
//     StepToTick。
//  2. 忙时逐 tick 推进：从当前 tick 的下一个起，每个 tick 调一次 Runtime.Advance(t)，直到目标 tick。Host 每个 tick
//     都被推进恰好一次、按顺序，所以驱动的一步覆盖 1 个还是 5 个 tick（心跳抖动、跳拍、循环帧长不同）不改变结果：
//     同一输入序列（输入落在同样的 tick 上）在任何驱动节奏下逐 tick 相同。
//  3. 推进中一旦空闲（Runtime.Pending 为 false）就停在这个 tick，不再往前走，通知驱动 Sleep。空闲点由 Runtime 的
//     状态决定，与驱动的节奏无关。
//  4. 空闲之后的下一次输入（Start / Input / Do）先一次 Advance 跳到输入的 tick（空闲时中间没有任何技能工作，跳与
//     逐 tick 的差别只在 Host 少推进几次空 tick），再执行输入、通知驱动 Wake。驱动的一步遇到空闲的 Runtime 不推进。
//  5. 追帧上限：一步最多逐 tick 推进 MaxCatchUp 换算出的 tick 数（乘 max(倍率, 1)）。落后更多（进程停顿、过载导致
//     连续跳拍、恢复出的 checkpoint 落后很久）时先逐 tick 推进上限那么多，剩下的一次 Advance 跳到目标：到期的排程
//     任务仍按 tick 顺序执行，只是中间的 Host tick 合并成一次。这一步计 Stats.Jumps，打限频 Warn——出现它说明这段
//     时间不满足第 2 条的逐 tick 确定性。
//  6. 时刻倒退（目标不晚于当前 tick：晚到的拍、业务时钟被往回调）时不动，不倒退 Runtime。
//
// 驱动的通知是电平式的：每次推进与输入之后，有工作就调 Wake、没有就调 Sleep（不只在边沿调），驱动自己去重。这样
// 驱动的状态被外部改掉之后（心跳的启动随回滚作废、Remote 共享模式停掉了心跳）下一次输入或 Sync 就能补上，
// Stepper 不需要猜驱动现在处于什么状态。
//
// 回放：Runtime 每个 tick 的结果只取决于“哪些输入落在哪个 tick”。回放要记录输入的 tick（RecordingHost 记的是 Host
// 调用，它的 tick 就是这里的 tick），与驱动的节奏无关；第 5 条的跳跃发生过的区间，回放需要按同样的 tick 跳（Stats
// 与 Warn 指出发生过）。

// Driver 是 Stepper 的驱动方式：决定什么时候、按什么时刻调 Stepper.StepTo / StepToTick。Stepper 在每次推进与输入之后
// 按 Runtime 是否还有工作调 Wake（有）或 Sleep（没有）；调用是电平式的，同一状态可能连续调多次，实现必须幂等。
// 两个方法在 Stepper 的调用方所在的上下文里同步调用（心跳适配层是实体锁内），不能阻塞。
type Driver interface {
	// Wake 请求驱动开始（或继续）推进。返回错误表示驱动挂不上（例如 Remote 共享模式不允许挂心跳），工作留在
	// Runtime 里不推进，之后 Sync 重试。
	Wake() error
	// Sleep 告诉驱动 Runtime 已空闲，可以停止推进。
	Sleep()
}

// DefaultMaxCatchUp 是 StepperConfig.MaxCatchUp 的缺省值。
const DefaultMaxCatchUp = time.Second

// StepperConfig 是 NewStepper 的参数。
type StepperConfig struct {
	// Clock 把时刻换成技能 tick，与 buff、StatusBridge.CurrentTick 用同一个。必填。
	Clock TickClock
	// Now 是输入侧的“现在”（Start / Input / Do / CatchUp / Tick 用它换算输入的 tick）。必填。心跳适配层给的是本条
	// 消息冻结的 fctx.Now 或 Domain.Now；定步长循环给的是当前帧的时刻（例如 Clock.StartOf(帧 tick)）。
	Now func() time.Time
	// Rate 是输入侧的倍率（只用于追帧上限，第 5 条）；nil 表示 1。挂了时间域时给 Domain.Rate。
	Rate func() float64
	// Driver 接收 Wake / Sleep；nil 表示不需要通知（驱动每一步都推进，例如测试里手动调用）。
	Driver Driver
	// MaxCatchUp 是一步逐 tick 推进的上限（换算成 tick，乘这一步的 max(倍率, 1)）；0 取 DefaultMaxCatchUp。不能短于
	// 一个 tick。
	MaxCatchUp time.Duration
	// Name 出现在跳跃 Warn 里，标识这个推进器（心跳适配层给的是心跳登记名）。
	Name string
}

// StepperStats 是推进的计数（只增不减）。
type StepperStats struct {
	// Steps 是逐 tick 推进的次数（每个 tick 一次 Advance）。
	Steps int64
	// Idle 是空闲 Runtime 被输入一次推进到输入 tick 的次数（第 4 条）。
	Idle int64
	// Jumps 是超过追帧上限、剩余部分一次跳过的次数（第 5 条）；JumpedTicks 是这些跳跃合并的 tick 数。
	Jumps, JumpedTicks int64
}

// Stepper 按包内说明的规则推进一个 Runtime，与驱动方式无关。方法只能在 Runtime 的所属上下文里串行调用（心跳适配层
// 是 Runtime 所属实体的锁内；定步长循环是循环自己的线程）；Stats 任意 goroutine 可读。
type Stepper struct {
	runtime    *Runtime
	clock      TickClock
	now        func() time.Time
	rate       func() float64
	driver     Driver
	maxCatchUp time.Duration
	name       string
	// 计数在所属上下文里写，Stats 任意 goroutine 读。
	steps, idle, jumps, jumpedTicks atomic.Int64
	// lastJumpWarn 限制跳跃 Warn 的频率（所属上下文里读写）。
	lastJumpWarn time.Time
}

// NewStepper 构造 runtime 的推进器。恢复出的 Runtime（RestoreRuntime）还有工作时，构造后调一次 Sync 通知驱动。
func NewStepper(runtime *Runtime, config StepperConfig) (*Stepper, error) {
	if runtime == nil {
		return nil, errors.New("skill: stepper needs a runtime")
	}
	if err := config.Clock.Validate(); err != nil {
		return nil, fmt.Errorf("skill: stepper: %w", err)
	}
	if config.Now == nil {
		return nil, errors.New("skill: stepper needs Now (the time inputs are taken at)")
	}
	if config.MaxCatchUp == 0 {
		config.MaxCatchUp = DefaultMaxCatchUp
	}
	if config.MaxCatchUp < config.Clock.Length {
		return nil, fmt.Errorf("skill: stepper catch-up limit %v is shorter than one tick (%v)", config.MaxCatchUp, config.Clock.Length)
	}
	return &Stepper{runtime: runtime, clock: config.Clock, now: config.Now, rate: config.Rate, driver: config.Driver,
		maxCatchUp: config.MaxCatchUp, name: config.Name}, nil
}

// Runtime 返回被推进的 Runtime；读状态（InspectCast、StateSnapshot、CooldownUntil……）直接用它，改状态的输入经
// Start / Input / Do，让 Runtime 先推进到输入的 tick、之后通知驱动。
func (s *Stepper) Runtime() *Runtime { return s.runtime }

// Clock 返回换算用的 TickClock。
func (s *Stepper) Clock() TickClock { return s.clock }

// Stats 返回推进计数。
func (s *Stepper) Stats() StepperStats {
	return StepperStats{Steps: s.steps.Load(), Idle: s.idle.Load(), Jumps: s.jumps.Load(), JumpedTicks: s.jumpedTicks.Load()}
}

// Tick 返回输入侧“现在”的技能 tick（StepperConfig.Now 经 Clock 换算）。
func (s *Stepper) Tick() Tick { return s.clock.TickAt(s.now()) }

func (s *Stepper) inputRate() float64 {
	if s.rate == nil {
		return 1
	}
	return s.rate()
}

// StepTo 是驱动的一步：忙时把 Runtime 逐 tick 推进到 now 对应的 tick（规则第 2、3、5、6 条），空闲时不推进，然后
// 通知驱动。rate 是这一步的倍率（只用于追帧上限）。返回推进的错误与 Wake 的错误；推进出错时 Runtime 停在实际到达的
// tick，下一步从那里继续。
func (s *Stepper) StepTo(now time.Time, rate float64) error {
	return s.StepToTick(s.clock.TickAt(now), rate)
}

// StepToTick 同 StepTo，目标直接给 tick（定步长循环按帧号推进时用）。
func (s *Stepper) StepToTick(target Tick, rate float64) error {
	return errors.Join(s.advanceTo(target, rate, true), s.Sync())
}

// CatchUp 把 Runtime 推进到输入侧“现在”的 tick（规则第 2～6 条），不通知驱动。读冷却、弹药这类“按当前 tick 判定”
// 的状态之前调用。
func (s *Stepper) CatchUp() error {
	return s.advanceTo(s.Tick(), s.inputRate(), false)
}

// Sync 按 Runtime 是否还有工作通知驱动（Wake / Sleep），返回 Wake 的错误。驱动的状态被外部改过之后调用：心跳适配层
// 里是 Remote 托管实体回到独占之后、恢复出 Runtime 之后，或一个随后回滚的 handler 改过 Runtime 之后。
func (s *Stepper) Sync() error {
	if s.driver == nil {
		return nil
	}
	if s.runtime.Pending() {
		return s.driver.Wake()
	}
	s.driver.Sleep()
	return nil
}

// Start 推进到输入的 tick、发起施法、通知驱动。施法本身的错误原样返回；施法已发起而驱动挂不上时返回施法 ID 与 Wake
// 的错误（施法留在 Runtime 里不推进，业务决定 Cancel 还是之后 Sync）。
func (s *Stepper) Start(program *Program, input CastInput) (CastID, error) {
	var id CastID
	err := s.Do(func(runtime *Runtime) error {
		var err error
		id, err = runtime.Start(program, input)
		return err
	})
	return id, err
}

// Input 推进到输入的 tick、把输入交给施法、通知驱动。
func (s *Stepper) Input(cast CastID, port InputPort, input InputPayload) error {
	return s.Do(func(runtime *Runtime) error { return runtime.Input(cast, port, input) })
}

// Do 推进到输入的 tick、执行 fn（Cancel、Interrupt、Release、ActivatePassive、QueueExternalEvent、
// ModifyAbilityState……任何改 Runtime 的调用），然后通知驱动。推进失败时不执行 fn。fn 的错误与 Wake 的错误一起返回
// （fn 失败也通知驱动：失败的输入同样可能留下工作）。
func (s *Stepper) Do(fn func(*Runtime) error) error {
	if err := s.CatchUp(); err != nil {
		return err
	}
	return errors.Join(fn(s.runtime), s.Sync())
}

// advanceTo 是推进规则的唯一实现（包内说明第 2～6 条）。fromDriver 为 true 时空闲的 Runtime 不推进（第 4 条：只有
// 输入才把空闲的 Runtime 推到输入的 tick）。
func (s *Stepper) advanceTo(target Tick, rate float64, fromDriver bool) error {
	runtime := s.runtime
	current := runtime.CurrentTick()
	if target <= current {
		return nil // 第 6 条：不倒退；同一 tick 里的工作已在上一次推进时执行
	}
	if !runtime.Pending() {
		if fromDriver {
			return nil
		}
		s.idle.Add(1)
		return runtime.Advance(target)
	}
	limit := s.catchUpTicks(rate)
	for steps := Tick(0); current < target; steps++ {
		if steps == limit {
			s.jumps.Add(1)
			s.jumpedTicks.Add(int64(target - current))
			s.warnJump(current, target)
			return runtime.Advance(target)
		}
		current++
		s.steps.Add(1)
		if err := runtime.Advance(current); err != nil {
			return err
		}
		if !runtime.Pending() {
			if fromDriver {
				return nil // 第 3 条：空闲点由 Runtime 决定，驱动的这一步停在这里
			}
			s.idle.Add(1) // 输入：空闲点之后照第 4 条一次跳到输入的 tick（与“驱动先停在空闲点、输入再跳”结果相同）
			return runtime.Advance(target)
		}
	}
	return nil
}

// catchUpTicks 是一步逐 tick 推进的上限：MaxCatchUp × max(倍率, 1) 换算成 tick（向下取整，至少 1）。
func (s *Stepper) catchUpTicks(rate float64) Tick {
	span := float64(s.maxCatchUp) * max(rate, 1)
	ticks := math.Floor(span / float64(s.clock.Length))
	if ticks >= math.MaxInt32 {
		return math.MaxInt32
	}
	return max(Tick(ticks), 1)
}

// stepperJumpWarnInterval 是跳跃 Warn 的最小间隔（与心跳跳拍 Warn 的缺省间隔相同）。限频用墙钟，与推进的结果无关。
const stepperJumpWarnInterval = 10 * time.Second

func (s *Stepper) warnJump(from, to Tick) {
	now := time.Now()
	if now.Sub(s.lastJumpWarn) < stepperJumpWarnInterval {
		return
	}
	s.lastJumpWarn = now
	slog.Default().Warn("skill: runtime fell behind more than the catch-up limit; the rest was advanced in one step "+
		"(per-tick determinism does not hold for this span)",
		"stepper", s.name, "from_tick", from, "to_tick", to, "jumps", s.jumps.Load(), "jumped_ticks", s.jumpedTicks.Load())
}
