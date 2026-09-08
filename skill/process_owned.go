package skill

import (
	"errors"
	"sort"
)

type OwnedProcessSnapshot struct {
	ID              ProcessID
	Owner           EntityID
	LifecycleEntity EntityID
	SourceCastID    CastID
	ProgramID       string
	StartTick       Tick
	EndTick         Tick
	Status          ProcessStatus
	HandedOff       bool
}

func (runtime *Runtime) startEntityProcess(cast *castInstance, template ProcessTemplateIndex, unitTemplate UnitTemplateHandle, lifecycle EntityID, duration Tick, position Position) error {
	if cast == nil || lifecycle == 0 || int(template) >= len(cast.program.processTemplates) {
		return ErrProgramInvariant
	}
	processTemplate := cast.program.processTemplates[template]
	if processTemplate.motion != nil || processTemplate.area != nil {
		duration = processTemplate.durationTicks
	}
	if duration <= 0 {
		return ErrProgramInvariant
	}
	runtime.nextProcessID++
	process := &ProcessInstance{
		ID: runtime.nextProcessID, CastID: cast.id, TemplateIndex: template, UnitTemplate: unitTemplate,
		Status: ProcessRunning, StartTick: runtime.currentTick, NextTick: saturatingTickAdd(runtime.currentTick, 1), EndTick: saturatingTickAdd(runtime.currentTick, duration),
		Scope: ProcessScopeEntity, Owner: cast.caster, LifecycleEntity: lifecycle, Program: cast.program,
		HostState: ProcessHostState{ProcessID: runtime.nextProcessID, Active: true, Position: position}, Motion: MotionState{Position: position},
		phaseToken: cast.phaseToken, locals: detachedProcessLocals(cast.program), snapshots: make(map[int]RuntimeValue), randomKey: cast.randomKey, randomInvocations: make(map[RandomSiteIndex]uint64), visibleRevision: cast.visibleRevision,
		eventContext: EventContext{Tick: runtime.currentTick, Source: cast.caster, Owner: cast.caster, Target: lifecycle, SkillID: cast.program.id, CastID: cast.id, ProcessID: runtime.nextProcessID},
	}
	signals, err := runtime.stepProcessMotion(cast, process)
	if err != nil {
		return errors.Join(err, runtime.detachMotionCarry(cast, process))
	}
	process.visibleRevision = cast.visibleRevision
	runtime.processes[process.ID] = process
	if err := runtime.drainHostEvents(cast); err != nil {
		return err
	}
	runtime.emitProcessPresentation(cast, process, PresentationProcessStart, "", "", cast.visibleRevision)
	if cast.areaCallbackFinish {
		return runtime.terminateProcess(cast, process, StopCauseCancel, "")
	}
	if err := runtime.captureOwnedProcessSnapshots(process); err != nil {
		stopErr := runtime.terminateProcess(cast, process, StopCauseFailure, "")
		if stopErr == nil {
			delete(runtime.processes, process.ID)
		}
		return errors.Join(err, stopErr)
	}
	if processTemplate.area != nil {
		signals = areaProcessSignals(signals)
		areaSignals, areaErr := runtime.stepAreaMembership(cast, process)
		if areaErr != nil {
			stopErr := runtime.terminateProcess(cast, process, StopCauseFailure, "")
			if stopErr == nil {
				delete(runtime.processes, process.ID)
			}
			return errors.Join(areaErr, stopErr)
		}
		signals = append(signals, areaSignals...)
		process.NextTick = saturatingTickAdd(runtime.currentTick, processTemplate.intervalTicks)
	}
	startSignals := signals
	if processTemplate.area == nil {
		startSignals = append(startSignals, ProcessSignal{Kind: ProcessSignalEnter, Target: lifecycle})
	}
	runtime.emitProcessSignals(cast, process, startSignals, cast.visibleRevision)
	if err := runtime.dispatchOwnedProcessSignals(process, startSignals); err != nil {
		stopErr := runtime.terminateProcess(cast, process, StopCauseFailure, "")
		if stopErr == nil {
			delete(runtime.processes, process.ID)
		}
		return errors.Join(err, stopErr)
	}
	if process.areaCallbackFinishedCast {
		return runtime.terminateProcess(cast, process, StopCauseCancel, "")
	}
	return nil
}

