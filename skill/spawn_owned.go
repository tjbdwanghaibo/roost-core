package skill

import (
	"errors"
	"sort"
)

type OwnedSpawnSnapshot struct {
	ID              SpawnID
	Owner           EntityID
	LifecycleEntity EntityID
	SourceCastID    CastID
	ProgramID       string
	StartTick       Tick
	EndTick         Tick
	Status          SpawnStatus
	HandedOff       bool
}

func (runtime *Runtime) startEntitySpawn(cast *castInstance, template SpawnTemplateIndex, unitTemplate UnitTemplateHandle, lifecycle EntityID, duration Tick, position Position) error {
	if cast == nil || lifecycle == 0 || int(template) >= len(cast.program.spawnTemplates) {
		return ErrProgramInvariant
	}
	spawnTemplate := cast.program.spawnTemplates[template]
	if spawnTemplate.motion != nil || spawnTemplate.area != nil {
		duration = spawnTemplate.durationTicks
	}
	if duration <= 0 {
		return ErrProgramInvariant
	}
	runtime.nextSpawnID++
	spawn := &SpawnInstance{
		ID: runtime.nextSpawnID, CastID: cast.id, TemplateIndex: template, UnitTemplate: unitTemplate,
		Status: SpawnRunning, StartTick: runtime.currentTick, NextTick: saturatingTickAdd(runtime.currentTick, 1), EndTick: saturatingTickAdd(runtime.currentTick, duration),
		Scope: SpawnScopeEntity, Owner: cast.caster, LifecycleEntity: lifecycle, Program: cast.program,
		HostState: SpawnHostState{SpawnID: runtime.nextSpawnID, Active: true, Position: position}, Motion: MotionState{Position: position},
		phaseToken: cast.phaseToken, locals: detachedSpawnLocals(cast.program), snapshots: make(map[int]RuntimeValue), randomKey: cast.randomKey, randomInvocations: make(map[RandomSiteIndex]uint64), visibleRevision: cast.visibleRevision,
		eventContext: EventContext{Tick: runtime.currentTick, Source: cast.caster, Owner: cast.caster, Target: lifecycle, SkillID: cast.program.id, CastID: cast.id, SpawnID: runtime.nextSpawnID},
	}
	// 启动那一步用施法本身求衍生物字段，但按 spawn_step 列查表：之后每一步在移交后的衍生物里
	// 求同样的字段，两边都要求得出（RR-20261005-NC-224）。numeric track 初值在
	// initializeSpawnNumeric 里切回施法流程。
	previous := cast.switchEvalContext(evalSpawnStep)
	signals, err := runtime.stepSpawnMotion(cast, spawn)
	cast.switchEvalContext(previous)
	if err != nil {
		return errors.Join(err, runtime.detachMotionCarry(cast, spawn))
	}
	spawn.visibleRevision = cast.visibleRevision
	runtime.spawns[spawn.ID] = spawn
	if err := runtime.drainHostEvents(cast); err != nil {
		return err
	}
	runtime.emitSpawnPresentation(cast, spawn, PresentationSpawnStart, "", "", cast.visibleRevision)
	if cast.areaCallbackFinish {
		return runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "")
	}
	if err := runtime.captureOwnedSpawnSnapshots(spawn); err != nil {
		stopErr := runtime.requestSpawnStop(cast, spawn, StopCauseFailure, "")
		if stopErr == nil {
			delete(runtime.spawns, spawn.ID)
		}
		return errors.Join(err, stopErr)
	}
	if spawnTemplate.area != nil {
		signals = areaSpawnSignals(signals)
		previous := cast.switchEvalContext(evalSpawnStep)
		areaSignals, areaErr := runtime.stepAreaMembership(cast, spawn)
		cast.switchEvalContext(previous)
		if areaErr != nil {
			stopErr := runtime.requestSpawnStop(cast, spawn, StopCauseFailure, "")
			if stopErr == nil {
				delete(runtime.spawns, spawn.ID)
			}
			return errors.Join(areaErr, stopErr)
		}
		signals = append(signals, areaSignals...)
		spawn.NextTick = saturatingTickAdd(runtime.currentTick, spawnTemplate.intervalTicks)
	}
	startSignals := signals
	if spawnTemplate.area == nil {
		startSignals = append(startSignals, SpawnSignal{Kind: SpawnSignalEnter, Target: lifecycle})
	}
	runtime.emitSpawnSignals(cast, spawn, startSignals, cast.visibleRevision)
	if err := runtime.dispatchOwnedSpawnSignals(spawn, startSignals); err != nil {
		stopErr := runtime.requestSpawnStop(cast, spawn, StopCauseFailure, "")
		if stopErr == nil {
			delete(runtime.spawns, spawn.ID)
		}
		return errors.Join(err, stopErr)
	}
	if spawn.areaCallbackFinishedCast {
		return runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "")
	}
	return nil
}

