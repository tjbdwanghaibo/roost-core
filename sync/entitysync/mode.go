package entitysync

import (
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// SyncMode 只改变调度时机，内容、订阅、版本与线协议共用原有实现。
type SyncMode uint8

const (
	ModePeriodic SyncMode = iota
	ModeOnChange
)

func ParseSyncMode(value string) (SyncMode, error) {
	switch value {
	case "", "periodic":
		return ModePeriodic, nil
	case "on_change":
		return ModeOnChange, nil
	}
	return ModePeriodic, fmt.Errorf("entitysync: unsupported sync mode %q", value)
}
func (mode SyncMode) String() string {
	switch mode {
	case ModePeriodic:
		return "periodic"
	case ModeOnChange:
		return "on_change"
	}
	return "invalid"
}
func (m *Manager) Mode() SyncMode          { return m.config.Mode }
func (m *Manager) Interval() time.Duration { return m.config.Interval }

// BindSyncProducer 由正式 Nest option 或实现同一提交协议的执行器在启动前调用。
func (m *Manager) BindSyncProducer() { m.producerBound.Store(true) }

// WakeSync 合并唤醒；pending 是事实来源，通知只负责降低延迟。
func (m *Manager) WakeSync() {
	if m == nil || m.config.Mode != ModeOnChange {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) CaptureSync(state *entity.SubjectSyncState) error {
	if m == nil || m.config.Mode != ModeOnChange {
		return nil
	}
	subj := m.subject(state.SubjectID())
	if subj == nil || subj.state != state {
		return nil
	}
	subj.mu.Lock()
	delta, snapshot := subj.profilesLocked(nil)
	retiring := subj.retiring
	subj.mu.Unlock()
	if retiring {
		return nil
	}
	var started time.Time
	if m.config.Trace != nil {
		started = time.Now()
	}
	err := state.FreezeSyncViews(delta, snapshot, m.reserveFrozen)
	if m.config.Trace != nil {
		stage := "frozen"
		if err != nil {
			stage = "freeze_failed"
		}
		m.config.Trace.Record(SyncTraceEvent{Stage: stage, SubjectID: state.SubjectID(), Duration: time.Since(started)})
	}
	if m.subject(state.SubjectID()) != subj {
		state.DiscardFrozenSync()
	}
	if err != nil {
		m.frozenDeferred.Add(1)
	}
	return err
}

func (m *Manager) reserveFrozen(bytes int) (func(), bool) {
	size := int64(bytes)
	for {
		used := m.frozenBytes.Load()
		if size > m.config.MaxFrozenBytes-used {
			return nil, false
		}
		if m.frozenBytes.CompareAndSwap(used, used+size) {
			for peak := m.frozenPeakBytes.Load(); used+size > peak; peak = m.frozenPeakBytes.Load() {
				if m.frozenPeakBytes.CompareAndSwap(peak, used+size) {
					break
				}
			}
			return sync.OnceFunc(func() { m.frozenBytes.Add(-size) }), true
		}
	}
}
