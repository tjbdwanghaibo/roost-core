package entitysync

import "time"

// ManagerCounters 用于周期观测，只读取原子计数，不遍历实体和订阅。
// 需要实体/订阅数量时使用 Stats；各字段不是同一瞬间的事务快照。
type ManagerCounters struct {
	// SendDeferred 是被连接预算延后次数，同一订阅重试会重复计数。
	SendDeferred                                                      uint64
	FrozenPeakBytes                                                   int64
	FrozenBytes                                                       int64
	FrozenDeferred                                                    uint64
	FlushCalls, EmptyFlushes, FlushFailures                           uint64
	DirtyCaptured, SnapshotsCaptured, SnapshotsDeferred               uint64
	FramesAdmitted, CreatesAdmitted, UpdatesAdmitted, RemovesAdmitted uint64
	SessionsLost, DurabilityDeferred                                  uint64
	// OverLimitDeferred / OverLimitEvicted：累计因会话这一类已满而暂缓的新对象数、为钉住订阅让位的已持有对象数（over_limit.go）。
	OverLimitDeferred, OverLimitEvicted uint64
	// CodecResyncs：累计因增量的 codec 与会话基线不同而改发全量的次数（flush.go codecMismatchLocked）。
	CodecResyncs                                       uint64
	FlushDuration, LastFlushDuration                   time.Duration
	CaptureDuration, EncodeDuration, AdmissionDuration time.Duration
	// PolicyDuration 是 Flush 政策阶段（释放 / 重新提交通知与各政策的 Apply，例如 Interest 应用排队的位置事实）
	// 的累计耗时。排队模式下 AOI 的工作都在这里，直接调用模式下大部分在调用方线程里，不计入。
	PolicyDuration time.Duration
}

func (m *Manager) Counters() ManagerCounters {
	if m == nil {
		return ManagerCounters{}
	}
	return ManagerCounters{
		SendDeferred: m.sendDeferred.Load(),
		FrozenBytes:  m.frozenBytes.Load(), FrozenPeakBytes: m.frozenPeakBytes.Load(), FrozenDeferred: m.frozenDeferred.Load(),
		FlushCalls: m.flushCalls.Load(), EmptyFlushes: m.emptyFlushes.Load(), FlushFailures: m.flushFailures.Load(),
		DirtyCaptured: m.dirtyCaptured.Load(), SnapshotsCaptured: m.snapshotsCaptured.Load(), SnapshotsDeferred: m.snapshotsDeferred.Load(),
		FramesAdmitted: m.framesAdmitted.Load(), CreatesAdmitted: m.createsAdmitted.Load(), UpdatesAdmitted: m.updatesAdmitted.Load(), RemovesAdmitted: m.removesAdmitted.Load(),
		SessionsLost: m.sessionsLost.Load(), DurabilityDeferred: m.deferred.Load(),
		OverLimitDeferred: m.overLimitDeferred.Load(), OverLimitEvicted: m.overLimitEvicted.Load(),
		CodecResyncs:  m.codecResyncs.Load(),
		FlushDuration: time.Duration(m.flushNanos.Load()), LastFlushDuration: time.Duration(m.lastFlushNanos.Load()),
		CaptureDuration: time.Duration(m.captureNanos.Load()), EncodeDuration: time.Duration(m.encodeNanos.Load()), AdmissionDuration: time.Duration(m.admissionNanos.Load()),
		PolicyDuration: time.Duration(m.policyNanos.Load()),
	}
}
