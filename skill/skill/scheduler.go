package skill

import (
	"container/heap"
	"sort"
)

// FrameID identifies an immutable snapshot of a cast's local slots. Scheduled
// work keeps the ID rather than consulting the cast's mutable current locals.
type FrameID uint64

type scheduledTaskPayload interface {
	isScheduledTaskPayload()
	frameID() FrameID
}

type flowContinuationTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	Operations []OperationIndex
}

func (*flowContinuationTask) isScheduledTaskPayload() {}
func (task *flowContinuationTask) frameID() FrameID   { return task.Frame }

type repeatIterationTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	Body       OperationIndex
	IndexLocal LocalIndex
	Iteration  int64
	Times      int64
	Interval   Tick
	Tail       []OperationIndex
}

func (*repeatIterationTask) isScheduledTaskPayload() {}
func (task *repeatIterationTask) frameID() FrameID   { return task.Frame }

type phaseTimeoutTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
}

func (*phaseTimeoutTask) isScheduledTaskPayload() {}
func (task *phaseTimeoutTask) frameID() FrameID   { return task.Frame }

type chainHopTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	Operation  OperationIndex
	Hop        int
}

func (*chainHopTask) isScheduledTaskPayload() {}
func (task *chainHopTask) frameID() FrameID   { return task.Frame }

type processStepTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	ProcessID  ProcessID
}

func (*processStepTask) isScheduledTaskPayload() {}
func (task *processStepTask) frameID() FrameID   { return task.Frame }

type castCommitTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
}

func (*castCommitTask) isScheduledTaskPayload() {}
func (task *castCommitTask) frameID() FrameID   { return task.Frame }

type castExecuteTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
}

func (*castExecuteTask) isScheduledTaskPayload() {}
func (task *castExecuteTask) frameID() FrameID   { return task.Frame }

type castRecoveryTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
}

func (*castRecoveryTask) isScheduledTaskPayload() {}
func (task *castRecoveryTask) frameID() FrameID   { return task.Frame }

type castPulseTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	PulseIndex int64
}

func (*castPulseTask) isScheduledTaskPayload() {}
func (task *castPulseTask) frameID() FrameID   { return task.Frame }

type castAutoReleaseTask struct {
	CastID     CastID
	PhaseToken uint64
	Frame      FrameID
	Reason     string
}

func (*castAutoReleaseTask) isScheduledTaskPayload() {}
func (task *castAutoReleaseTask) frameID() FrameID   { return task.Frame }

type ammoRechargeTask struct {
	Caster     EntityID
	Skill      string
	Generation uint64
}

func (*ammoRechargeTask) isScheduledTaskPayload() {}
func (*ammoRechargeTask) frameID() FrameID        { return 0 }

type passiveActivationTask struct {
	ID      PassiveActivationID
	Program *Program
	Event   EventContext
	Owner   EntityID
	Ability AbilityHandle
}

func (*passiveActivationTask) isScheduledTaskPayload() {}
func (*passiveActivationTask) frameID() FrameID        { return 0 }

type externalEventTask struct{ Event EventContext }

func (*externalEventTask) isScheduledTaskPayload() {}
func (*externalEventTask) frameID() FrameID        { return 0 }

type abilityOverlayExpiryTask struct {
	Owner     EntityID
	Ability   AbilityHandle
	OverlayID uint64
	Context   EventContext
}

func (*abilityOverlayExpiryTask) isScheduledTaskPayload() {}
func (*abilityOverlayExpiryTask) frameID() FrameID        { return 0 }

type scheduledTask struct {
	DueTick  Tick
	Sequence uint64
	Payload  scheduledTaskPayload
}

type taskHeap []scheduledTask

func (tasks taskHeap) Len() int { return len(tasks) }
func (tasks taskHeap) Less(left, right int) bool {
	if tasks[left].DueTick != tasks[right].DueTick {
		return tasks[left].DueTick < tasks[right].DueTick
	}
	return tasks[left].Sequence < tasks[right].Sequence
}
func (tasks taskHeap) Swap(left, right int) { tasks[left], tasks[right] = tasks[right], tasks[left] }
func (tasks *taskHeap) Push(value any)      { *tasks = append(*tasks, value.(scheduledTask)) }
func (tasks *taskHeap) Pop() any {
	old := *tasks
	last := len(old) - 1
	value := old[last]
	old[last] = scheduledTask{}
	*tasks = old[:last]
	return value
}

type scheduler struct{ tasks taskHeap }

func newScheduler() *scheduler {
	value := &scheduler{}
	heap.Init(&value.tasks)
	return value
}

func (value *scheduler) Len() int { return value.tasks.Len() }
func (value *scheduler) Push(task scheduledTask) {
	heap.Push(&value.tasks, task)
}
func (value *scheduler) Pop() scheduledTask {
	if value.Len() == 0 {
		return scheduledTask{}
	}
	return heap.Pop(&value.tasks).(scheduledTask)
}
func (value *scheduler) Peek() (scheduledTask, bool) {
	if value.Len() == 0 {
		return scheduledTask{}, false
	}
	return value.tasks[0], true
}

