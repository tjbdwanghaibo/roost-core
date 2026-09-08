package skill

import "fmt"

type flowControlKind uint8

const (
	flowContinue flowControlKind = iota
	flowGoto
	flowFinish
	flowSuspend
)

type flowControl struct {
	kind    flowControlKind
	phase   PhaseIndex
	dueTick Tick
	payload scheduledTaskPayload
}

func (runtime *Runtime) executeCast(cast *castInstance) error {
	for transitions := 0; transitions <= len(cast.program.phases); transitions++ {
		if int(cast.currentPhase) >= len(cast.program.phases) {
			return ErrProgramInvariant
		}
		phase := cast.program.phases[cast.currentPhase]
		if err := runtime.captureSnapshots(cast, snapshotPhaseStart); err != nil {
			return err
		}
		operation, found := phaseRootOperation(cast.program, phase, "enter")
		if !found {
			return ErrAsyncFlowNotScheduled
		}
		control, err := runtime.executeOperation(cast, operation)
		if err != nil {
			return err
		}
		switch control.kind {
		case flowGoto:
			if err := runtime.stopProcesses(cast, false); err != nil {
				return err
			}
			runtime.cancelPhaseTasks(cast, cast.phaseToken)
			cast.phaseToken++
			cast.currentPhase = control.phase
			continue
		case flowFinish:
			return runtime.finishCast(cast)
		case flowSuspend:
			return runtime.schedule(cast, control.dueTick, control.payload)
		case flowContinue:
			return runtime.beginPolicyWait(cast)
		default:
			return ErrProgramInvariant
		}
	}
	return ErrProgramInvariant
}

func (runtime *Runtime) executeOperations(cast *castInstance, operations []OperationIndex) (flowControl, error) {
	for index, operation := range operations {
		control, err := runtime.executeOperation(cast, operation)
		if err != nil {
			return control, err
		}
		if control.kind != flowContinue {
			if control.kind == flowSuspend {
				if err := appendSuspensionTail(control.payload, operations[index+1:]); err != nil {
					return flowControl{}, err
				}
			}
			return control, nil
		}
	}
	return flowControl{kind: flowContinue}, nil
}

func appendSuspensionTail(payload scheduledTaskPayload, tail []OperationIndex) error {
	if len(tail) == 0 {
		return nil
	}
	switch task := payload.(type) {
	case *flowContinuationTask:
		task.Operations = append(task.Operations, tail...)
	case *repeatIterationTask:
		task.Tail = append(task.Tail, tail...)
	default:
		return ErrProgramInvariant
	}
	return nil
}

func phaseRootOperation(program *Program, phase phaseProgram, event string) (OperationIndex, bool) {
	for _, rootIndex := range phase.roots {
		if int(rootIndex) >= len(program.roots) {
			return 0, false
		}
		root := program.roots[rootIndex]
		if root.event == event && root.hasOperation {
			return root.operation, true
		}
	}
	return 0, false
}

