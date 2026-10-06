package skill

import (
	"log/slog"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 停止失败的衍生物由 Runtime 负责到底（RR-20261006-21 后续，维护者 2026-10-06：“如果是技能本身的问题是不是技能
// 自己处理比较好”）。
//
// 施法失败（failCastLocked）时 Runtime 让宿主停掉这次施法启动的衍生物。宿主 StopSpawn 失败，衍生物仍在宿主侧运行；
// 之前 Runtime 把错误还给调用方后就不再管它，宿主不自己清理的话召唤物 / 区域 / 飞行物一直留在场景里。现在：
//
//  1. 记录：failCastLocked 停完之后，本 cast 名下仍在运行的衍生物标成 SpawnStopPending；tick 驱动的停止
//     （移交后的衍生物到期 / 失效、未移交 entity 衍生物的 lifecycle 消失）被宿主拒绝时同样处理（deferRefusedStopLocked，
//     RR-20261006-31），并从 owned 表摘掉。不新建状态表：记录还是
//     runtime.spawns 里那一条，按 RR-21 / RR-23 的生命周期钉住 cast（castHasRunningSpawnLocked），cast ID
//     不复用；衍生物不再步进、不派发信号、不跑回调。
//  2. 重试：advanceHost 每推进到一个 tick，对到期的待停止衍生物重试 StopSpawn。第一次重试在
//     SpawnStopRetryBackoff 个 tick 之后，之后每失败一次间隔翻倍，最多翻到 64 倍。成功后衍生物进入停止状态
//     （发 spawn_stop 表现），不再钉住 cast，随后按 RR-23 的规则（CompletedCastLimit）连同记录回收。
//  3. 上限：失败的重试达到 SpawnStopRetryLimit 次后不再自动重试，记 skill.spawn.stop_retry_exhausted.total、
//     写一条 Warn 日志，记录保留（StateSnapshot 里状态仍是 stop_pending，RetentionStats 计数），运维可见。
//     Shutdown / RemoveProgram 仍会再停一次。
//  4. 内存：待停止条目最多 MaxStopPendingSpawns 条。新条目会超限时，先丢最早的已到上限的条目，没有就丢最早的
//     仍在重试的条目：删除它的记录（state mutation 发 spawn_remove，cast 不再被钉住、按 CompletedCastLimit 回收），
//     记 skill.spawn.stop_pending_dropped.total、写一条 Error 日志；Runtime 从此不再负责停它。
//
// 宿主侧要求 StopSpawn 幂等（Host 契约）：重试、Shutdown、RemoveProgram 都可能对同一个衍生物再停一次。
// 重试只发生在 Advance 推进 tick 时，结果是 tick 与宿主应答的确定函数，回放与 checkpoint 恢复后行为一致；
// 重试状态（次数、下一次时刻、是否已到上限）随衍生物记录写进 checkpoint。
const (
	MetricSpawnStopRetryExhausted = "skill.spawn.stop_retry_exhausted.total"
	MetricSpawnStopPendingDropped = "skill.spawn.stop_pending_dropped.total"

	// spawnStopRetryMaxDoublings 限制退避翻倍次数：默认 4 tick 起，最长间隔 256 tick。
	spawnStopRetryMaxDoublings = 6
)

// spawnStopRetryDelay 返回第 attempts 次失败之后到下一次重试的间隔（attempts 从 0 起：进入待停止时）。
func spawnStopRetryDelay(base Tick, attempts int) Tick {
	delay := base
	for doubling := 0; doubling < attempts && doubling < spawnStopRetryMaxDoublings; doubling++ {
		delay = saturatingTickAdd(delay, delay)
	}
	return delay
}

// deferUnstoppedSpawnsLocked 在 failCastLocked 停完衍生物之后调用：本 cast 名下仍在运行（宿主停止失败）的
// 衍生物标成待停止，交给之后的 tick 重试。已移交的衍生物不归失败处理停止，不在这里。
func (runtime *Runtime) deferUnstoppedSpawnsLocked(cast *castInstance) {
	ids := make([]SpawnID, 0)
	for id, spawn := range runtime.spawns {
		if spawn.CastID == cast.id && spawn.Status == SpawnRunning && !spawn.handedOff {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	for _, id := range ids {
		runtime.markStopPendingLocked(cast, runtime.spawns[id], StopCauseCancel)
	}
}

// deferRefusedStopLocked 用于 tick 驱动的停止（移交后的衍生物到期 / 失效 / 失败，未移交 entity 衍生物的 lifecycle
// 实体消失）：terminateSpawn 出错后衍生物仍是 running，说明宿主拒绝了 StopSpawn。之前错误一路返回 Advance，
// 衍生物仍到期，下一次 Advance 先重做它、再次失败、再次返回——Runtime 的 tick 停在原地，这个 Runtime 上所有施法
// 一起冻住，宿主每次 Advance 都被打一次（RR-20261006-31）。现在同样标成待停止交给退避重试，从 owned 表摘掉
// （Runtime 不再推进它），错误照常返回这一次。cast 为 nil 表示经 detachedSpawnCast 发表现（已移交）。
func (runtime *Runtime) deferRefusedStopLocked(cast *castInstance, spawn *SpawnInstance, cause StopCause) {
	if spawn == nil || spawn.Status != SpawnRunning {
		return
	}
	delete(runtime.ownedSpawns, spawn.ID)
	runtime.markStopPendingLocked(cast, spawn, cause)
}

func (runtime *Runtime) markStopPendingLocked(cast *castInstance, spawn *SpawnInstance, cause StopCause) {
	runtime.makeRoomForStopPendingLocked()
	spawn.Status = SpawnStopPending
	spawn.stopCause = cause
	spawn.stopRetryAttempts, spawn.stopRetryExhausted = 0, false
	spawn.stopRetryTick = saturatingTickAdd(runtime.currentTick, spawnStopRetryDelay(runtime.options.SpawnStopRetryBackoff, 0))
	// 衍生物表现仍在（宿主侧还在运行）：发一条 spawn_update 带上新状态，与 PresentationSnapshot 的 reset 一致。
	revision := runtime.host.CurrentRevision()
	if cast != nil {
		revision = cast.visibleRevision
	}
	runtime.emitSpawnPresentation(cast, spawn, PresentationSpawnUpdate, "", "", revision)
}

// makeRoomForStopPendingLocked 在新增一条待停止记录之前保证总数不超过 MaxStopPendingSpawns。
func (runtime *Runtime) makeRoomForStopPendingLocked() {
	for {
		pending := 0
		var oldestExhausted, oldestRetrying *SpawnInstance
		for _, spawn := range runtime.spawns {
			if spawn.Status != SpawnStopPending {
				continue
			}
			pending++
			if spawn.stopRetryExhausted {
				if oldestExhausted == nil || spawn.ID < oldestExhausted.ID {
					oldestExhausted = spawn
				}
			} else if oldestRetrying == nil || spawn.ID < oldestRetrying.ID {
				oldestRetrying = spawn
			}
		}
		if pending < runtime.options.MaxStopPendingSpawns {
			return
		}
		victim := oldestExhausted
		if victim == nil {
			victim = oldestRetrying
		}
		delete(runtime.spawns, victim.ID)
		delete(runtime.ownedSpawns, victim.ID)
		metrics.IncCounter(MetricSpawnStopPendingDropped, nil, 1)
		slog.Default().Error("skill: stop-pending spawn dropped at MaxStopPendingSpawns; the runtime no longer stops it and the host may still run it",
			"spawn_id", victim.ID, "cast_id", victim.CastID, "owner", victim.Owner, "lifecycle_entity", victim.LifecycleEntity,
			"retry_exhausted", victim.stopRetryExhausted, "limit", runtime.options.MaxStopPendingSpawns)
	}
}

// retrySpawnStopsLocked 在 advanceHost 推进 tick 之后调用，按 ID 顺序重试到期的待停止衍生物。重试失败不让 Advance
// 失败：错误已在第一次停止时返回过调用方，这里只计次、退避，到上限告警。
func (runtime *Runtime) retrySpawnStopsLocked() {
	ids := make([]SpawnID, 0)
	for id, spawn := range runtime.spawns {
		if spawn.Status == SpawnStopPending && !spawn.stopRetryExhausted && spawn.stopRetryTick <= runtime.currentTick {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	released := false
	for _, id := range ids {
		spawn := runtime.spawns[id]
		if spawn == nil || spawn.Status != SpawnStopPending {
			continue
		}
		// 已移交的衍生物经 detachedSpawnCast 发表现（与它移交后的增量一致，RR-20261006-22），用宿主当前 revision。
		cast := runtime.casts[spawn.CastID]
		if spawn.handedOff {
			cast = nil
		}
		err := runtime.stopSpawn(cast, spawn, spawn.stopCause)
		if spawn.Status != SpawnStopPending {
			// 宿主已停：之后的事件派发错误不影响“已停”这一事实，与 failCastLocked 忽略停止错误一致。
			released = true
			continue
		}
		spawn.stopRetryAttempts++
		if spawn.stopRetryAttempts >= runtime.options.SpawnStopRetryLimit {
			spawn.stopRetryExhausted = true
			metrics.IncCounter(MetricSpawnStopRetryExhausted, nil, 1)
			slog.Default().Warn("skill: spawn stop retries exhausted; the host still runs it and the record is kept",
				"spawn_id", spawn.ID, "cast_id", spawn.CastID, "owner", spawn.Owner, "lifecycle_entity", spawn.LifecycleEntity,
				"attempts", spawn.stopRetryAttempts, "error", err)
			continue
		}
		spawn.stopRetryTick = saturatingTickAdd(runtime.currentTick, spawnStopRetryDelay(runtime.options.SpawnStopRetryBackoff, spawn.stopRetryAttempts))
	}
	if released {
		// 停掉之后 cast 不再被钉住，按 RR-23 的规则回收（完成队列超过 CompletedCastLimit 时连同记录删除）。
		runtime.pruneCompletedCastsLocked()
	}
}
