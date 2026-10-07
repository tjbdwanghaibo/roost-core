package skill

import (
	"errors"
	"fmt"
)

// ErrRuntimeLimitsInvalid：RuntimeOptions 的上限之间的关系不成立（RootEventLimit 不大于被引用根数的上界），
// NewRuntime 以它 panic，RestoreRuntime 把它与 ErrCheckpointCorrupt 一起返回。
var ErrRuntimeLimitsInvalid = errors.New("skill: invalid runtime limits")

// RuntimeRetentionStats exposes bounded-state pressure for production
// monitoring without leaking mutable runtime internals.
type RuntimeRetentionStats struct {
	Casts                int
	CompletedCasts       int
	RootEvents           int
	ProcLedgerEntries    int
	RuntimeEvents        int
	RuntimeEventsDropped uint64
	// StopPendingSpawns counts spawns the Host failed to stop that the
	// Runtime still owns (bounded by MaxStopPendingSpawns);
	// StopRetryExhaustedSpawns is the subset whose retries hit
	// SpawnStopRetryLimit and are no longer retried automatically.
	// AbandonedSpawns counts records abandoned past MaxStopPendingSpawns
	// that are still kept (bounded by MaxAbandonedSpawns at the end of
	// Advance); the Runtime no longer stops them.
	StopPendingSpawns        int
	StopRetryExhaustedSpawns int
	AbandonedSpawns          int
}

func (runtime *Runtime) RetentionStats() RuntimeRetentionStats {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	stats := RuntimeRetentionStats{Casts: len(runtime.casts), CompletedCasts: len(runtime.completedCastOrder), RootEvents: len(runtime.rootEventCounts), ProcLedgerEntries: len(runtime.procLedger), RuntimeEvents: len(runtime.runtimeEvents), RuntimeEventsDropped: runtime.runtimeEventDropped}
	stats.StopPendingSpawns = runtime.spawns.count(spawnStopPending)
	runtime.spawns.each(func(spawn *SpawnInstance) {
		if spawn.stopRetryExhausted {
			stats.StopRetryExhaustedSpawns++
		}
	}, spawnStopPending)
	stats.AbandonedSpawns = runtime.spawns.count(spawnAbandoned)
	return stats
}

func (runtime *Runtime) trackCompletedCastLocked(cast *castInstance) {
	if cast == nil || cast.abilityFinished == false {
		return
	}
	if runtime.activeCastCount > 0 {
		runtime.activeCastCount--
	}
	// 所有终态 cast（finished 与 failed，不论是否已提交）都进完成队列，按 CompletedCastLimit 回收；
	// checkpoint 恢复按同一规则重建队列（restoreCheckpointPayload 把 finished / failed 都当作完成）。
	// 之前只收“已提交的 failed”，提交前失败（commit 付费不足、Cancel / Release 回调失败）的 cast
	// 永不回收，累计超过上限后 checkpoint 恢复判 corrupt（NC-117）。startLocked 删除并复用 ID 的
	// 失败启动随后用 forgetCompletedCastLocked 撤掉这里登记的条目。
	if cast.status != CastFinished && cast.status != CastFailed {
		return
	}
	runtime.completedCastOrder = append(runtime.completedCastOrder, cast.id)
	runtime.pruneCompletedCastsLocked()
}

// forgetCompletedCastLocked 从完成队列撤掉一个已被删除的 cast，避免它的 ID 被复用后队列里留下
// 指向新 cast 的旧条目（NC-117）。
func (runtime *Runtime) forgetCompletedCastLocked(id CastID) {
	for index, completed := range runtime.completedCastOrder {
		if completed == id {
			runtime.completedCastOrder = append(runtime.completedCastOrder[:index], runtime.completedCastOrder[index+1:]...)
			return
		}
	}
}

// castHasRunningSpawnLocked 报告 cast 名下是否还有在宿主侧运行的衍生物（含已移交的，以及停止失败、等 Runtime
// 重试停止的 stop_pending）。已停的衍生物记录不算：它们只是历史，随 cast 一起回收（forgetCastSpawnsLocked）。
// 已放弃的也不算（维护者第十三轮“待停止上限”选 B）：Runtime 不再推进、不再重试它，没有任何路径要再经它的 cast
// 求值或发表现，所以 cast 照 RR-20261006-23 的规则回收；放弃的记录自带 Program，不随 cast 删，只在 Advance 末尾
// 按 MaxAbandonedSpawns 清理。
func (runtime *Runtime) castHasRunningSpawnLocked(id CastID) bool {
	running := false
	runtime.spawns.each(func(spawn *SpawnInstance) {
		running = running || spawn.CastID == id
	}, spawnLivePartitions...)
	return running
}

