package skill

import (
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 衍生物停止的统一状态机（停止入口统一，维护者 2026-10-07；docs/feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md）。
//
// 每个停止入口只“请求停止”（requestSpawnStop），宿主拒绝 StopSpawn 之后的处理只在这里一处：
//
//	running ──请求停止──▶ 停止中：解除 carry、区域离开信号、回调（只对 running）、宿主 StopSpawn
//	停止中 ──宿主已停──▶ 已停止（ended / cancelled / failed），进已停止分区；记录随 cast 按 RR-20261006-23 的规则回收
//	停止中 ──宿主拒绝──▶ stop_pending，进待停止分区；Runtime 不再推进它（不步进、不派发信号、不跑回调）
//	stop_pending ──重试到期 / 再次请求──▶ 停止中（只重发宿主 StopSpawn；停止原因沿用第一次请求）
//	stop_pending ──失败的重试达到上限──▶ stop_pending（exhausted）：告警、记录保留，不再自动重试；再次请求仍会停
//	stop_pending ──待停止条目超过 MaxStopPendingSpawns──▶ 记录删除：告警，Runtime 不再负责停它
//
// 停止入口（spawn_stop_entries_promises_test.go 的 spawnStopEntries 逐个登记，守卫核对源码里调用 requestSpawnStop
// 的函数与登记表一致）：
//   - 施法里的停止：failCastLocked、goto、Cancel、Interrupt、施法收尾（stopScopedSpawns），衍生物启动失败的清理
//     （startEntitySpawn、executeOwnedSummon），移交时 lifecycle 实体已失效（handoffEntitySpawns）；
//   - tick 驱动：施法期间 lifecycle 实体消失（reapUnhandedEntitySpawns），移交后的到期 / 失效 / 步进失败 / area 回调
//     finish（terminateOwnedSpawn）；
//   - 调用方驱动：RemoveProgram、Shutdown。
//
// 宿主拒绝时入口照常把错误返回这一次，之后由 Runtime 在 tick 上重试；入口不再各自写失败分支。之前四类入口各管各的
// （RR-20261006-21 后续、RR-20261006-31 只收了施法失败与 tick 回收）：Shutdown / RemoveProgram 把记录留成 running、
// 交给调用方重试，Runtime 从此不管；施法里的其余路径靠错误传到 failCastLocked 再停一次，第二次停止又跑一遍 cancel
// 回调（RR-20261006-32）。
//
// Shutdown 没有后续 tick 时，停不下的衍生物同样留成 stop_pending、写进 checkpoint：继续 Advance、或 Checkpoint 后
// 在新进程 RestoreRuntime 再 Advance，都会按原来的重试时刻接着停；再调一次 Shutdown 会立即再请求一次。不在 Shutdown
// 里同步重试：Runtime 是按 tick 推进的确定性状态机，没有 ctx 也不读系统时钟，原地循环只会在宿主状态不变时连打宿主，
// 按墙钟等待会让回放与 checkpoint 恢复失去确定性，还会在调用方的执行线程上阻塞。RemoveProgram 之后，程序的
// stop_pending 衍生物同样由 Runtime 在 tick 上重试：重试只重发宿主 StopSpawn，不执行程序代码，记录仍引用程序
// （checkpoint 恢复时 resolver 仍要能解析它，cast 记录本来也引用它）。
//
// 重试（retrySpawnStopsLocked，advanceHost 每推进到一个 tick 调用）：第一次在 SpawnStopRetryBackoff 个 tick 之后，
// 之后每失败一次间隔翻倍，最多翻到 64 倍；成功后 cast 不再被钉住，按 CompletedCastLimit 连同记录回收。失败的重试
// 达到 SpawnStopRetryLimit 次后记 skill.spawn.stop_retry_exhausted.total、写一条 Warn 日志；待停止条目超过
// MaxStopPendingSpawns 时先丢最早的已到上限的条目、没有就丢最早仍在重试的，记 skill.spawn.stop_pending_dropped.total、
// 写一条 Error 日志（state mutation 发 spawn_remove）。
//
// 宿主侧要求 StopSpawn 幂等（Host 契约）：重试、再次请求都可能对同一个衍生物再停一次。重试只发生在 Advance 推进
// tick 时，结果是 tick 与宿主应答的确定函数；重试状态（次数、下一次时刻、是否已到上限）随衍生物记录写进 checkpoint。
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

// requestSpawnStop 是衍生物停止的唯一入口：停止入口只调用它，宿主拒绝之后的状态迁移只在这里。
// 已停的衍生物直接返回；宿主停了，stopSpawn 已把记录挪进已停止分区；宿主拒绝就转入 stop_pending、交给 retrySpawnStopsLocked。
// 错误（宿主拒绝、回调或区域信号出错）照常返回给这次请求的调用方。cast 为 nil 时经宿主当前 revision 发表现
// （已移交的衍生物、Shutdown、tick 回收）。
func (runtime *Runtime) requestSpawnStop(cast *castInstance, spawn *SpawnInstance, cause StopCause, callbackEvent string) error {
	if !spawn.liveOnHost() {
		return nil
	}
	pending := spawn.Status == SpawnStopPending
	if pending {
		// 停止原因由第一次请求决定；回调与区域离开信号也只在第一次处理（terminateSpawn 只对 running 跑它们）。
		cause = spawn.stopCause
	}
	err := runtime.terminateSpawn(cast, spawn, cause, callbackEvent)
	if spawn.liveOnHost() && !pending {
		// terminateSpawn 总会调用 stopSpawn；之后仍是 running 只说明宿主拒绝了 StopSpawn。
		runtime.enterStopPendingLocked(cast, spawn, cause)
	}
	return err
}

// enterStopPendingLocked 把宿主拒绝停止的衍生物转入 stop_pending：挪进待停止分区（Runtime 不再推进它），排第一次重试，
// 发一条带新状态的 spawn_update（宿主侧还在运行，与 PresentationSnapshot 的 reset 一致）。
func (runtime *Runtime) enterStopPendingLocked(cast *castInstance, spawn *SpawnInstance, cause StopCause) {
	runtime.makeRoomForStopPendingLocked()
	runtime.spawns.setState(spawn, SpawnStopPending, spawn.handedOff)
	spawn.stopCause = cause
	spawn.stopRetryAttempts, spawn.stopRetryExhausted = 0, false
	spawn.stopRetryTick = saturatingTickAdd(runtime.currentTick, spawnStopRetryDelay(runtime.options.SpawnStopRetryBackoff, 0))
	revision := runtime.host.CurrentRevision()
	if cast != nil {
		revision = cast.visibleRevision
	}
	runtime.emitSpawnPresentation(cast, spawn, PresentationSpawnUpdate, "", "", revision)
}

// makeRoomForStopPendingLocked 在新增一条待停止记录之前保证总数不超过 MaxStopPendingSpawns。
func (runtime *Runtime) makeRoomForStopPendingLocked() {
	for runtime.spawns.count(spawnStopPending) >= runtime.options.MaxStopPendingSpawns {
		var oldestExhausted, oldestRetrying *SpawnInstance
		runtime.spawns.each(func(spawn *SpawnInstance) {
			if spawn.stopRetryExhausted {
				if oldestExhausted == nil || spawn.ID < oldestExhausted.ID {
					oldestExhausted = spawn
				}
			} else if oldestRetrying == nil || spawn.ID < oldestRetrying.ID {
				oldestRetrying = spawn
			}
		}, spawnStopPending)
		victim := oldestExhausted
		if victim == nil {
			victim = oldestRetrying
		}
		runtime.spawns.drop(victim.ID)
		metrics.IncCounter(MetricSpawnStopPendingDropped, nil, 1)
		slog.Default().Error("skill: stop-pending spawn dropped at MaxStopPendingSpawns; the runtime no longer stops it and the host may still run it",
			"spawn_id", victim.ID, "cast_id", victim.CastID, "owner", victim.Owner, "lifecycle_entity", victim.LifecycleEntity,
			"retry_exhausted", victim.stopRetryExhausted, "limit", runtime.options.MaxStopPendingSpawns)
	}
}

// retrySpawnStopsLocked 在 advanceHost 推进 tick 之后调用，按 ID 顺序对到期的待停止衍生物再请求一次停止。重试失败
// 不让 Advance 失败：错误已在第一次请求时返回过调用方，这里只计次、退避，到上限告警。
func (runtime *Runtime) retrySpawnStopsLocked() {
	ids := runtime.spawns.sortedIDs(func(spawn *SpawnInstance) bool {
		return !spawn.stopRetryExhausted && spawn.stopRetryTick <= runtime.currentTick
	}, spawnStopPending)
	if len(ids) == 0 {
		return
	}
	released := false
	for _, id := range ids {
		spawn := runtime.spawns.get(id, spawnStopPending)
		if spawn == nil {
			continue
		}
		// 已移交的衍生物经 detachedSpawnCast 发表现（与它移交后的增量一致，RR-20261006-22），用宿主当前 revision。
		cast := runtime.casts[spawn.CastID]
		if spawn.handedOff {
			cast = nil
		}
		err := runtime.requestSpawnStop(cast, spawn, spawn.stopCause, "")
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
