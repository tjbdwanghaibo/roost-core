package skill

import (
	"errors"
	"fmt"
	"time"
)

// TickClock 是业务时间与技能 tick 的换算规则（DESIGN-TODO D20 P5）：tick 是离散的整数时刻，Runtime、buff 的到期
// （combat.BuffInstance.DueTick）、冷却、衍生物都按它计；心跳与 handler 把各自冻结的“现在”（fctx.Now，挂了场景时间域
// 时是域时间）经它换成 tick 再推进。
//
//	tick(t) = floor((t − Origin) / Length)
//
// 向下取整：tick k 覆盖 [Origin + k·Length, Origin + (k+1)·Length)，时刻落在 tick 内部时仍算这个 tick，只有走完一整个
// Length 才进下一个 tick——所以同一时刻在任何地方换算都得到同一个 tick，tick 也不会因为换算方式而提前。
//
// Origin 缺省（零值）是 Unix 纪元：tick 是绝对的，DAO 里存下的 DueTick、Runtime checkpoint 的 CurrentTick 跨进程
// 重启仍然有意义（停服期间照样过去）。只在内存里、随对局开始与结束的 Runtime 可以把 Origin 设成对局开始时刻，tick
// 从 0 数起；持久的 tick 不能换 Origin，也不能换 Length（换了以后存下的 tick 对应的时刻全变了）。
type TickClock struct {
	// Length 是一个 tick 的时长，必须在 [time.Millisecond, time.Second] 内。
	Length time.Duration
	// Origin 是 tick 0 的起点；零值表示 Unix 纪元（1970-01-01T00:00:00Z）。
	Origin time.Time
}

// ErrTickClockInvalid 表示 TickClock 的参数不合法。
var ErrTickClockInvalid = errors.New("skill: invalid tick clock")

// Validate 检查参数。
func (c TickClock) Validate() error {
	if c.Length < time.Millisecond || c.Length > time.Second {
		return fmt.Errorf("%w: tick length %v is outside [1ms, 1s]", ErrTickClockInvalid, c.Length)
	}
	return nil
}

func (c TickClock) originNano() int64 {
	if c.Origin.IsZero() {
		return 0
	}
	return c.Origin.UnixNano()
}

// TickAt 返回时刻 t 所在的 tick（向下取整，早于 Origin 的时刻得到负数）。Length 不合法时 panic（装配错误，先 Validate）。
func (c TickClock) TickAt(t time.Time) Tick {
	if c.Length <= 0 {
		panic(fmt.Errorf("%w: tick length %v", ErrTickClockInvalid, c.Length))
	}
	delta := t.UnixNano() - c.originNano()
	tick := delta / int64(c.Length)
	if delta%int64(c.Length) < 0 {
		tick-- // Go 的整除向零截断；负数要向下
	}
	return Tick(tick)
}

// StartOf 返回 tick 的起点时刻（TickAt(StartOf(k)) == k）。
func (c TickClock) StartOf(tick Tick) time.Time {
	return time.Unix(0, c.originNano()+int64(tick)*int64(c.Length))
}

// Ticks 把一段时长换成 tick 数，向上取整（buff 时长、冷却按“至少这么久”配置：250ms 在 100ms 的 tick 下是 3 tick）。
// d <= 0 返回 0。
func (c TickClock) Ticks(d time.Duration) Tick {
	if d <= 0 {
		return 0
	}
	return Tick((d + c.Length - 1) / c.Length)
}