// castHasAbandonedSpawnLocked 报告 cast 名下是否有已放弃的衍生物记录：未提交的失败启动据此决定能不能还 cast ID。
func (runtime *Runtime) castHasAbandonedSpawnLocked(id CastID) bool {
	found := false
	runtime.spawns.each(func(spawn *SpawnInstance) {
		found = found || spawn.CastID == id
	}, spawnAbandoned)
	return found
}

// forgetCastSpawnsLocked 在 cast 被删除之前删掉它名下已停止的衍生物记录（调用方已确认没有运行中的衍生物）。
// 已放弃的记录不在这里删：它们只在 Advance 末尾按 MaxAbandonedSpawns 清理（pruneAbandonedSpawnsLocked）。
// 衍生物停止后记录一直留在已停止分区里；之前没有任何路径删它们：
//   - 未提交的失败启动删 cast、还 ID 后，旧记录挂到下一个 cast 名下，entity 衍生物停止时又清掉了 Program，
//     Checkpoint 找不到它的程序直接报 corrupt（RR-20261006-21）；
//   - castEvictableLocked 把已停的记录也当作引用，起过衍生物的 cast 永不回收：live Runtime 的 cast 与衍生物记录无界增长，
//     完成队列超过 CompletedCastLimit 后 checkpoint 恢复判 corrupt（RR-20261006-23）。
func (runtime *Runtime) forgetCastSpawnsLocked(id CastID) {
	for _, spawnID := range runtime.spawns.sortedIDs(func(spawn *SpawnInstance) bool { return spawn.CastID == id }, spawnStopped) {
		runtime.spawns.drop(spawnID)
	}
}

