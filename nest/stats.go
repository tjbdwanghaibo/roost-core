package nest

import (
	"github.com/tjbdwanghaibo/roost-core/worker"
	"time"
)

type DispatcherWorkStats struct {
	ProcessedMessages uint64
	Slow200msMessages uint64
}

type DispatcherStats struct {
	Fast, Slow        worker.PoolStats
	FastContinuations int
	// Deprecated: Main=Fast、Remote=Slow；Heart/Cost 不再创建池。
	Main    worker.PoolStats `json:"-"`
	Heart   worker.PoolStats `json:"-"`
	Cost    worker.PoolStats `json:"-"`
	Remote  worker.PoolStats `json:"-"`
	Delayed int
	Stopped bool
	Work    DispatcherWorkStats
}

func (m *Dispatcher) Stats() DispatcherStats {
	if m == nil {
		return DispatcherStats{}
	}
	m.mu.Lock()
	delayed := len(m.delayed)
	stopped := m.stopped
	m.mu.Unlock()
	stats := DispatcherStats{
		Delayed: delayed,
		Stopped: stopped,
		Work: DispatcherWorkStats{
			ProcessedMessages: m.processed.Load(),
			Slow200msMessages: m.slow200ms.Load(),
		},
	}
	stats.Fast, stats.Slow, stats.FastContinuations = m.queue.stats()
	stats.Main, stats.Remote = stats.Fast, stats.Slow
	return stats
}

func (m *Dispatcher) recordDispatch(cost time.Duration) {
	if m == nil {
		return
	}
	m.processed.Add(1)
	if shouldLogSlowDispatch(cost) {
		m.slow200ms.Add(1)
	}
}

func (mgr *NestMgr) Stats() DispatcherStats {
	if mgr == nil || mgr.dispatcher == nil {
		return DispatcherStats{}
	}
	return mgr.dispatcher.Stats()
}

func (mgr *NestMgr) recordDispatch(cost time.Duration) {
	if mgr == nil || mgr.dispatcher == nil {
		return
	}
	mgr.dispatcher.recordDispatch(cost)
}
