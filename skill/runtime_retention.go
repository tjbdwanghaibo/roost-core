package skill

// RuntimeRetentionStats exposes bounded-state pressure for production
// monitoring without leaking mutable runtime internals.
type RuntimeRetentionStats struct {
	Casts                int
	CompletedCasts       int
	RootEvents           int
	ProcLedgerEntries    int
	RuntimeEvents        int
	RuntimeEventsDropped uint64
}

func (runtime *Runtime) RetentionStats() RuntimeRetentionStats {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return RuntimeRetentionStats{Casts: len(runtime.casts), CompletedCasts: len(runtime.completedCastOrder), RootEvents: len(runtime.rootEventCounts), ProcLedgerEntries: len(runtime.procLedger), RuntimeEvents: len(runtime.runtimeEvents), RuntimeEventsDropped: runtime.runtimeEventDropped}
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
	for _, process := range runtime.processes {
		if process != nil && process.CastID == cast.id {
			return false
		}
	}
	for _, process := range runtime.ownedProcesses {
		if process != nil && process.CastID == cast.id {
			return false
		}
	}
	return true
}

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
	for _, process := range runtime.processes {
		if process != nil && process.eventContext.RootEventID == root {
			return true
		}
	}
	for _, process := range runtime.ownedProcesses {
		if process != nil && process.eventContext.RootEventID == root {
			return true
		}
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
