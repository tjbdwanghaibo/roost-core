package combatcomponent

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// 技能 Runtime 的心跳（DESIGN-TODO D20 P5）：挂在实体上的 skill.Runtime（通常属于场景 / 房间实体，或单个战斗单位）
// 由该实体自己的心跳推进，不再由业务写周期消息调 Advance。有工作（Runtime.Pending：在途施法、排程任务、衍生物、
// 待重试的停止）时挂登记，每拍把 Runtime 推进到本拍时刻对应的 tick；空闲时停，空闲的 Runtime 不占调度。
//
// 换算与推进规则（确定性的前提，impl/10-time §2G）：
//
//  1. tick = TickClock.TickAt(本拍 Beat.Now)；挂了时间域时 Beat.Now 是域时间，所以时停时 tick 不动、2 倍速时走两倍。
//     handler 里的输入用本条消息冻结的时刻（fctx.Now 或 Domain.Now）换算，同一把尺子。
//  2. 忙时逐 tick 推进：从当前 tick 的下一个起，每个 tick 调一次 Advance(t)，直到本拍的 tick。Host 每个 tick 都被推进
//     恰好一次、按顺序，所以一拍覆盖 1 个还是 5 个 tick（心跳抖动、跳拍）不改变结果：同一输入序列（输入落在同样的
//     tick 上）在任何拍间隔下逐 tick 相同。
//  3. 推进中一旦空闲（Pending 为 false）就停在这个 tick，不再往前走、停心跳。空闲点由 Runtime 的状态决定，与拍无关。
//  4. 空闲之后的下一次输入（handler 经 Start / Input / Do）先一次 Advance 跳到输入的 tick（空闲时中间没有任何技能
//     工作，跳与逐 tick 的差别只在 Host 少推进几次空 tick），再执行输入、挂心跳。心跳的拍遇到空闲的 Runtime 不推进。
//  5. 追帧上限：一次最多逐 tick 推进 MaxCatchUp 换算出的 tick 数（挂了时间域时乘倍率）。落后更多（进程停顿、快池
//     过载导致连续跳拍、恢复出的 checkpoint 落后很久）时先逐 tick 推进上限那么多，剩下的一次 Advance 跳到目标：
//     到期的排程任务仍按 tick 顺序执行，只是中间的 Host tick 合并成一次。这一步计 Stats.Jumps，打限频 Warn——
//     出现它说明这段时间不满足第 2 条的逐 tick 确定性。
//  6. 时刻倒退（前一条消息冻结的时刻更晚、业务时钟被往回调）时不动，不倒退 Runtime。
//
// 与事务：Runtime 不在 Nest 的回滚域里（guide/08-skill §4）。拍里 Advance 出的错误交给 OnError、不让本拍事务回滚
// （Host 效果对战斗 DAO 的修改已随这一拍发生，回滚它们而 Runtime 不回滚只会更不一致）；下一拍从 Runtime 实际到达的
// tick 继续。handler 里的 Start / Input 同理：放在 handler 的最后一步（与 actionflow 的 runner 同一规则），之后还可能
// 失败、回滚了心跳的启动时，Runtime 里的施法留着、心跳没挂上——调一次 Sync（或下一次输入）补上。
//
// 回放：Runtime 每个 tick 的结果只取决于“哪些输入落在哪个 tick”。回放要记录输入的 tick（RecordingHost 记的是 Host
// 调用，它的 tick 就是这里的 tick），与心跳的拍无关；第 5 条的跳跃发生过的区间，回放需要按同样的 tick 跳（Stats 与
// Warn 指出发生过）。

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
	// MaxCatchUp 是一次逐 tick 推进的上限（换算成 tick，挂了时间域时乘这一拍的倍率）；0 取 max(1s, Period)，与心跳
	// Beat.Elapsed 的上限同一口径。
	MaxCatchUp time.Duration
	// Options 传给 heartbeat.Host.Register（例如 WithDurability；缺省 async）。
	Options []heartbeat.RegOption
	// TimeDomain 非 nil 时 Runtime 在场景的时间域里（DESIGN-TODO D22）：心跳挂上这个域，tick 按域时间换算，handler
	// 里的输入也取域时间——时停时 tick 不前进，2 倍速时 tick 走两倍。
	TimeDomain *timedomain.Domain
	// OnError 接收拍里 Advance 的错误与挂不上心跳的错误；nil 时打 Warn 日志。
	OnError func(error)
}

// RuntimeBeatStats 是推进的计数（只增不减）。
type RuntimeBeatStats struct {
	// Steps 是逐 tick 推进的次数（每个 tick 一次 Advance）。
	Steps int64
	// Idle 是空闲 Runtime 被输入一次推进到输入 tick 的次数（第 4 条）。
	Idle int64
	// Jumps 是超过追帧上限、剩余部分一次跳过的次数（第 5 条）；JumpedTicks 是这些跳跃合并的 tick 数。
	Jumps, JumpedTicks int64
}