func detachedProcessLocals(program *Program) []RuntimeValue {
	locals := make([]RuntimeValue, len(program.locals))
	for index, slot := range program.locals {
		locals[index] = MissingRuntimeValue(slot.typ)
	}
	return locals
}

func (runtime *Runtime) detachedProcessCast(process *ProcessInstance) *castInstance {
	context := process.eventContext
	context.Tick, context.WorldRevision, context.ProcessID = runtime.currentTick, runtime.host.CurrentRevision(), process.ID
	return &castInstance{
		id: process.CastID, program: process.Program, caster: process.Owner, primaryTarget: process.LifecycleEntity,
		locals: cloneLocalFrame(process.locals), snapshots: cloneProcessSnapshots(process.snapshots), status: CastRunning,
		visibleRevision: runtime.host.CurrentRevision(), randomKey: process.randomKey, randomInvocations: cloneRandomInvocations(process.randomInvocations),
		eventContext: context, detachedProcess: process, detachedEvent: context,
	}
}

func activeCallbackProcess(cast *castInstance, processID ProcessID) (*ProcessInstance, error) {
	if cast == nil || cast.detachedProcess == nil {
		return nil, ErrCastInputRejected
	}
	process := cast.detachedProcess
	if process.ID != processID || process.CastID != cast.id || process.Status != ProcessRunning || process.Program != cast.program {
		return nil, ErrCastInputRejected
	}
	return process, nil
}