func cloneLocalFrame(values []RuntimeValue) []RuntimeValue {
	result := make([]RuntimeValue, len(values))
	for index, value := range values {
		result[index] = cloneRuntimeValue(value)
	}
	return result
}

func (runtime *Runtime) retainLocalFrame(values []RuntimeValue) FrameID {
	runtime.nextFrameID++
	frame := runtime.nextFrameID
	runtime.frames[frame] = cloneLocalFrame(values)
	return frame
}

func (runtime *Runtime) takeLocalFrame(frame FrameID) ([]RuntimeValue, bool) {
	values, found := runtime.frames[frame]
	if !found {
		return nil, false
	}
	delete(runtime.frames, frame)
	return cloneLocalFrame(values), true
}

func (runtime *Runtime) schedule(cast *castInstance, due Tick, payload scheduledTaskPayload) error {
	if payload == nil || payload.frameID() == 0 || due < runtime.currentTick {
		return ErrProgramInvariant
	}
	runtime.nextTaskSequence++
	runtime.scheduler.Push(scheduledTask{DueTick: due, Sequence: runtime.nextTaskSequence, Payload: payload})
	cast.pendingTasks++
	cast.status = CastSuspended
	return nil
}

func (runtime *Runtime) scheduleSystem(due Tick, payload scheduledTaskPayload) error {
	if payload == nil || due < runtime.currentTick {
		return ErrProgramInvariant
	}
	runtime.nextTaskSequence++
	runtime.scheduler.Push(scheduledTask{DueTick: due, Sequence: runtime.nextTaskSequence, Payload: payload})
	return nil
}

func (runtime *Runtime) cancelPhaseTasks(cast *castInstance, phaseToken uint64) {
	kept := runtime.scheduler.tasks[:0]
	for _, task := range runtime.scheduler.tasks {
		castID, token := scheduledTaskIdentity(task.Payload)
		if castID == cast.id && token == phaseToken {
			if frame := task.Payload.frameID(); frame != 0 {
				delete(runtime.frames, frame)
			}
			if cast.pendingTasks > 0 {
				cast.pendingTasks--
			}
			continue
		}
		kept = append(kept, task)
	}
	runtime.scheduler.tasks = kept
	heap.Init(&runtime.scheduler.tasks)
}

func (runtime *Runtime) Advance(tick Tick) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if tick < runtime.currentTick {
		return ErrReverseAdvance
	}
	if tick == runtime.currentTick {
		task, taskFound := runtime.scheduler.Peek()
		ownedDue, ownedFound := runtime.nextOwnedProcessTick()
		if (!taskFound || task.DueTick > tick) && (!ownedFound || ownedDue > tick) {
			return nil
		}
	}
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	for {
		task, found := runtime.scheduler.Peek()
		ownedDue, ownedFound := runtime.nextOwnedProcessTick()
		taskDue := Tick(0)
		if found {
			taskDue = task.DueTick
		}
		nextDue, dueFound := taskDue, found && taskDue <= tick
		if ownedFound && ownedDue <= tick && (!dueFound || ownedDue < nextDue) {
			nextDue, dueFound = ownedDue, true
		}
		if !dueFound {
			break
		}
		if nextDue > runtime.currentTick {
			if err := runtime.advanceHost(nextDue); err != nil {
				return err
			}
		} else if ownedFound && ownedDue <= runtime.currentTick {
			if err := runtime.advanceOwnedProcesses(); err != nil {
				return err
			}
		}
		if current, taskFound := runtime.scheduler.Peek(); taskFound && current.DueTick == nextDue {
			task = runtime.scheduler.Pop()
			if err := runtime.executeScheduledTask(task); err != nil {
				return err
			}
		}
	}
	if tick > runtime.currentTick {
		return runtime.advanceHost(tick)
	}
	return nil
}

func (runtime *Runtime) nextOwnedProcessTick() (Tick, bool) {
	var due Tick
	found := false
	for _, process := range runtime.ownedProcesses {
		if process.Status != ProcessRunning {
			continue
		}
		if !found || process.NextTick < due {
			due, found = process.NextTick, true
		}
	}
	return due, found
}

func (runtime *Runtime) advanceHost(tick Tick) error {
	if _, err := runtime.host.Advance(tick); err != nil {
		return err
	}
	runtime.currentTick = tick
	if err := runtime.collectHostEvents(); err != nil {
		return err
	}
	return runtime.advanceOwnedProcesses()
}