// RuntimeBeat 是心跳驱动的 skill.Runtime 宿主。方法只能在 Runtime 所属实体的锁内调用（Nest handler、心跳回调，或
// 加载 / 新建的初始化阶段）。
type RuntimeBeat struct {
	runtime    *skill.Runtime
	reg        *heartbeat.Reg
	clock      skill.TickClock
	domain     *timedomain.Domain
	maxCatchUp time.Duration
	onError    func(error)
	// now 是 handler 侧的“现在”；测试替换。
	now func() time.Time
	// 计数在实体锁内写，Stats 任意 goroutine 读。
	steps, idle, jumps, jumpedTicks atomic.Int64
	// lastJumpWarn 限制跳跃 Warn 的频率（实体锁内读写）。
	lastJumpWarn time.Time
}

// NewRuntimeBeat 把 runtime 接到实体心跳上：登记一个停止状态的心跳，runtime 有工作时由 RuntimeBeat 启动。在实体
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
	if config.MaxCatchUp < config.Clock.Length {
		return nil, fmt.Errorf("combatcomponent: runtime beat catch-up limit %v is shorter than one tick (%v)", config.MaxCatchUp, config.Clock.Length)
	}
	b := &RuntimeBeat{runtime: runtime, clock: config.Clock, domain: config.TimeDomain, maxCatchUp: config.MaxCatchUp, onError: config.OnError}
	b.now = b.messageNow
	options := config.Options
	if config.TimeDomain != nil {
		options = append(slices.Clone(options), heartbeat.WithTimeDomain(config.TimeDomain))
	}
	reg, err := config.Host.Register(config.Name, config.Period, b.beat, options...)
	if err != nil {
		return nil, err
	}
	b.reg = reg
	return b, nil
}

// Runtime 返回被驱动的 Runtime；读状态（InspectCast、StateSnapshot、CooldownUntil……）直接用它，改状态的输入经
// Start / Input / Do，让 Runtime 先推进到输入的 tick、之后挂上心跳。
func (b *RuntimeBeat) Runtime() *skill.Runtime { return b.runtime }

// Reg 返回心跳登记（诊断、测试用；启停由 RuntimeBeat 管理，业务不要直接 Start / Stop）。
func (b *RuntimeBeat) Reg() *heartbeat.Reg { return b.reg }

// Stats 返回推进计数。
func (b *RuntimeBeat) Stats() RuntimeBeatStats {
	return RuntimeBeatStats{Steps: b.steps.Load(), Idle: b.idle.Load(), Jumps: b.jumps.Load(), JumpedTicks: b.jumpedTicks.Load()}
}

// Tick 返回本条消息的技能 tick（本条消息冻结的 fctx.Now，挂了时间域时是域时间，经 Clock 换算）。
func (b *RuntimeBeat) Tick() skill.Tick { return b.clock.TickAt(b.now()) }

func (b *RuntimeBeat) messageNow() time.Time {
	if b.domain != nil {
		return b.domain.Now()
	}
	return fctx.Now()
}

func (b *RuntimeBeat) rate() float64 {
	if b.domain != nil {
		return b.domain.Rate()
	}
	return 1
}

// CatchUp 把 Runtime 推进到本条消息的 tick（规则见包内说明第 2～6 条），不改心跳。读冷却、弹药这类“按当前 tick
// 判定”的状态之前调用。
func (b *RuntimeBeat) CatchUp() error {
	return b.advanceTo(b.Tick(), b.rate(), false)
}

// Sync 按 Runtime 是否还有工作启停心跳，返回挂不上的错误（Remote 共享模式：heartbeat.ErrHeartbeatRemoteShared）。
// 业务在 Remote 托管实体回到独占之后、恢复出 Runtime 之后，或一个随后回滚的 handler 改过 Runtime 之后调用。
func (b *RuntimeBeat) Sync() error {
	switch pending := b.runtime.Pending(); {
	case pending && !b.reg.Active():
		return b.reg.Start()
	case !pending && b.reg.Active():
		b.reg.Stop()
	}
	return nil
}

// Start 推进到本条消息的 tick、发起施法、按需挂心跳。施法本身的错误原样返回；施法已发起而心跳挂不上时返回施法 ID
// 与挂心跳的错误（施法留在 Runtime 里不推进，业务决定 Cancel 还是等回到独占后 Sync）。
func (b *RuntimeBeat) Start(program *skill.Program, input skill.CastInput) (skill.CastID, error) {
	var id skill.CastID
	err := b.Do(func(runtime *skill.Runtime) error {
		var err error
		id, err = runtime.Start(program, input)
		return err
	})
	return id, err
}