func detachedSpawnLocals(program *Program) []RuntimeValue {
	locals := make([]RuntimeValue, len(program.locals))
	for index, slot := range program.locals {
		locals[index] = MissingRuntimeValue(slot.typ)
	}
	return locals
}

// detachedSpawnCast 构造移交后（或脱离施法）的衍生物求值用的 cast：没有施法的输入、memory、
// 施法状态，caster 是衍生物的 owner、primaryTarget 是 lifecycle 实体。evalContext 说明它用于
// 衍生物每一步（evalSpawnStep）还是回调（evalSpawnCallback），Runtime 按求值上下文表查引用。
func (runtime *Runtime) detachedSpawnCast(spawn *SpawnInstance, evalContext evalContext) *castInstance {
	context := spawn.eventContext
	context.Tick, context.WorldRevision, context.SpawnID = runtime.currentTick, runtime.host.CurrentRevision(), spawn.ID
	return &castInstance{
		id: spawn.CastID, program: spawn.Program, caster: spawn.Owner, primaryTarget: spawn.LifecycleEntity,
		locals: cloneLocalFrame(spawn.locals), snapshots: cloneSpawnSnapshots(spawn.snapshots), status: CastRunning,
		visibleRevision: runtime.host.CurrentRevision(), randomKey: spawn.randomKey, randomInvocations: cloneRandomInvocations(spawn.randomInvocations),
		eventContext: context, detachedSpawn: spawn, detachedEvent: context, evalContext: evalContext,
	}
}

func activeCallbackSpawn(cast *castInstance, spawnID SpawnID) (*SpawnInstance, error) {
	if cast == nil || cast.detachedSpawn == nil {
		return nil, ErrCastInputRejected
	}
	spawn := cast.detachedSpawn
	if spawn.ID != spawnID || spawn.CastID != cast.id || spawn.Status != SpawnRunning || spawn.Program != cast.program {
		return nil, ErrCastInputRejected
	}
	return spawn, nil
}

func cloneSpawnSnapshots(values map[int]RuntimeValue) map[int]RuntimeValue {
	result := make(map[int]RuntimeValue, len(values))
	for key, value := range values {
		result[key] = cloneRuntimeValue(value)
	}
	return result
}