func (runtime *Runtime) executeOperation(cast *castInstance, index OperationIndex) (flowControl, error) {
	if int(index) >= len(cast.program.operations) || cast.program.operations[index] == nil {
		return flowControl{}, ErrProgramInvariant
	}
	switch operation := cast.program.operations[index].(type) {
	case sequenceOperation:
		return runtime.executeOperations(cast, operation.children)
	case parallelOperation:
		for _, branch := range operation.branches {
			control, err := runtime.executeOperation(cast, branch)
			if err != nil {
				return control, err
			}
			if control.kind == flowSuspend {
				if err := runtime.schedule(cast, control.dueTick, control.payload); err != nil {
					return flowControl{}, err
				}
				continue
			}
			if control.kind != flowContinue {
				return control, nil
			}
		}
		return flowControl{kind: flowContinue}, nil
	case branchOperation:
		condition, err := runtime.evalValue(cast, operation.condition)
		if err != nil {
			return flowControl{}, err
		}
		value, ok := condition.Bool()
		if !ok {
			return flowControl{}, ErrRuntimeTypeMismatch
		}
		if value {
			return runtime.executeOperation(cast, operation.thenOperation)
		}
		if operation.hasElse {
			return runtime.executeOperation(cast, operation.elseOperation)
		}
		return flowControl{}, nil
	case repeatOperation:
		timesValue, err := runtime.evalValue(cast, operation.times)
		if err != nil {
			return flowControl{}, err
		}
		times, ok := timesValue.Int()
		if !ok || times < 0 || times > int64(cast.program.limits.Repeat) {
			return flowControl{}, ErrProgramInvariant
		}
		if times == 0 {
			return flowControl{kind: flowContinue}, nil
		}
		if operation.intervalTicks != 0 {
			previous := cast.locals[operation.indexLocal]
			cast.locals[operation.indexLocal] = IntRuntimeValue(0, quantityCount)
			control, executeErr := runtime.executeOperation(cast, operation.body)
			cast.locals[operation.indexLocal] = previous
			if executeErr != nil || control.kind != flowContinue {
				return control, executeErr
			}
			if times == 1 {
				return flowControl{kind: flowContinue}, nil
			}
			frame := runtime.retainLocalFrame(cast.locals)
			return flowControl{
				kind: flowSuspend, dueTick: runtime.currentTick + operation.intervalTicks,
				payload: &repeatIterationTask{
					CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame,
					Body: operation.body, IndexLocal: operation.indexLocal, Iteration: 1,
					Times: times, Interval: operation.intervalTicks,
				},
			}, nil
		}
		previous := cast.locals[operation.indexLocal]
		for iteration := int64(0); iteration < times; iteration++ {
			cast.locals[operation.indexLocal] = IntRuntimeValue(iteration, quantityCount)
			control, err := runtime.executeOperation(cast, operation.body)
			if err != nil || control.kind != flowContinue {
				return control, err
			}
		}
		cast.locals[operation.indexLocal] = previous
		return flowControl{kind: flowContinue}, nil
	case waitOperation:
		frame := runtime.retainLocalFrame(cast.locals)
		return flowControl{
			kind: flowSuspend, dueTick: runtime.currentTick + operation.ticks,
			payload: &flowContinuationTask{
				CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame,
				Operations: []OperationIndex{operation.then},
			},
		}, nil
	case queryOperation:
		if int(operation.selector) >= len(cast.program.selectors) {
			return flowControl{}, ErrProgramInvariant
		}
		return runtime.executeQuery(cast, cast.program.selectors[operation.selector])
	case captureSnapshotOperation:
		result, err := runtime.executeCaptureSnapshot(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case restoreSnapshotOperation:
		result, err := runtime.executeRestoreSnapshot(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case damageOperation:
		result, err := runtime.executeDamage(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case healOperation:
		result, err := runtime.executeHeal(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case shieldOperation:
		result, err := runtime.executeShield(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case statusOperation:
		result, err := runtime.executeStatus(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case modifyStatusInstanceOperation:
		result, err := runtime.executeStatusInstance(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case attributeModifierOperation:
		result, err := runtime.executeAttributeModifier(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case resourceOperation:
		return flowControl{}, runtime.executeResource(cast, operation)
	case memoryOperation:
		return flowControl{}, runtime.executeMemory(cast, operation)
	case stateOperation:
		result, err := runtime.executeStateMutation(cast, operation)
		return runtime.resolveStateEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case abilityStateOperation:
		result, err := runtime.executeAbilityStateMutation(cast, operation)
		return runtime.resolveAbilityEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case modifyProcessOperation:
		return flowControl{kind: flowContinue}, runtime.executeModifyProcess(cast, operation)
	case spawnOperation:
		result, err := runtime.executeOwnedSpawn(cast, operation)
		if err == nil && cast.areaCallbackFinish {
			return flowControl{kind: flowFinish}, nil
		}
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case entityCommandOperation:
		result, err := runtime.executeOwnedCommand(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case teleportOperation:
		result, err := runtime.executeTeleport(cast, operation)
		return runtime.resolveHostEffectExecution(cast, operation.effectContinuations, operation.effectIndex, result, err)
	case motionImpulseOperation:
		return flowControl{}, runtime.executeMotionImpulse(cast, operation)
	case stopMovementOperation:
		target, err := runtime.evalEntity(cast, operation.target)
		if err != nil {
			return flowControl{}, err
		}
		runtime.emitEffectPresentation(cast, operation.effectContinuations, operation.effectIndex, runtime.host.CurrentRevision(), PresentationAnchor{Source: cast.caster, Target: target})
		return flowControl{}, nil
	case gotoOperation:
		return flowControl{kind: flowGoto, phase: operation.phase}, nil
	case finishOperation:
		return flowControl{kind: flowFinish}, nil
	default:
		return flowControl{}, fmt.Errorf("%w: operation %T", ErrProgramInvariant, operation)
	}
}

func (runtime *Runtime) executeRepeatIteration(cast *castInstance, task *repeatIterationTask) (flowControl, error) {
	if int(task.IndexLocal) >= len(cast.locals) || task.Iteration >= task.Times {
		return flowControl{}, ErrProgramInvariant
	}
	previous := cast.locals[task.IndexLocal]
	cast.locals[task.IndexLocal] = IntRuntimeValue(task.Iteration, quantityCount)
	control, err := runtime.executeOperation(cast, task.Body)
	cast.locals[task.IndexLocal] = previous
	if err != nil || control.kind != flowContinue {
		return control, err
	}
	if task.Iteration+1 < task.Times {
		frame := runtime.retainLocalFrame(cast.locals)
		return flowControl{
			kind: flowSuspend, dueTick: runtime.currentTick + task.Interval,
			payload: &repeatIterationTask{
				CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame,
				Body: task.Body, IndexLocal: task.IndexLocal, Iteration: task.Iteration + 1,
				Times: task.Times, Interval: task.Interval, Tail: append([]OperationIndex(nil), task.Tail...),
			},
		}, nil
	}
	return runtime.executeOperations(cast, task.Tail)
}

func (runtime *Runtime) resolveControl(cast *castInstance, control flowControl) error {
	switch control.kind {
	case flowSuspend:
		return runtime.schedule(cast, control.dueTick, control.payload)
	case flowGoto:
		if err := runtime.stopProcesses(cast, false); err != nil {
			return err
		}
		runtime.cancelPhaseTasks(cast, cast.phaseToken)
		cast.phaseToken++
		cast.currentPhase = control.phase
		cast.status = CastRunning
		return runtime.executeCast(cast)
	case flowFinish:
		return runtime.finishCast(cast)
	case flowContinue:
		if cast.pendingTasks > 0 {
			cast.status = CastSuspended
			return nil
		}
		if cast.logicalFinished {
			return runtime.beginCastRecovery(cast)
		}
		return ErrProgramInvariant
	default:
		return ErrProgramInvariant
	}
}

func (runtime *Runtime) finishCast(cast *castInstance) error {
	return runtime.beginCastRecovery(cast)
}
