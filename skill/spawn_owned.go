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
	}
	// 衍生物属于施法的因果链：继承施法事件的 EventID（作为回调事件的父事件）、RootEventID、ProcDepth 与 proc 系数，
	// 每次回调再派生一个新事件（runOwnedSpawnCallback）。之前这里只填 tick / source / owner / target / skill / cast / spawn，
	// 根事件与深度在衍生物处清零：max_depth 管不住“被动 → 召唤衍生物 → 回调伤害 → 被动”，回调效果每个 tick 同一个
	// EventID、自成一个根（RR-20261006-53）。
	spawn.eventContext = cast.eventContext
	spawn.eventContext.Tick, spawn.eventContext.Target, spawn.eventContext.SpawnID = runtime.currentTick, lifecycle, runtime.nextSpawnID
	spawn.eventContext.Source, spawn.eventContext.Owner, spawn.eventContext.SkillID, spawn.eventContext.CastID = cast.caster, cast.caster, cast.program.id, cast.id
	spawn.eventContext.EffectIndex = 0
	// 启动那一步用施法本身求衍生物字段，但按 spawn_step 列查表：之后每一步在移交后的衍生物里
	// 求同样的字段，两边都要求得出（RR-20261005-NC-224）。numeric track 初值在
	// initializeSpawnNumeric 里切回施法流程。
	if !runtime.spawns.add(spawn) {
		// 不可达：ID 取自只增不减的 nextSpawnID，checkpoint 恢复核对过记录 ID 不超过它。
		return ErrProgramInvariant
	}
	previous := cast.switchEvalContext(evalSpawnStep)
	signals, err := runtime.stepSpawnMotion(cast, spawn)
	cast.switchEvalContext(previous)
	if err != nil {
		// 启动步的任何一步被宿主拒绝时，宿主可能已经登记了这个衍生物（Frame 等前几步已提交），所以记录先入表、
		// 失败时经 requestSpawnStop 停掉（含解除 carry）；宿主拒绝停止时转入待停止、由 Runtime 重试，Shutdown 也停得到。
		// 之前记录在步进成功之后才入表，失败只解除 carry 就返回：召唤事务回滚了，宿主侧的衍生物没人停（RR-20261006-52）。
		stopErr := runtime.requestSpawnStop(cast, spawn, StopCauseFailure, "")
		if stopErr == nil {
			runtime.spawns.drop(spawn.ID)
		}
		return errors.Join(err, stopErr)
	}
	spawn.visibleRevision = cast.visibleRevision
	runtime.drainHostEvents(cast)
	runtime.emitSpawnPresentation(cast, spawn, PresentationSpawnStart, "", "", cast.visibleRevision)
	if cast.areaCallbackFinish {
		return runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "")
	}
	if err := runtime.captureOwnedSpawnSnapshots(spawn); err != nil {
		stopErr := runtime.requestSpawnStop(cast, spawn, StopCauseFailure, "")
		if stopErr == nil {
			runtime.spawns.drop(spawn.ID)
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
				runtime.spawns.drop(spawn.ID)
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
			runtime.spawns.drop(spawn.ID)
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
	// 待停止的衍生物仍在宿主侧运行，照样占容量。
	runtime.spawns.each(func(spawn *SpawnInstance) {
		if spawn.Scope != SpawnScopeEntity || excluded[spawn.LifecycleEntity] {
			return
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
	}, spawnLivePartitions...)
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
	ids := runtime.spawns.sortedIDs(func(spawn *SpawnInstance) bool {
		return spawn.CastID == cast.id && spawn.Scope == SpawnScopeEntity
	}, spawnCasting)
	if len(ids) == 0 {
		return nil
	}
	host, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok {
		return ErrHostContractViolation
	}
	valid, invalid := make([]SpawnID, 0, len(ids)), make([]SpawnID, 0)
	for _, id := range ids {
		spawn := runtime.spawns.get(id)
		if _, alive := host.OwnedEntity(spawn.LifecycleEntity); !alive {
			invalid = append(invalid, id)
		} else {
			valid = append(valid, id)
		}
	}
	var cleanupErr error
	for _, id := range invalid {
		spawn := runtime.spawns.get(id)
		cleanupErr = errors.Join(cleanupErr, runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"))
	}
	if cleanupErr != nil {
		for _, id := range valid {
			spawn := runtime.spawns.get(id)
			cleanupErr = errors.Join(cleanupErr, runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"))
		}
		return cleanupErr
	}
	for _, id := range valid {
		spawn := runtime.spawns.get(id)
		runtime.spawns.setState(spawn, spawn.Status, true)
	}
	return nil
}

func (runtime *Runtime) OwnedSpawns(owner EntityID) []OwnedSpawnSnapshot {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	// 只扫已移交分区：移交给谁由记录的 Owner 字段表达。
	result := make([]OwnedSpawnSnapshot, 0, runtime.spawns.count(spawnHandedOff))
	runtime.spawns.each(func(spawn *SpawnInstance) {
		if owner != 0 && spawn.Owner != owner {
			return
		}
		programID := ""
		if spawn.Program != nil {
			programID = spawn.Program.id
		}
		result = append(result, OwnedSpawnSnapshot{ID: spawn.ID, Owner: spawn.Owner, LifecycleEntity: spawn.LifecycleEntity, SourceCastID: spawn.CastID, ProgramID: programID, StartTick: spawn.StartTick, EndTick: spawn.EndTick, Status: spawn.Status, HandedOff: spawn.handedOff})
	}, spawnHandedOff)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (runtime *Runtime) reapUnhandedEntitySpawns() error {
	host, hostOK := runtime.host.(OwnedEntityRuntimeHost)
	entitySpawns := runtime.spawns.sortedIDs(func(spawn *SpawnInstance) bool { return spawn.Scope == SpawnScopeEntity }, spawnCasting)
	if len(entitySpawns) != 0 && !hostOK {
		return ErrHostContractViolation
	}
	ids := make([]SpawnID, 0)
	for _, id := range entitySpawns {
		if _, alive := host.OwnedEntity(runtime.spawns.get(id).LifecycleEntity); !alive {
			ids = append(ids, id)
		}
	}
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns.get(id)
		cast := runtime.casts[spawn.CastID]
		if err := runtime.requestSpawnStop(cast, spawn, StopCauseCancel, "cancel"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// advanceOwnedSpawns 是衍生物逐 tick 推进的唯一入口（RR-20261006-51；spawn_lifecycle_e2e_promises_test.go 的源码守卫
// 核对只有它与 startEntitySpawn 的启动步调用 stepSpawnMotion / stepAreaMembership）。施放中与已移交的衍生物走同一条路：
// 回收已失效 / 已到期的、按 NextTick 推进到期的、派发信号与回调、再回收这一步之后到期的。两者只差在停止、表现与 revision
// 记在谁名下（spawnOwnerCast）。之前这里只推进已移交分区；施放中的衍生物唯一的推进路径 spawnStepTask 在生产路径上没有
// 创建点，召唤后 wait 的施法里衍生物在移交之前一步都不走，EndTick 照样从启动算起，移交后只补走一步，中间的 tick 永久丢失。
func (runtime *Runtime) advanceOwnedSpawns() error {
	if err := runtime.reapUnhandedEntitySpawns(); err != nil {
		return err
	}
	if err := runtime.reapInvalidOwnedSpawns(); err != nil {
		return err
	}
	for _, id := range runtime.spawns.sortedIDs(nil, spawnSteppedPartitions...) {
		spawn := runtime.spawns.get(id, spawnSteppedPartitions...)
		if spawn == nil || spawn.NextTick > runtime.currentTick {
			// 前面的回调可能已停掉它（挪进已停止 / 待停止分区），或它这一 tick 已经走过。
			continue
		}
		owner := runtime.spawnOwnerCast(spawn)
		// 每一步都在脱离施法的 cast 上求值（spawn_step 列），与移交与否无关：同一个衍生物移交前后求得同样的字段
		// （RR-20261005-NC-224）。表现记在所属施法名下（施放中），已移交的记在 detachedSpawnCast 名下（RR-20261006-22）。
		stepCast := runtime.detachedSpawnCast(spawn, evalSpawnStep)
		presentationCast := stepCast
		if owner != nil {
			presentationCast = owner
		}
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
		if cast := runtime.casts[spawn.CastID]; cast != nil {
			cast.visibleRevision = maxRevision(cast.visibleRevision, stepCast.visibleRevision)
		}
		runtime.emitSpawnPresentation(presentationCast, spawn, PresentationSpawnUpdate, "", "", stepCast.visibleRevision)
		runtime.emitSpawnSignals(presentationCast, spawn, signals, stepCast.visibleRevision)
		if err := runtime.dispatchOwnedSpawnSignals(spawn, signals); err != nil {
			return runtime.failOwnedSpawn(spawn, err)
		}
		if spawn.areaCallbackFinishedCast {
			// area 回调 finish：停止本 area 衍生物，不再跑 end / cancel 回调（RR-20261005-NC-211）；施法仍在时同时结束施法
			// （与启动那一步 startEntitySpawn 的处理一致）。
			if err := runtime.terminateOwnedSpawn(id, StopCauseCancel, ""); err != nil {
				return err
			}
			if err := runtime.finishCastFromAreaCallback(owner); err != nil {
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
	runtime.collectHostEvents()
	return runtime.reapOwnedSpawns()
}

// spawnOwnerCast 返回施放中衍生物的所属施法：停止经它请求、表现与 revision 记在它名下；已移交（或施法已回收）时为 nil，
// 停止与表现经 detachedSpawnCast、用宿主当前 revision。
func (runtime *Runtime) spawnOwnerCast(spawn *SpawnInstance) *castInstance {
	if spawn == nil || spawn.handedOff {
		return nil
	}
	return runtime.casts[spawn.CastID]
}

// finishCastFromAreaCallback 处理施放中 area 衍生物在 tick 推进里执行的 finish：area 回调的 finish = 结束拥有它的施法
// （compile_owned_entity.go 的 allowAreaFinish）。施法此时停在排程任务上（wait / repeat 等），撤掉当前 phase 的任务
// 再走正常收尾，余下的流程不再执行。启动那一步的 finish 由 executor 的 summon 分支按 flowFinish 处理，不经过这里。
func (runtime *Runtime) finishCastFromAreaCallback(cast *castInstance) error {
	if cast == nil || !cast.areaCallbackFinish || castEnded(cast) {
		return nil
	}
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	if err := runtime.finishCast(cast); err != nil {
		return runtime.failCastLocked(cast, err)
	}
	return nil
}

// reapInvalidOwnedSpawns 在推进之前回收逐 tick 推进的衍生物里已经结束的：非运动的到了 EndTick、运动的已完成（运动衍生物
// 在 EndTick 那一步完成，所以到期但未完成的先走完这一步），以及 lifecycle 实体已失效的。
func (runtime *Runtime) reapInvalidOwnedSpawns() error {
	host, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok && runtime.spawns.count(spawnSteppedPartitions...) != 0 {
		return ErrHostContractViolation
	}
	type reapedSpawn struct {
		id    SpawnID
		cause StopCause
		event string
	}
	items := make([]reapedSpawn, 0)
	for _, id := range runtime.spawns.sortedIDs(nil, spawnSteppedPartitions...) {
		spawn := runtime.spawns.get(id)
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
	stepped := runtime.spawns.sortedIDs(nil, spawnSteppedPartitions...)
	if len(stepped) != 0 && !hostOK {
		return ErrHostContractViolation
	}
	for _, id := range stepped {
		spawn := runtime.spawns.get(id)
		_, alive := host.OwnedEntity(spawn.LifecycleEntity)
		if spawn.Motion.Stage == MotionStageCompleted || spawn.EndTick <= runtime.currentTick {
			expired = append(expired, expiredSpawn{id: id, cause: StopCauseEnd, event: "end"})
		} else if !alive || spawn.Program == nil {
			expired = append(expired, expiredSpawn{id: id, cause: StopCauseCancel, event: "cancel"})
		}
	}
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
		// 每次回调是衍生物事件的一个新子事件（新 EventID，父事件是施法事件，根事件与深度不变），回调里的效果再从它派生
		// （RR-20261006-53）。
		callbackEvent := deriveEvent(callbackCast.eventContext, runtime.nextSpawnEventID())
		callbackCast.eventContext, callbackCast.detachedEvent = callbackEvent, callbackEvent
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

// terminateOwnedSpawn 请求停止一个逐 tick 推进的衍生物（tick 驱动：到期、失效、步进失败、area 回调 finish），施放中的
// 经所属施法请求（spawnOwnerCast）。宿主拒绝时 requestSpawnStop 把它转入待停止，下一次 Advance 不会再卡在它上面
// （RR-20261006-31）。
func (runtime *Runtime) terminateOwnedSpawn(id SpawnID, cause StopCause, callbackEvent string) error {
	spawn := runtime.spawns.get(id, spawnSteppedPartitions...)
	if spawn == nil {
		return nil
	}
	return runtime.requestSpawnStop(runtime.spawnOwnerCast(spawn), spawn, cause, callbackEvent)
}

// RemoveProgram 请求停止该程序的全部衍生物（entity 衍生物不论施放中还是已移交都跑一次 cancel 回调，
// spawnCancelCallbackEvent），再让宿主删除它的 owned 实体。之前只给已移交的跑，施放中的被 RemoveProgram 停下时一次
// cancel 也没有，与 Shutdown / Cancel / Interrupt 不一致（RR-20261006-55 后续二，维护者 2026-10-07 F08-H+2 ②）。
// 宿主拒绝停止时返回第一个错误（errors.Is 宿主的错误），停不下的衍生物留成 stop_pending：程序移除之后仍由 Runtime
// 在之后的 tick 按退避重试（重试只重发宿主 StopSpawn，不执行程序代码），再调用一次 RemoveProgram 会立即再请求一次。
// 之前它们留成 running、列在 OwnedSpawns 里等调用方重试（停止入口统一，维护者 2026-10-07）。该程序已放弃的衍生物
// 不再停；它们的记录仍引用程序（checkpoint 恢复时 resolver 仍要能解析它），直到 Advance 末尾按 MaxAbandonedSpawns 清理。
func (runtime *Runtime) RemoveProgram(programID string) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	for program := range runtime.hostAdmitted {
		if program.id == programID {
			delete(runtime.hostAdmitted, program)
		}
	}
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	// 已停止分区的记录，停止请求本来就是空操作，只看仍在宿主侧运行的分区。
	ids := runtime.spawns.sortedIDs(func(spawn *SpawnInstance) bool {
		return spawn.Program != nil && spawn.Program.id == programID
	}, spawnLivePartitions...)
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns.get(id)
		if spawn == nil {
			// 双重保险（RR-20261006-34）：循环中途不会删记录——到待停止上限的记录挪进已放弃分区而不删，取回的是
			// 那条已放弃的记录，停止请求对它是空操作；删记录只在 spawnDropSites 登记的安全点。
			continue
		}
		cast := runtime.casts[spawn.CastID]
		if spawn.handedOff {
			cast = nil
		}
		// 待停止的不再跑回调（requestSpawnStop 只在第一次请求时跑）。
		if err := runtime.requestSpawnStop(cast, spawn, StopCauseCancel, spawnCancelCallbackEvent(spawn)); err != nil && firstErr == nil {
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

// Shutdown 请求停止全部仍在宿主侧的衍生物（含 stop_pending；entity 衍生物不论施放中还是已移交都跑一次 cancel 回调），再让宿主清理比赛的 owned 实体。
// 宿主拒绝停止时返回第一个错误（errors.Is 宿主的错误），停不下的衍生物留成 stop_pending 并写进 checkpoint：
// 继续 Advance、或 Checkpoint 后在新进程 RestoreRuntime 再 Advance，Runtime 都会按原来的重试时刻接着停；再调用一次
// Shutdown 会立即再请求一次。Shutdown 不在原地同步重试，理由见 runtime_spawn_stop.go。已放弃的衍生物
// （待停止超过 MaxStopPendingSpawns 时放弃的）不再停：Runtime 已告警、不再负责它们，记录留到 Advance 末尾按上限清理。
func (runtime *Runtime) Shutdown() error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	// 待停止的衍生物（含重试已到上限的）在这里再请求一次；宿主 StopSpawn 幂等。
	ids := runtime.spawns.sortedIDs(nil, spawnLivePartitions...)
	var firstErr error
	for _, id := range ids {
		spawn := runtime.spawns.get(id)
		if spawn == nil {
			// 双重保险（RR-20261006-34）：循环中途不会删记录——到待停止上限的记录挪进已放弃分区而不删，取回的是
			// 那条已放弃的记录，停止请求对它是空操作；删记录只在 spawnDropSites 登记的安全点。
			continue
		}
		// 施放中与已移交的 entity 衍生物一样跑一次 cancel 回调（spawnCancelCallbackEvent）；待停止的不再跑回调（requestSpawnStop）。
		if err := runtime.requestSpawnStop(nil, spawn, StopCauseCancel, spawnCancelCallbackEvent(spawn)); err != nil && firstErr == nil {
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