func cloneRandomInvocations(values map[RandomSiteIndex]uint64) map[RandomSiteIndex]uint64 {
	result := make(map[RandomSiteIndex]uint64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func (runtime *Runtime) captureOwnedSpawnSnapshots(spawn *SpawnInstance) error {
	callbackCast := runtime.detachedSpawnCast(spawn, evalSpawnCallback)
	if err := runtime.captureSnapshots(callbackCast, snapshotSpawnStart); err != nil {
		return err
	}
	spawn.snapshots = cloneSpawnSnapshots(callbackCast.snapshots)
	spawn.visibleRevision = callbackCast.visibleRevision
	return nil
}

func (runtime *Runtime) hasOwnedSpawnCapacityExcluding(owner EntityID, programID string, template UnitTemplateHandle, additional int, excluded map[EntityID]bool) bool {
	total, ownerCount, programCount, templateCount := 0, 0, 0, 0
	for _, spawn := range runtime.spawns {
		// 待停止的衍生物仍在宿主侧运行，照样占容量。
		if spawn.Scope != SpawnScopeEntity || !spawn.liveOnHost() {
			continue
		}
		if excluded[spawn.LifecycleEntity] {
			continue
		}
		total++
		if spawn.Owner == owner {
			ownerCount++
		}
		if spawn.Program != nil && spawn.Program.id == programID {
			programCount++
		}
		if spawn.UnitTemplate == template {
			templateCount++
		}
	}
	return total+additional <= runtime.options.MaxOwnedSpawns && ownerCount+additional <= runtime.options.MaxOwnedSpawnsPerOwner && programCount+additional <= runtime.options.MaxOwnedSpawnsPerProgram && templateCount+additional <= runtime.options.MaxOwnedSpawnsPerTemplate
}

func (runtime *Runtime) previewOwnedSpawnCapacity(host OwnedEntityRuntimeHost, command SummonCommand) (ExpectedFailureReason, error) {
	excluded := make(map[EntityID]bool)
	preview, err := host.PreviewOwnedSummon(command)
	if err != nil {
		return ExpectedFailureNone, err
	}
	if preview.FailureReason != ExpectedFailureNone {
		return preview.FailureReason, nil
	}
	for _, entity := range preview.ReplacedEntities {
		excluded[entity] = true
	}
	if !runtime.hasOwnedSpawnCapacityExcluding(command.Owner, command.SourceSkillID, command.Template, command.Count, excluded) {
		return ExpectedFailureCapacityReached, nil
	}
	return ExpectedFailureNone, nil
}

func (runtime *Runtime) failOwnedSpawn(spawn *SpawnInstance, cause error) error {
	if spawn != nil {
		return errors.Join(cause, runtime.terminateOwnedSpawn(spawn.ID, StopCauseFailure, ""))
	}
	return cause
}

func (runtime *Runtime) handoffEntitySpawns(cast *castInstance) error {
	ids := make([]SpawnID, 0)
	for id, spawn := range runtime.spawns {
		if spawn.CastID != cast.id || spawn.Scope != SpawnScopeEntity || spawn.Status != SpawnRunning || spawn.handedOff {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return nil
	}
	host, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok {
		return ErrHostContractViolation
	}
	valid, invalid := make([]SpawnID, 0, len(ids)), make([]SpawnID, 0)
	for _, id := range ids {
		spawn := runtime.spawns[id]
		if _, alive := host.OwnedEntity(spawn.LifecycleEntity); !alive {
			invalid = append(invalid, id)
		} else {
			valid = append(valid, id)
		}
	}
	var cleanupErr error
	for _, id := range invalid {
		spawn := runtime.spawns[id]
		cleanupErr = errors.Join(cleanupErr, runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"))
	}
	if cleanupErr != nil {
		for _, id := range valid {
			spawn := runtime.spawns[id]
			cleanupErr = errors.Join(cleanupErr, runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"))
		}
		return cleanupErr
	}
	for _, id := range valid {
		spawn := runtime.spawns[id]
		spawn.handedOff = true
		runtime.ownedSpawns[spawn.ID] = spawn
	}
	return nil
}

func (runtime *Runtime) OwnedSpawns(owner EntityID) []OwnedSpawnSnapshot {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	result := make([]OwnedSpawnSnapshot, 0, len(runtime.ownedSpawns))
	for _, spawn := range runtime.ownedSpawns {
		if owner != 0 && spawn.Owner != owner {
			continue
		}
		programID := ""
		if spawn.Program != nil {
			programID = spawn.Program.id
		}
		result = append(result, OwnedSpawnSnapshot{ID: spawn.ID, Owner: spawn.Owner, LifecycleEntity: spawn.LifecycleEntity, SourceCastID: spawn.CastID, ProgramID: programID, StartTick: spawn.StartTick, EndTick: spawn.EndTick, Status: spawn.Status, HandedOff: spawn.handedOff})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (runtime *Runtime) reapUnhandedEntitySpawns() error {
	ids := make([]SpawnID, 0)
	host, hostOK := runtime.host.(OwnedEntityRuntimeHost)
	for id, spawn := range runtime.spawns {
		if spawn.Scope != SpawnScopeEntity || spawn.Status != SpawnRunning || spawn.handedOff {
			continue
		}
		if !hostOK {
			return ErrHostContractViolation
		}
		if _, alive := host.OwnedEntity(spawn.LifecycleEntity); !alive {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns[id]
		cast := runtime.casts[spawn.CastID]
		if err := runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) advanceOwnedSpawns() error {
	if err := runtime.reapUnhandedEntitySpawns(); err != nil {
		return err
	}
	if err := runtime.reapInvalidOwnedSpawns(); err != nil {
		return err
	}
	ids := make([]SpawnID, 0, len(runtime.ownedSpawns))
	for id := range runtime.ownedSpawns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		spawn := runtime.ownedSpawns[id]
		if spawn == nil || spawn.Status != SpawnRunning || spawn.NextTick > runtime.currentTick {
			continue
		}
		stepCast := runtime.detachedSpawnCast(spawn, evalSpawnStep)
		signals, err := runtime.stepSpawnMotion(stepCast, spawn)
		if err != nil {
			return runtime.failOwnedSpawn(spawn, err)
		}
		template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
		if template.area != nil {
			signals = areaSpawnSignals(signals)
			areaSignals, areaErr := runtime.stepAreaMembership(stepCast, spawn)
			if areaErr != nil {
				return runtime.failOwnedSpawn(spawn, areaErr)
			}
			signals = append(signals, areaSignals...)
		}
		runtime.emitSpawnPresentation(stepCast, spawn, PresentationSpawnUpdate, "", "", stepCast.visibleRevision)
		runtime.emitSpawnSignals(stepCast, spawn, signals, stepCast.visibleRevision)
		if cast := runtime.casts[spawn.CastID]; cast != nil {
			cast.visibleRevision = maxRevision(cast.visibleRevision, stepCast.visibleRevision)
		}
		if err := runtime.dispatchOwnedSpawnSignals(spawn, signals); err != nil {
			return runtime.failOwnedSpawn(spawn, err)
		}
		if spawn.areaCallbackFinishedCast {
			// 移交后的 area 回调 finish：与施法存活时 startEntitySpawn 的处理一致，
			// 停止本 area 衍生物，不再跑 end / cancel 回调（RR-20261005-NC-211）。
			if err := runtime.terminateOwnedSpawn(id, StopCauseCancel, ""); err != nil {
				return err
			}
			continue
		}
		interval := Tick(1)
		if template.area != nil {
			interval = template.intervalTicks
		}
		spawn.NextTick = saturatingTickAdd(runtime.currentTick, interval)
	}
	if err := runtime.collectHostEvents(); err != nil {
		return err
	}
	return runtime.reapOwnedSpawns()
}

func (runtime *Runtime) reapInvalidOwnedSpawns() error {
	host, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok && len(runtime.ownedSpawns) != 0 {
		return ErrHostContractViolation
	}
	type reapedSpawn struct {
		id    SpawnID
		cause StopCause
		event string
	}
	items := make([]reapedSpawn, 0)
	for id, spawn := range runtime.ownedSpawns {
		moving := spawn.Program != nil && int(spawn.TemplateIndex) < len(spawn.Program.spawnTemplates) && spawn.Program.spawnTemplates[spawn.TemplateIndex].motion != nil
		if spawn.Motion.Stage == MotionStageCompleted || spawn.EndTick <= runtime.currentTick {
			if moving {
				if spawn.Motion.Stage == MotionStageCompleted {
					items = append(items, reapedSpawn{id: id, cause: StopCauseEnd, event: "end"})
				}
				continue
			}
			items = append(items, reapedSpawn{id: id, cause: StopCauseEnd, event: "end"})
			continue
		}
		_, alive := host.OwnedEntity(spawn.LifecycleEntity)
		if !alive || spawn.Program == nil {
			items = append(items, reapedSpawn{id: id, cause: StopCauseCancel, event: "cancel"})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	var firstErr error
	for _, item := range items {
		if err := runtime.terminateOwnedSpawn(item.id, item.cause, item.event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) dispatchOwnedSpawnSignals(spawn *SpawnInstance, signals []SpawnSignal) error {
	baseContext := spawn.eventContext
	baseContext.Target = spawn.LifecycleEntity
	for _, signal := range normalizeSpawnSignals(signals) {
		if spawn.areaCallbackFinishedCast {
			break
		}
		context := baseContext
		if signal.Target != 0 {
			context.Target = signal.Target
		}
		context.MembershipTicks = signal.MembershipTicks
		context.EnterCount = signal.EnterCount
		spawn.eventContext = context
		if err := runtime.runOwnedSpawnCallback(spawn, string(signal.Kind)); err != nil {
			return err
		}
	}
	spawn.eventContext = baseContext
	return nil
}

func (runtime *Runtime) reapOwnedSpawns() error {
	host, hostOK := runtime.host.(OwnedEntityRuntimeHost)
	type expiredSpawn struct {
		id    SpawnID
		cause StopCause
		event string
	}
	expired := make([]expiredSpawn, 0)
	for id, spawn := range runtime.ownedSpawns {
		if !hostOK {
			return ErrHostContractViolation
		}
		_, alive := host.OwnedEntity(spawn.LifecycleEntity)
		if spawn.Motion.Stage == MotionStageCompleted || spawn.EndTick <= runtime.currentTick {
			expired = append(expired, expiredSpawn{id: id, cause: StopCauseEnd, event: "end"})
		} else if !alive || spawn.Program == nil {
			expired = append(expired, expiredSpawn{id: id, cause: StopCauseCancel, event: "cancel"})
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].id < expired[j].id })
	var firstErr error
	for _, item := range expired {
		if err := runtime.terminateOwnedSpawn(item.id, item.cause, item.event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) runOwnedSpawnCallback(spawn *SpawnInstance, event string) error {
	if spawn == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return nil
	}
	if event == "end" {
		if spawn.Motion.EndCallbackEmitted {
			return nil
		}
		spawn.Motion.EndCallbackEmitted = true
	}
	template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
	for _, callback := range template.callbacks {
		if callback.event != event {
			continue
		}
		callbackCast := runtime.detachedSpawnCast(spawn, evalSpawnCallback)
		control, err := runtime.executeOperation(callbackCast, callback.operation)
		spawn.locals = cloneLocalFrame(callbackCast.locals)
		spawn.snapshots = cloneSpawnSnapshots(callbackCast.snapshots)
		spawn.randomInvocations = cloneRandomInvocations(callbackCast.randomInvocations)
		spawn.visibleRevision = callbackCast.visibleRevision
		if cast := runtime.casts[spawn.CastID]; cast != nil && !spawn.handedOff {
			cast.visibleRevision = maxRevision(cast.visibleRevision, callbackCast.visibleRevision)
		}
		if err != nil {
			return err
		}
		if control.kind != flowContinue {
			if control.kind != flowFinish || template.area == nil {
				return ErrProgramInvariant
			}
			// area 回调的 finish = 结束拥有它的施法 + 停止本 area 余下的信号。编译器
			// （compile_owned_entity.go 的 allowAreaFinish）不知道施法会不会先结束：
			// 施法先结束时 entity 衍生物已经移交（handedOff），拥有者施法是终态或已回收，
			// 这时 finish 没有施法可结束，只停止本 area。此前这里返回
			// ErrProgramInvariant，Advance 在 tick 中途失败（RR-20261005-NC-211）。
			ownerCast := runtime.casts[spawn.CastID]
			ownerLive := !spawn.handedOff && ownerCast != nil && ownerCast.status != CastFinished && ownerCast.status != CastFailed && !ownerCast.logicalFinished
			if ownerLive {
				ownerCast.areaCallbackFinish = true
			}
			spawn.areaCallbackFinishedCast = true
		}
		runtime.appendRuntimeEvent(RuntimeEvent{Tick: runtime.currentTick, Kind: "owned_spawn_callback_" + event, Entity: spawn.LifecycleEntity, Context: callbackCast.detachedEvent})
	}
	return nil
}

// terminateOwnedSpawn 请求停止一个移交后的衍生物（tick 驱动：到期、失效、步进失败、area 回调 finish）。
// 宿主拒绝时 requestSpawnStop 把它转入待停止、摘出 owned 表，下一次 Advance 不会再卡在它上面（RR-20261006-31）。
func (runtime *Runtime) terminateOwnedSpawn(id SpawnID, cause StopCause, callbackEvent string) error {
	spawn := runtime.ownedSpawns[id]
	if spawn == nil {
		return nil
	}
	return runtime.requestSpawnStop(nil, spawn, cause, callbackEvent)
}

// RemoveProgram 请求停止该程序的全部衍生物（已移交的跑 cancel 回调），再让宿主删除它的 owned 实体。
// 宿主拒绝停止时返回第一个错误（errors.Is 宿主的错误），停不下的衍生物留成 stop_pending：程序移除之后仍由 Runtime
// 在之后的 tick 按退避重试（重试只重发宿主 StopSpawn，不执行程序代码），再调用一次 RemoveProgram 会立即再请求一次。
// 之前它们留成 running、列在 OwnedSpawns 里等调用方重试（停止入口统一，维护者 2026-10-07）。
func (runtime *Runtime) RemoveProgram(programID string) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	ids := make([]SpawnID, 0)
	for id, spawn := range runtime.spawns {
		if spawn.Program != nil && spawn.Program.id == programID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns[id]
		cast := runtime.casts[spawn.CastID]
		callbackEvent := ""
		if spawn.handedOff {
			cast = nil
			callbackEvent = "cancel"
		}
		if err := runtime.requestSpawnStop(cast, spawn, StopCauseCancel, callbackEvent); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if lifecycleHost, ok := runtime.host.(OwnedEntityRuntimeHost); ok {
		if err := lifecycleHost.RemoveOwnedEntitiesByProgram(programID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Shutdown 请求停止全部仍在宿主侧的衍生物（含 stop_pending，已移交的跑 cancel 回调），再让宿主清理比赛的 owned 实体。
// 宿主拒绝停止时返回第一个错误（errors.Is 宿主的错误），停不下的衍生物留成 stop_pending 并写进 checkpoint：
// 继续 Advance、或 Checkpoint 后在新进程 RestoreRuntime 再 Advance，Runtime 都会按原来的重试时刻接着停；再调用一次
// Shutdown 会立即再请求一次。Shutdown 不在原地同步重试，理由见 runtime_spawn_stop.go。
func (runtime *Runtime) Shutdown() error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	ids := make([]SpawnID, 0, len(runtime.spawns))
	for id, spawn := range runtime.spawns {
		// 待停止的衍生物（含重试已到上限的）在这里再请求一次；宿主 StopSpawn 幂等。
		if spawn.liveOnHost() {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns[id]
		callbackEvent := ""
		if spawn.handedOff {
			callbackEvent = "cancel"
		}
		if err := runtime.requestSpawnStop(nil, spawn, StopCauseCancel, callbackEvent); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if lifecycleHost, ok := runtime.host.(OwnedEntityRuntimeHost); ok {
		if err := lifecycleHost.RemoveOwnedEntitiesForMatchEnd(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