func (runtime *Runtime) pruneCompletedCastsLocked() {
	limit := runtime.options.CompletedCastLimit
	for len(runtime.completedCastOrder) > limit {
		evicted := false
		for index, id := range runtime.completedCastOrder {
			cast := runtime.casts[id]
			if cast == nil {
				runtime.completedCastOrder = append(runtime.completedCastOrder[:index], runtime.completedCastOrder[index+1:]...)
				evicted = true
				break
			}
			if !runtime.castEvictableLocked(cast) {
				continue
			}
			runtime.forgetCastSpawnsLocked(id)
			delete(runtime.casts, id)
			runtime.completedCastOrder = append(runtime.completedCastOrder[:index], runtime.completedCastOrder[index+1:]...)
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

func (runtime *Runtime) castEvictableLocked(cast *castInstance) bool {
	if cast == nil || cast.pendingTasks != 0 || cast.policyActive || (cast.status != CastFinished && cast.status != CastFailed) {
		return false
	}
	// 运行中的衍生物（包括移交后仍在运行的）仍引用这个 cast；已停的记录随 cast 一起删（RR-20261006-23）。
	return !runtime.castHasRunningSpawnLocked(cast.id)
}

// rootEventReferenceBound 是同一时刻被施法 / 衍生物引用（rootEventReferencedLocked）的不同根数的上界：
//   - 未结束的施法：每个施法引用一个根，未结束的施法数不超过 MaxActiveCasts（startLocked 准入；每条终态路径都经
//     markAbilityCastFinished 减计数）；
//   - entity 衍生物（施放中、已移交、待停止）：不超过 MaxOwnedSpawns（hasOwnedSpawnCapacityExcluding 按全部仍由 Runtime
//     负责的分区计数）；
//   - phase / cast 作用域的衍生物：施放中的跟所属施法同一个根，已算在施法里；所属施法结束后只可能留成待停止，
//     不超过 MaxStopPendingSpawns（makeRoomForStopPendingLocked）；
//   - 已停止 / 已放弃的记录不引用根，MaxAbandonedSpawns 不进上界。
//
// 排程任务（未执行的被动激活、QueueExternalEvent 排的外部事件、能力覆盖到期、已结束施法残留的任务）也引用根，但它们的
// 数量没有配置上限，不在上界里；表被它们占满时由 dispatchEvent 的兜底跳过事件（RR-20261006-55 后续）。
func rootEventReferenceBound(options RuntimeOptions) int64 {
	return int64(options.MaxActiveCasts) + int64(options.MaxOwnedSpawns) + int64(options.MaxStopPendingSpawns)
}

// validateRootEventLimit 要求 RootEventLimit 大于 rootEventReferenceBound：表满时至少有一个根不被施法 / 衍生物引用、
// 可以淘汰，“表满且全部被施法 / 衍生物引用”在合法配置下不会发生。NewRuntime 与 RestoreRuntime 在默认值补齐之后调用。
func validateRootEventLimit(options RuntimeOptions) error {
	bound := rootEventReferenceBound(options)
	if int64(options.RootEventLimit) > bound {
		return nil
	}
	return fmt.Errorf("%w: RootEventLimit (%d) must exceed MaxActiveCasts (%d) + MaxOwnedSpawns (%d) + MaxStopPendingSpawns (%d) = %d, the most roots casts and spawns can reference at once; raise RootEventLimit or lower those limits",
		ErrRuntimeLimitsInvalid, options.RootEventLimit, options.MaxActiveCasts, options.MaxOwnedSpawns, options.MaxStopPendingSpawns, bound)
}

// trackRootEventLocked 把根记进根事件表；表满时淘汰最早一个不再被引用的根（连同它的 proc 账本）。仍然淘汰不出来时
// 返回 ErrRuntimeCapacityExceeded：validateRootEventLimit 保证施法 / 衍生物占不满，只有排程任务钉住的根能走到这里。
func (runtime *Runtime) trackRootEventLocked(root EventID) error {
	if _, exists := runtime.rootEventCounts[root]; exists {
		runtime.rootEventCounts[root]++
		return nil
	}
	for len(runtime.rootEventCounts) >= runtime.options.RootEventLimit {
		if !runtime.evictInactiveRootLocked(root) {
			return ErrRuntimeCapacityExceeded
		}
	}
	runtime.rootEventCounts[root] = 1
	runtime.rootEventOrder = append(runtime.rootEventOrder, root)
	return nil
}

func (runtime *Runtime) evictInactiveRootLocked(exclude EventID) bool {
	for index, root := range runtime.rootEventOrder {
		if root == exclude || runtime.rootEventReferencedLocked(root) {
			continue
		}
		delete(runtime.rootEventCounts, root)
		for key := range runtime.procLedger {
			if key.Root == root {
				delete(runtime.procLedger, key)
			}
		}
		runtime.rootEventOrder = append(runtime.rootEventOrder[:index], runtime.rootEventOrder[index+1:]...)
		return true
	}
	return false
}

func (runtime *Runtime) rootEventReferencedLocked(root EventID) bool {
	for _, cast := range runtime.casts {
		if cast != nil && cast.eventContext.RootEventID == root && cast.status != CastFinished && cast.status != CastFailed {
			return true
		}
	}
	// 衍生物继承施法的根事件（RR-20261006-53）：Runtime 仍负责的衍生物还会在这个根下产生事件，根不能被淘汰，否则
	// once_per_root 的账本随根一起删掉、同一个根再触发一次。已停止 / 已放弃的记录不再产生事件，不钉住根。
	referenced := false
	runtime.spawns.each(func(spawn *SpawnInstance) {
		referenced = referenced || spawn.eventContext.RootEventID == root
	}, spawnLivePartitions...)
	if referenced {
		return true
	}
	for _, task := range runtime.scheduler.tasks {
		if runtime.scheduledTaskRootLocked(task.Payload) == root {
			return true
		}
	}
	return false
}

func (runtime *Runtime) scheduledTaskRootLocked(payload scheduledTaskPayload) EventID {
	switch task := payload.(type) {
	case *passiveActivationTask:
		return task.Event.RootEventID
	case *externalEventTask:
		return task.Event.RootEventID
	case *abilityOverlayExpiryTask:
		return task.Context.RootEventID
	default:
		castID, _ := scheduledTaskIdentity(payload)
		if cast := runtime.casts[castID]; cast != nil {
			return cast.eventContext.RootEventID
		}
		return 0
	}
}