// Input 推进到本条消息的 tick、把输入交给施法、按需挂心跳。
func (b *RuntimeBeat) Input(cast skill.CastID, port skill.InputPort, input skill.InputPayload) error {
	return b.Do(func(runtime *skill.Runtime) error { return runtime.Input(cast, port, input) })
}

// Do 推进到本条消息的 tick、执行 fn（Cancel、Interrupt、Release、ActivatePassive、QueueExternalEvent、
// ModifyAbilityState……任何改 Runtime 的调用），然后按需启停心跳。推进失败时不执行 fn。fn 的错误与挂心跳的错误
// 一起返回（fn 失败也对齐心跳：失败的输入同样可能留下工作）。
func (b *RuntimeBeat) Do(fn func(*skill.Runtime) error) error {
	if err := b.CatchUp(); err != nil {
		return err
	}
	return errors.Join(fn(b.runtime), b.Sync())
}

// beat 是心跳回调：忙时把 Runtime 逐 tick 推进到本拍的 tick，空闲时停心跳。Advance 的错误交给 OnError，不让本拍
// 事务回滚（见包内说明“与事务”）。
func (b *RuntimeBeat) beat(beat heartbeat.Beat) error {
	rate := 1.0
	if b.domain != nil {
		rate = b.domain.Rate() // 与 Beat.Now 同一次冻结（heartbeat 读的是同一条消息的 Domain.Read）
	}
	if err := b.advanceTo(b.clock.TickAt(beat.Now), rate, true); err != nil {
		b.report(fmt.Errorf("combatcomponent: advance skill runtime: %w", err))
	}
	if err := b.Sync(); err != nil {
		b.report(err)
	}
	return nil
}

// advanceTo 是推进规则的唯一实现（包内说明第 2～6 条）。fromBeat 为 true 时空闲的 Runtime 不推进（第 4 条：只有输入
// 才把空闲的 Runtime 推到输入的 tick）。
func (b *RuntimeBeat) advanceTo(target skill.Tick, rate float64, fromBeat bool) error {
	runtime := b.runtime
	current := runtime.CurrentTick()
	if target <= current {
		return nil // 第 6 条：不倒退；同一 tick 里的工作已在上一次推进时执行
	}
	if !runtime.Pending() {
		if fromBeat {
			return nil
		}
		b.idle.Add(1)
		return runtime.Advance(target)
	}
	limit := b.catchUpTicks(rate)
	for steps := skill.Tick(0); current < target; steps++ {
		if steps == limit {
			b.jumps.Add(1)
			b.jumpedTicks.Add(int64(target - current))
			b.warnJump(current, target)
			return runtime.Advance(target)
		}
		current++
		b.steps.Add(1)
		if err := runtime.Advance(current); err != nil {
			return err
		}
		if !runtime.Pending() {
			if fromBeat {
				return nil // 第 3 条：空闲点由 Runtime 决定，拍停在这里
			}
			b.idle.Add(1) // 输入：空闲点之后照第 4 条一次跳到输入的 tick（与“拍先停在空闲点、输入再跳”结果相同）
			return runtime.Advance(target)
		}
	}
	return nil
}

// catchUpTicks 是一次逐 tick 推进的上限：MaxCatchUp × max(倍率, 1) 换算成 tick（向下取整，至少 1）。
func (b *RuntimeBeat) catchUpTicks(rate float64) skill.Tick {
	span := float64(b.maxCatchUp) * max(rate, 1)
	ticks := math.Floor(span / float64(b.clock.Length))
	if ticks >= math.MaxInt32 {
		return math.MaxInt32
	}
	return max(skill.Tick(ticks), 1)
}

// jumpWarnInterval 是跳跃 Warn 的最小间隔（与心跳跳拍 Warn 的缺省间隔相同）。
const jumpWarnInterval = 10 * time.Second

func (b *RuntimeBeat) warnJump(from, to skill.Tick) {
	now := time.Now()
	if now.Sub(b.lastJumpWarn) < jumpWarnInterval {
		return
	}
	b.lastJumpWarn = now
	slog.Default().Warn("combatcomponent: skill runtime fell behind more than the catch-up limit; the rest was advanced in one step "+
		"(per-tick determinism does not hold for this span)",
		"registration", b.reg.Name(), "from_tick", from, "to_tick", to, "jumps", b.jumps.Load(), "jumped_ticks", b.jumpedTicks.Load())
}

func (b *RuntimeBeat) report(err error) {
	if b.onError != nil {
		b.onError(err)
		return
	}
	slog.Default().Warn("combatcomponent: skill runtime heartbeat", "registration", b.reg.Name(), "error", err)
}