func (runtime *Runtime) collectHostEvents() error {
	events := runtime.host.Events(runtime.eventCursor)
	if len(events) == 0 {
		return nil
	}
	castIDs := make([]int, 0, len(runtime.casts))
	for castID := range runtime.casts {
		castIDs = append(castIDs, int(castID))
	}
	sort.Ints(castIDs)
	for _, event := range events {
		if err := runtime.dispatchEvent(event.Context); err != nil {
			return err
		}
		if event.Cursor > runtime.eventCursor {
			runtime.eventCursor = event.Cursor
		}
		runtime.recordStateEvent(event)
		for _, castID := range castIDs {
			cast := runtime.casts[CastID(castID)]
			if cast.status == CastRunning || cast.status == CastSuspended {
				runtime.appendCastEvent(cast, event)
			}
		}
	}
	if compactor, ok := runtime.host.(HostEventCompactor); ok && runtime.eventCursor != 0 {
		compactor.CompactEventsThrough(runtime.eventCursor)
	}
	return nil
}

func scheduledTaskIdentity(payload scheduledTaskPayload) (CastID, uint64) {
	switch task := payload.(type) {
	case *flowContinuationTask:
		return task.CastID, task.PhaseToken
	case *repeatIterationTask:
		return task.CastID, task.PhaseToken
	case *phaseTimeoutTask:
		return task.CastID, task.PhaseToken
	case *chainHopTask:
		return task.CastID, task.PhaseToken
	case *processStepTask:
		return task.CastID, task.PhaseToken
	case *castCommitTask:
		return task.CastID, task.PhaseToken
	case *castExecuteTask:
		return task.CastID, task.PhaseToken
	case *castRecoveryTask:
		return task.CastID, task.PhaseToken
	case *castPulseTask:
		return task.CastID, task.PhaseToken
	case *castAutoReleaseTask:
		return task.CastID, task.PhaseToken
	default:
		return 0, 0
	}
}

func (runtime *Runtime) executeScheduledTask(task scheduledTask) error {
	if recharge, ok := task.Payload.(*ammoRechargeTask); ok {
		return runtime.executeAmmoRecharge(recharge)
	}
	if passive, ok := task.Payload.(*passiveActivationTask); ok {
		return runtime.executePassiveActivation(passive)
	}
	if external, ok := task.Payload.(*externalEventTask); ok {
		return runtime.dispatchEvent(external.Event)
	}
	if overlay, ok := task.Payload.(*abilityOverlayExpiryTask); ok {
		return runtime.expireAbilityOverlay(overlay)
	}
	castID, phaseToken := scheduledTaskIdentity(task.Payload)
	locals, frameFound := runtime.takeLocalFrame(task.Payload.frameID())
	cast, castFound := runtime.casts[castID]
	if castFound && cast.pendingTasks > 0 {
		cast.pendingTasks--
	}
	if !frameFound || !castFound {
		return nil
	}
	if cast.phaseToken != phaseToken || cast.status == CastFailed || cast.status == CastFinished {
		if cast.pendingTasks == 0 && cast.logicalFinished && cast.status != CastFailed {
			return runtime.beginCastRecovery(cast)
		}
		return nil
	}
	cast.locals = locals
	var (
		control flowControl
		err     error
	)
	switch payload := task.Payload.(type) {
	case *flowContinuationTask:
		control, err = runtime.executeOperations(cast, payload.Operations)
	case *repeatIterationTask:
		control, err = runtime.executeRepeatIteration(cast, payload)
	case *processStepTask:
		err = runtime.executeProcessStep(cast, payload.ProcessID)
		control = flowControl{kind: flowContinue}
	case *castCommitTask:
		err = runtime.commitCast(cast)
		if err != nil {
			return runtime.failScheduledCast(cast, err)
		}
		return nil
	case *castExecuteTask:
		err = runtime.beginCastExecution(cast)
		if err != nil {
			return runtime.failScheduledCast(cast, err)
		}
		return nil
	case *castRecoveryTask:
		return runtime.completeCastRecovery(cast)
	case *castPulseTask:
		if err := runtime.executeCastPulse(cast, payload.PulseIndex); err != nil {
			return runtime.failScheduledCast(cast, err)
		}
		return nil
	case *castAutoReleaseTask:
		if err := runtime.executeAutoRelease(cast, payload.Reason); err != nil {
			return runtime.failScheduledCast(cast, err)
		}
		return nil
	default:
		return ErrProgramInvariant
	}
	if err != nil {
		cast.status, cast.failure = CastFailed, err.Error()
		_ = runtime.stopProcesses(cast, true)
		runtime.markAbilityCastFinished(cast)
		return err
	}
	return runtime.resolveControl(cast, control)
}

func (runtime *Runtime) failScheduledCast(cast *castInstance, err error) error {
	cast.status, cast.failure = CastFailed, err.Error()
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	_ = runtime.stopProcesses(cast, true)
	runtime.markAbilityCastFinished(cast)
	return err
}

func (runtime *Runtime) executeProcessStep(cast *castInstance, processID ProcessID) error {
	process := runtime.processes[processID]
	if process == nil || process.Status != ProcessRunning {
		return nil
	}
	signals, err := runtime.stepProcessMotion(cast, process)
	if err != nil {
		return err
	}
	runtime.emitProcessPresentation(cast, process, PresentationProcessUpdate, "", "", cast.visibleRevision)
	runtime.emitProcessSignals(cast, process, signals, cast.visibleRevision)
	return nil
}
