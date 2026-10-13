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

// executeOperation 执行一个 lower 后的操作：控制流操作（sequence / parallel / if / repeat / wait / goto / finish）
// 决定后续走向，效果操作经各自的 execute 函数落到 Host，再由 resolve*EffectExecution 处理结果续行。
func (runtime *Runtime) executeOperation(cast *castInstance, index OperationIndex) (flowControl, error) {
	if int(index) >= len(cast.program.operations) || cast.program.operations[index] == nil {
		return flowControl{}, ErrProgramInvariant
	}
	switch operation := cast.program.operations[index].(type) {
	case sequenceOperation:
		return runtime.executeOperations(cast, operation.children)
	case parallelOperation:
		return runtime.executeParallel(cast, operation)
	case branchOperation:
		return runtime.executeBranch(cast, operation)
	case repeatOperation:
		return runtime.executeRepeat(cast, operation)
	case waitOperation:
		return runtime.suspendWait(cast, operation), nil
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
	case modifySpawnOperation:
		return flowControl{kind: flowContinue}, runtime.executeModifySpawn(cast, operation)
	case summonOperation:
		result, err := runtime.executeOwnedSummon(cast, operation)
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
		return flowControl{}, runtime.executeStopMovement(cast, operation)
	case gotoOperation:
		return flowControl{kind: flowGoto, phase: operation.phase}, nil
	case finishOperation:
		return flowControl{kind: flowFinish}, nil
	default:
		return flowControl{}, fmt.Errorf("%w: operation %T", ErrProgramInvariant, operation)
	}
}

// executeParallel 依次执行各分支：挂起的分支交给排程器后继续下一个分支，其他非 continue 结果（goto / finish）
// 立即结束整个 parallel。
func (runtime *Runtime) executeParallel(cast *castInstance, operation parallelOperation) (flowControl, error) {
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
}

// executeBranch 求值条件（必须是 bool）并执行对应分支；条件为假且没有 else 时什么也不做。
func (runtime *Runtime) executeBranch(cast *castInstance, operation branchOperation) (flowControl, error) {
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
}

// executeRepeat 求值次数（0 到程序上限）。无间隔时在本 tick 内依次执行全部迭代，循环变量在结束后恢复；
// 有间隔时见 startIntervalRepeat。
func (runtime *Runtime) executeRepeat(cast *castInstance, operation repeatOperation) (flowControl, error) {
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
		return runtime.startIntervalRepeat(cast, operation, times)
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
}

// startIntervalRepeat 立即执行第 0 次迭代，其余迭代以 repeatIterationTask 按间隔挂起（由 executeRepeatIteration 续行）。
func (runtime *Runtime) startIntervalRepeat(cast *castInstance, operation repeatOperation, times int64) (flowControl, error) {
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

// suspendWait 保存当前局部变量帧，挂起到 ticks 之后从 then 续行。
func (runtime *Runtime) suspendWait(cast *castInstance, operation waitOperation) flowControl {
	frame := runtime.retainLocalFrame(cast.locals)
	return flowControl{
		kind: flowSuspend, dueTick: runtime.currentTick + operation.ticks,
		payload: &flowContinuationTask{
			CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame,
			Operations: []OperationIndex{operation.then},
		},
	}
}

// executeStopMovement 求值目标，并发出锚定施法者与目标的效果表现事件。
func (runtime *Runtime) executeStopMovement(cast *castInstance, operation stopMovementOperation) error {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return err
	}
	runtime.emitEffectPresentation(cast, operation.effectContinuations, operation.effectIndex, runtime.host.CurrentRevision(), PresentationAnchor{Source: cast.caster, Target: target})
	return nil
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
