package nest

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

// DispatcherWorkStats 是派发完成计数：总处理数与超过慢日志阈值（200ms）的条数。
type DispatcherWorkStats struct {
	ProcessedMessages uint64
	Slow200msMessages uint64
}

// DispatchLaneStats 区分 ID 顺序等待和执行资源等待；累计耗时在进入下一阶段时记账。
// Ready 包含已预留执行额度但 goroutine 尚未取走的工作。
// Rejected 只累计容量拒绝，不包含尚未启动或已经关闭的请求。
// PeakWaiting / OldestWaiting 合并两种等待；DependencyWait / WorkerWait 分阶段累计。
// TryBatchJobs 累计准入的批量作业数（DispatchTryBatch，每通道每次调用至多一个）；TryBatchConverted 累计批量作业里
// 因同 ID 冲突（有已准入未完成的作业，或实体锁取不到）当场转成单目标作业的目标数。
type DispatchLaneStats struct {
	Running, Ready, BlockedOnPredecessor, WaitingForWorker int
	PeakWaiting                                            int
	Started, Rejected                                      uint64
	TryBatchJobs, TryBatchConverted                        uint64
	DependencyWait, WorkerWait                             time.Duration
	MaxDependencyWait, MaxWorkerWait, OldestWaiting        time.Duration
}

// DispatchQueueStats 是三个通道的观测统计与 long 池指标、续行计数。
type DispatchQueueStats struct {
	Short, IO, Long     DispatchLaneStats
	LongPool            worker.PoolStats
	LongContinuations   int
	ContinuationRunning int
}

// DispatcherStats 是派发器的瞬时观测值，只用于监控与诊断。
type DispatcherStats struct {
	Queue              DispatchQueueStats
	Short, IO, Long    worker.PoolStats
	ShortContinuations int
	Delayed            int
	Stopped            bool
	Work               DispatcherWorkStats
}

// Stats 返回派发器的瞬时统计。
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
	stats.Short, stats.IO, stats.ShortContinuations, stats.Queue = m.queue.snapshotStats()
	stats.Long = stats.Queue.LongPool
	return stats
}

// recordDispatch 记录一次派发完成及是否为慢派发。
func (m *Dispatcher) recordDispatch(cost time.Duration) {
	if m == nil {
		return
	}
	m.processed.Add(1)
	if shouldLogSlowDispatch(cost) {
		m.slow200ms.Add(1)
	}
}

// Stats 返回引擎派发器的瞬时统计。
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
