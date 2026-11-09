package entitysync

import (
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// TypeSchedule 按 SubjectKind 决定内容捕获时机，不改变可见性、授权或持久门槛。
// Periodic 的 Interval 必须为正，从首次变脏起算，不随后续写入续期；OnChange 不填 Interval。
// 未列出的类型继承 Manager 原有 Mode/Interval 行为。配置在构造时复制。
type TypeSchedule struct {
	Mode     SyncMode
	Interval time.Duration
}

func copyTypeSchedules(config *ManagerConfig) error {
	for kind, rule := range config.TypeSchedules {
		if rule.Mode == ModeOnChange && config.Mode != ModeOnChange {
			return fmt.Errorf("entitysync: kind %d on_change requires manager on_change mode (shared wake/budget clock)", kind)
		}
		if rule.Mode != ModePeriodic && rule.Mode != ModeOnChange {
			return fmt.Errorf("entitysync: kind %d has invalid sync mode", kind)
		}
		if rule.Mode == ModePeriodic && rule.Interval <= 0 || rule.Mode == ModeOnChange && rule.Interval != 0 {
			return fmt.Errorf("entitysync: kind %d requires a positive periodic interval or zero on_change interval", kind)
		}
	}
	config.TypeSchedules = maps.Clone(config.TypeSchedules)
	return nil
}

func (m *Manager) typeMode(state *entity.SubjectSyncState) SyncMode {
	if rule, ok := m.config.TypeSchedules[state.SubjectKind()]; ok {
		return rule.Mode
	}
	return m.config.Mode
}

func (m *Manager) hasOnChange() bool {
	if m.config.Mode == ModeOnChange {
		return true
	}
	for _, rule := range m.config.TypeSchedules {
		if rule.Mode == ModeOnChange {
			return true
		}
	}
	return false
}

// 每个显式周期类型的状态独立计时。通知器不取 subject 锁，避免实体提交与捕获锁序反转。
// 只保留脏 subject 在 pending 中；空闲对象不做轮询/编码。第一次实现沿用 Manager tick 检查到期，
// 调度粒度为 Manager.Interval；没有额外 goroutine，也不改变其他类型的唤醒时机。
type subjectCadence struct {
	mu       sync.Mutex
	interval time.Duration
	due      time.Time
}

func (m *Manager) cadenceTime() time.Time {
	if m.cadenceClock != nil {
		return m.cadenceClock()
	}
	return time.Now()
}

func (m *Manager) makeCadence(state *entity.SubjectSyncState) *subjectCadence {
	rule, ok := m.config.TypeSchedules[state.SubjectKind()]
	if !ok || rule.Mode != ModePeriodic {
		return nil
	}
	return &subjectCadence{interval: rule.Interval}
}

func (c *subjectCadence) dirty(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.due.IsZero() {
		c.due = now.Add(c.interval)
	}
	c.mu.Unlock()
}

func (c *subjectCadence) waiting(now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.due.IsZero() {
		c.due = now.Add(c.interval)
	}
	return now.Before(c.due)
}

// 普通捕获成功才结束本批；捕获期间的新写入仍脏，从结算时开始下一批。
// snapshot-only 与失败不调用，保留原到期时间，不会被新订阅/失败反复续期。
func (c *subjectCadence) settled(state *entity.SubjectSyncState, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if state.PendingDirty() {
		c.due = now.Add(c.interval)
	} else {
		c.due = time.Time{}
	}
}