func cloneProcessSnapshots(values map[int]RuntimeValue) map[int]RuntimeValue {
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

func (runtime *Runtime) captureOwnedProcessSnapshots(process *ProcessInstance) error {
	callbackCast := runtime.detachedProcessCast(process)
	if err := runtime.captureSnapshots(callbackCast, snapshotProcessStart); err != nil {
		return err
	}
	process.snapshots = cloneProcessSnapshots(callbackCast.snapshots)
	process.visibleRevision = callbackCast.visibleRevision
	return nil
}

func (runtime *Runtime) hasOwnedProcessCapacityExcluding(owner EntityID, programID string, template UnitTemplateHandle, additional int, excluded map[EntityID]bool) bool {
	total, ownerCount, programCount, templateCount := 0, 0, 0, 0
	for _, process := range runtime.processes {
		if process.Scope != ProcessScopeEntity || process.Status != ProcessRunning {
			continue
		}
		if excluded[process.LifecycleEntity] {
			continue
		}
		total++
		if process.Owner == owner {
			ownerCount++
		}
		if process.Program != nil && process.Program.id == programID {
			programCount++
		}
		if process.UnitTemplate == template {
			templateCount++
		}
	}
	return total+additional <= runtime.options.MaxOwnedProcesses && ownerCount+additional <= runtime.options.MaxOwnedProcessesPerOwner && programCount+additional <= runtime.options.MaxOwnedProcessesPerProgram && templateCount+additional <= runtime.options.MaxOwnedProcessesPerTemplate
}

func (runtime *Runtime) previewOwnedProcessCapacity(host OwnedEntityRuntimeHost, command SpawnCommand) (ExpectedFailureReason, error) {
	excluded := make(map[EntityID]bool)
	preview, err := host.PreviewOwnedSpawn(command)
	if err != nil {
		return ExpectedFailureNone, err
	}
	if preview.FailureReason != ExpectedFailureNone {
		return preview.FailureReason, nil
	}
	for _, entity := range preview.ReplacedEntities {
		excluded[entity] = true
	}
	if !runtime.hasOwnedProcessCapacityExcluding(command.Owner, command.SourceSkillID, command.Template, command.Count, excluded) {
		return ExpectedFailureCapacityReached, nil
	}
	return ExpectedFailureNone, nil
}

func (runtime *Runtime) failOwnedProcess(process *ProcessInstance, cause error) error {
	if process != nil {
		return errors.Join(cause, runtime.stopOwnedProcess(process.ID, StopCauseFailure))
	}
	return cause
}

func (runtime *Runtime) handoffEntityProcesses(cast *castInstance) error {
	ids := make([]ProcessID, 0)
	for id, process := range runtime.processes {
		if process.CastID != cast.id || process.Scope != ProcessScopeEntity || process.Status != ProcessRunning || process.handedOff {
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
	valid, invalid := make([]ProcessID, 0, len(ids)), make([]ProcessID, 0)
	for _, id := range ids {
		process := runtime.processes[id]
		if _, alive := host.OwnedEntity(process.LifecycleEntity); !alive {
			invalid = append(invalid, id)
		} else {
			valid = append(valid, id)
		}
	}
	var cleanupErr error
	for _, id := range invalid {
		process := runtime.processes[id]
		cleanupErr = errors.Join(cleanupErr, runtime.terminateProcess(cast, process, StopCauseCancel, "cancel"))
	}
	if cleanupErr != nil {
		for _, id := range valid {
			process := runtime.processes[id]
			cleanupErr = errors.Join(cleanupErr, runtime.terminateProcess(cast, process, StopCauseCancel, "cancel"))
		}
		return cleanupErr
	}
	for _, id := range valid {
		process := runtime.processes[id]
		process.handedOff = true
		runtime.ownedProcesses[process.ID] = process
	}
	return nil
}

func (runtime *Runtime) OwnedProcesses(owner EntityID) []OwnedProcessSnapshot {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	result := make([]OwnedProcessSnapshot, 0, len(runtime.ownedProcesses))
	for _, process := range runtime.ownedProcesses {
		if owner != 0 && process.Owner != owner {
			continue
		}
		programID := ""
		if process.Program != nil {
			programID = process.Program.id
		}
		result = append(result, OwnedProcessSnapshot{ID: process.ID, Owner: process.Owner, LifecycleEntity: process.LifecycleEntity, SourceCastID: process.CastID, ProgramID: programID, StartTick: process.StartTick, EndTick: process.EndTick, Status: process.Status, HandedOff: process.handedOff})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (runtime *Runtime) reapUnhandedEntityProcesses() error {
	ids := make([]ProcessID, 0)
	host, hostOK := runtime.host.(OwnedEntityRuntimeHost)
	for id, process := range runtime.processes {
		if process.Scope != ProcessScopeEntity || process.Status != ProcessRunning || process.handedOff {
			continue
		}
		if !hostOK {
			return ErrHostContractViolation
		}
		if _, alive := host.OwnedEntity(process.LifecycleEntity); !alive {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		process := runtime.processes[id]
		if err := runtime.terminateProcess(runtime.casts[process.CastID], process, StopCauseCancel, "cancel"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) advanceOwnedProcesses() error {
	if err := runtime.reapUnhandedEntityProcesses(); err != nil {
		return err
	}
	if err := runtime.reapInvalidOwnedProcesses(); err != nil {
		return err
	}
	ids := make([]ProcessID, 0, len(runtime.ownedProcesses))
	for id := range runtime.ownedProcesses {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		process := runtime.ownedProcesses[id]
		if process == nil || process.Status != ProcessRunning || process.NextTick > runtime.currentTick {
			continue
		}
		stepCast := runtime.detachedProcessCast(process)
		signals, err := runtime.stepProcessMotion(stepCast, process)
		if err != nil {
			return runtime.failOwnedProcess(process, err)
		}
		template := process.Program.processTemplates[process.TemplateIndex]
		if template.area != nil {
			signals = areaProcessSignals(signals)
			areaSignals, areaErr := runtime.stepAreaMembership(stepCast, process)
			if areaErr != nil {
				return runtime.failOwnedProcess(process, areaErr)
			}
			signals = append(signals, areaSignals...)
		}
		runtime.emitProcessPresentation(stepCast, process, PresentationProcessUpdate, "", "", stepCast.visibleRevision)
		runtime.emitProcessSignals(stepCast, process, signals, stepCast.visibleRevision)
		if cast := runtime.casts[process.CastID]; cast != nil {
			cast.visibleRevision = maxRevision(cast.visibleRevision, stepCast.visibleRevision)
		}
		if err := runtime.dispatchOwnedProcessSignals(process, signals); err != nil {
			return runtime.failOwnedProcess(process, err)
		}
		interval := Tick(1)
		if template.area != nil {
			interval = template.intervalTicks
		}
		process.NextTick = saturatingTickAdd(runtime.currentTick, interval)
	}
	if err := runtime.collectHostEvents(); err != nil {
		return err
	}
	return runtime.reapOwnedProcesses()
}

func (runtime *Runtime) reapInvalidOwnedProcesses() error {
	host, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok && len(runtime.ownedProcesses) != 0 {
		return ErrHostContractViolation
	}
	type reapedProcess struct {
		id    ProcessID
		cause StopCause
		event string
	}
	items := make([]reapedProcess, 0)
	for id, process := range runtime.ownedProcesses {
		moving := process.Program != nil && int(process.TemplateIndex) < len(process.Program.processTemplates) && process.Program.processTemplates[process.TemplateIndex].motion != nil
		if process.Motion.Stage == MotionStageCompleted || process.EndTick <= runtime.currentTick {
			if moving {
				if process.Motion.Stage == MotionStageCompleted {
					items = append(items, reapedProcess{id: id, cause: StopCauseEnd, event: "end"})
				}
				continue
			}
			items = append(items, reapedProcess{id: id, cause: StopCauseEnd, event: "end"})
			continue
		}
		_, alive := host.OwnedEntity(process.LifecycleEntity)
		if !alive || process.Program == nil {
			items = append(items, reapedProcess{id: id, cause: StopCauseCancel, event: "cancel"})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].id < items[j].id })
	var firstErr error
	for _, item := range items {
		if err := runtime.terminateOwnedProcess(nil, item.id, item.cause, item.event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) dispatchOwnedProcessSignals(process *ProcessInstance, signals []ProcessSignal) error {
	baseContext := process.eventContext
	baseContext.Target = process.LifecycleEntity
	for _, signal := range normalizeProcessSignals(signals) {
		if process.areaCallbackFinishedCast {
			break
		}
		context := baseContext
		if signal.Target != 0 {
			context.Target = signal.Target
		}
		context.MembershipTicks = signal.MembershipTicks
		context.EnterCount = signal.EnterCount
		process.eventContext = context
		if err := runtime.runOwnedProcessCallback(process, string(signal.Kind)); err != nil {
			return err
		}
	}
	process.eventContext = baseContext
	return nil
}

func (runtime *Runtime) reapOwnedProcesses() error {
	host, hostOK := runtime.host.(OwnedEntityRuntimeHost)
	type expiredProcess struct {
		id    ProcessID
		cause StopCause
		event string
	}
	expired := make([]expiredProcess, 0)
	for id, process := range runtime.ownedProcesses {
		if !hostOK {
			return ErrHostContractViolation
		}
		_, alive := host.OwnedEntity(process.LifecycleEntity)
		if process.Motion.Stage == MotionStageCompleted || process.EndTick <= runtime.currentTick {
			expired = append(expired, expiredProcess{id: id, cause: StopCauseEnd, event: "end"})
		} else if !alive || process.Program == nil {
			expired = append(expired, expiredProcess{id: id, cause: StopCauseCancel, event: "cancel"})
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].id < expired[j].id })
	var firstErr error
	for _, item := range expired {
		if err := runtime.terminateOwnedProcess(nil, item.id, item.cause, item.event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) runOwnedProcessCallback(process *ProcessInstance, event string) error {
	if process == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return nil
	}
	if event == "end" {
		if process.Motion.EndCallbackEmitted {
			return nil
		}
		process.Motion.EndCallbackEmitted = true
	}
	template := process.Program.processTemplates[process.TemplateIndex]
	for _, callback := range template.callbacks {
		if callback.event != event {
			continue
		}
		callbackCast := runtime.detachedProcessCast(process)
		control, err := runtime.executeOperation(callbackCast, callback.operation)
		process.locals = cloneLocalFrame(callbackCast.locals)
		process.snapshots = cloneProcessSnapshots(callbackCast.snapshots)
		process.randomInvocations = cloneRandomInvocations(callbackCast.randomInvocations)
		process.visibleRevision = callbackCast.visibleRevision
		if cast := runtime.casts[process.CastID]; cast != nil && !process.handedOff {
			cast.visibleRevision = maxRevision(cast.visibleRevision, callbackCast.visibleRevision)
		}
		if err != nil {
			return err
		}
		if control.kind != flowContinue {
			if control.kind != flowFinish || template.area == nil || process.handedOff {
				return ErrProgramInvariant
			}
			ownerCast := runtime.casts[process.CastID]
			if ownerCast == nil || ownerCast.status == CastFinished || ownerCast.status == CastFailed || ownerCast.logicalFinished {
				return ErrProgramInvariant
			}
			ownerCast.areaCallbackFinish = true
			process.areaCallbackFinishedCast = true
		}
		runtime.appendRuntimeEvent(RuntimeEvent{Tick: runtime.currentTick, Kind: "owned_process_callback_" + event, Entity: process.LifecycleEntity, Context: callbackCast.detachedEvent})
	}
	return nil
}

func (runtime *Runtime) stopOwnedProcess(id ProcessID, cause StopCause) error {
	return runtime.terminateOwnedProcess(nil, id, cause, "")
}

func (runtime *Runtime) terminateOwnedProcess(cast *castInstance, id ProcessID, cause StopCause, callbackEvent string) error {
	process := runtime.ownedProcesses[id]
	if process == nil {
		return nil
	}
	if err := runtime.terminateProcess(cast, process, cause, callbackEvent); err != nil {
		return err
	}
	delete(runtime.ownedProcesses, id)
	return nil
}

func (runtime *Runtime) RemoveProgram(programID string) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	ids := make([]ProcessID, 0)
	for id, process := range runtime.processes {
		if process.Program != nil && process.Program.id == programID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		process := runtime.processes[id]
		cast := runtime.casts[process.CastID]
		callbackEvent := ""
		if process.handedOff {
			cast = nil
			callbackEvent = "cancel"
		}
		stopErr := runtime.terminateProcess(cast, process, StopCauseCancel, callbackEvent)
		if stopErr != nil {
			if firstErr == nil {
				firstErr = stopErr
			}
		} else {
			delete(runtime.ownedProcesses, id)
		}
	}
	if lifecycleHost, ok := runtime.host.(OwnedEntityRuntimeHost); ok {
		if err := lifecycleHost.RemoveOwnedEntitiesByProgram(programID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (runtime *Runtime) Shutdown() error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	ids := make([]ProcessID, 0, len(runtime.processes))
	for id, process := range runtime.processes {
		if process.Status == ProcessRunning {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var firstErr error
	for _, id := range ids {
		process := runtime.processes[id]
		callbackEvent := ""
		if process.handedOff {
			callbackEvent = "cancel"
		}
		stopErr := runtime.terminateProcess(nil, process, StopCauseCancel, callbackEvent)
		if stopErr != nil {
			if firstErr == nil {
				firstErr = stopErr
			}
		} else {
			delete(runtime.ownedProcesses, id)
		}
	}
	if lifecycleHost, ok := runtime.host.(OwnedEntityRuntimeHost); ok {
		if err := lifecycleHost.RemoveOwnedEntitiesForMatchEnd(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
