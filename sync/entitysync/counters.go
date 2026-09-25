package entitysync

import "time"

// ManagerCounters 用于周期观测，只读取原子计数，不遍历实体和订阅。
// 需要实体/订阅数量时使用 Stats；各字段不是同一瞬间的事务快照。
type ManagerCounters struct {
	FrozenPeakBytes                                                   int64
	FrozenBytes                                                       int64
	FrozenDeferred                                                    uint64
	FlushCalls, EmptyFlushes, FlushFailures                           uint64
	DirtyCaptured, SnapshotsCaptured, SnapshotsDeferred               uint64
	FramesAdmitted, CreatesAdmitted, UpdatesAdmitted, RemovesAdmitted uint64
	SessionsLost, DurabilityDeferred                                  uint64
	FlushDuration, LastFlushDuration                                  time.Duration
	CaptureDuration, EncodeDuration, AdmissionDuration                time.Duration
}

func (m *Manager) Counters() ManagerCounters {
	if m == nil {
		return ManagerCounters{}
	}
	return ManagerCounters{
		FrozenBytes: m.frozenBytes.Load(), FrozenPeakBytes: m.frozenPeakBytes.Load(), FrozenDeferred: m.frozenDeferred.Load(),
		FlushCalls: m.flushCalls.Load(), EmptyFlushes: m.emptyFlushes.Load(), FlushFailures: m.flushFailures.Load(),
		DirtyCaptured: m.dirtyCaptured.Load(), SnapshotsCaptured: m.snapshotsCaptured.Load(), SnapshotsDeferred: m.snapshotsDeferred.Load(),
		FramesAdmitted: m.framesAdmitted.Load(), CreatesAdmitted: m.createsAdmitted.Load(), UpdatesAdmitted: m.updatesAdmitted.Load(), RemovesAdmitted: m.removesAdmitted.Load(),
		SessionsLost: m.sessionsLost.Load(), DurabilityDeferred: m.deferred.Load(),
		FlushDuration: time.Duration(m.flushNanos.Load()), LastFlushDuration: time.Duration(m.lastFlushNanos.Load()),
		CaptureDuration: time.Duration(m.captureNanos.Load()), EncodeDuration: time.Duration(m.encodeNanos.Load()), AdmissionDuration: time.Duration(m.admissionNanos.Load()),
	}
}
