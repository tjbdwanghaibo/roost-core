package level

import (
	"math"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// NextDeadline 返回逻辑期限；物理关卡用 tick，墙钟关卡用 Unix 毫秒。没有期限时不登记唤醒。
func (c *LevelComponent) NextDeadline() (int64, bool) {
	s := c.dao.state
	next := int64(math.MaxInt64)
	found := false
	if s.Phase == Preparing || s.Phase == Closing {
		next = s.Deadline
		found = true
	}
	for _, timer := range s.Timers {
		if !found || timer.Due < next {
			next = timer.Due
			found = true
		}
	}
	return next, found
}

// OnBeat 可直接交给实体 heartbeat.Host.Register；实际 Start/Stop 由实体生命周期装配。
// 使用消息冻结业务时间；离线到期只消费每个单次定时器一次，不循环追帧。
func (c *LevelComponent) OnBeat(beat heartbeat.Beat) error {
	if c.program.definition.TimeDomain != "millisecond" {
		return ErrDefinition
	}
	return c.Advance(beat.Now.UnixMilli())
}
func (c *LevelComponent) AdvanceTick(tick uint64) error {
	if c.program.definition.TimeDomain != "tick" || tick > math.MaxInt64 {
		return ErrDefinition
	}
	return c.Advance(int64(tick))
}
func (c *LevelComponent) NextWakeup() (time.Time, bool) {
	if c.program.definition.TimeDomain != "millisecond" {
		return time.Time{}, false
	}
	due, ok := c.NextDeadline()
	return time.UnixMilli(due), ok
}
