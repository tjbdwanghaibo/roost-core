package skill

import (
	"errors"
	"sort"
)

type ProcessStatus string

const (
	ProcessRunning   ProcessStatus = "running"
	ProcessEnded     ProcessStatus = "ended"
	ProcessCancelled ProcessStatus = "cancelled"
	ProcessFailed    ProcessStatus = "failed"
)

type ProcessScope string

const (
	ProcessScopePhase  ProcessScope = "phase"
	ProcessScopeCast   ProcessScope = "cast"
	ProcessScopeEntity ProcessScope = "entity"
)

type StopCause string

const (
	StopCauseEnd     StopCause = "end"
	StopCauseCancel  StopCause = "cancel"
	StopCauseFailure StopCause = "failure"
)

type MotionProcessStage uint8

const (
	MotionStageOutbound MotionProcessStage = iota + 1
	MotionStagePaused
	MotionStageReturning
	MotionStageCompleted
)

type MotionState struct {
	Initialized        bool
	Position           Position
	TrajectoryPosition Position
	Origin             Position
	Direction          Direction
	FrameAnchor        Position
	FrameAnchored      bool
	Stage              MotionProcessStage
	Tick               Tick
	TrajectoryIndex    int
	ReflectCount       int
	PierceCount        int
	PauseCount         Tick
	ReturnCount        Tick
	TargetLostEmitted  bool
	EndCallbackEmitted bool
	CarryTarget        EntityID
	CarryAttached      bool
}

type ProcessNumericState struct {
	Initialized bool
	Properties  []numericPropertyState
}

type ProcessInstance struct {
	ID                ProcessID
	CastID            CastID
	TemplateIndex     ProcessTemplateIndex
	UnitTemplate      UnitTemplateHandle
	Status            ProcessStatus
	StartTick         Tick
	NextTick          Tick
	EndTick           Tick
	Scope             ProcessScope
	HostState         ProcessHostState
	Motion            MotionState
	Numeric           ProcessNumericState
	Owner             EntityID
	LifecycleEntity   EntityID
	Program           *Program
	inputs            []RuntimeValue
	memory            []RuntimeValue
	locals            []RuntimeValue
	snapshots         map[int]RuntimeValue
	randomKey         [32]byte
	randomInvocations map[RandomSiteIndex]uint64
	visibleRevision   WorldRevision
	eventContext      EventContext
	AreaMembers       map[EntityID]AreaMemberState

	phaseToken               uint64
	stopCause                StopCause
	handedOff                bool
	areaCallbackFinishedCast bool
}

type ProcessSignalKind string

const (
	ProcessSignalHit        ProcessSignalKind = "hit"
	ProcessSignalCollision  ProcessSignalKind = "collision"
	ProcessSignalTargetLost ProcessSignalKind = "target_lost"
	ProcessSignalTransition ProcessSignalKind = "transition"
	ProcessSignalLeave      ProcessSignalKind = "leave"
	ProcessSignalEnter      ProcessSignalKind = "enter"
	ProcessSignalTick       ProcessSignalKind = "tick"
	ProcessSignalEnd        ProcessSignalKind = "end"
	ProcessSignalCancel     ProcessSignalKind = "cancel"
)

type ProcessSignal struct {
	Kind            ProcessSignalKind
	Target          EntityID
	Distance        int64
	ContactOrdinal  uint64
	MembershipTicks int64
	EnterCount      int64
}

func normalizeProcessSignals(signals []ProcessSignal) []ProcessSignal {
	result := append([]ProcessSignal(nil), signals...)
	sort.SliceStable(result, func(left, right int) bool {
		leftRank, rightRank := processSignalRank(result[left].Kind), processSignalRank(result[right].Kind)
		leftContact, rightContact := leftRank == 0, rightRank == 0
		if leftContact && rightContact {
			if result[left].Distance != result[right].Distance {
				return result[left].Distance < result[right].Distance
			}
			if result[left].ContactOrdinal != result[right].ContactOrdinal {
				return result[left].ContactOrdinal < result[right].ContactOrdinal
			}
			if result[left].Target != result[right].Target {
				return result[left].Target < result[right].Target
			}
			return processSignalRankWithinContact(result[left].Kind) < processSignalRankWithinContact(result[right].Kind)
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return result[left].Target < result[right].Target
	})
	return result
}

func processSignalRank(kind ProcessSignalKind) int {
	switch kind {
	case ProcessSignalHit, ProcessSignalCollision:
		return 0
	case ProcessSignalTargetLost:
		return 1
	case ProcessSignalTransition:
		return 2
	case ProcessSignalLeave:
		return 3
	case ProcessSignalEnter:
		return 4
	case ProcessSignalTick:
		return 5
	case ProcessSignalEnd:
		return 6
	case ProcessSignalCancel:
		return 7
	default:
		return 8
	}
}

func processSignalRankWithinContact(kind ProcessSignalKind) int {
	if kind == ProcessSignalHit {
		return 0
	}
	return 1
}

func processStatusForStop(cause StopCause) ProcessStatus {
	switch cause {
	case StopCauseEnd:
		return ProcessEnded
	case StopCauseFailure:
		return ProcessFailed
	default:
		return ProcessCancelled
	}
}

func (runtime *Runtime) stopProcess(cast *castInstance, process *ProcessInstance, cause StopCause) error {
	if process == nil || process.Status != ProcessRunning {
		return nil
	}
	stopAreaMembership(process, false)
	detachErr := runtime.detachMotionCarry(cast, process)
	requiredRevision := runtime.host.CurrentRevision()
	if cast != nil {
		requiredRevision = cast.visibleRevision
	}
	receipt, err := runtime.host.StopProcess(ProcessStopCommand{Meta: ProcessCommandMeta{
		RequiredRevision: requiredRevision,
		ProcessID:        process.ID,
	}}, process.HostState)
	if err != nil {
		return errors.Join(detachErr, err)
	}
	process.Status = processStatusForStop(cause)
	process.stopCause = cause
	process.HostState.Active = false
	runtime.emitProcessPresentation(cast, process, PresentationProcessStop, "", cause, receipt.Revision)
	if process.Scope == ProcessScopeEntity {
		process.Program = nil
		process.inputs = nil
		process.memory = nil
		process.locals = nil
		process.snapshots = nil
		process.randomInvocations = nil
	}
	if cast != nil {
		cast.visibleRevision = maxRevision(cast.visibleRevision, receipt.Revision)
		detachErr = errors.Join(detachErr, runtime.drainHostEvents(cast))
	}
	return detachErr
}

func (runtime *Runtime) detachMotionCarry(cast *castInstance, process *ProcessInstance) error {
	if process == nil || !process.Motion.CarryAttached || process.Motion.CarryTarget == 0 {
		return nil
	}
	target := process.Motion.CarryTarget
	process.Motion.CarryTarget = 0
	process.Motion.CarryAttached = false
	requiredRevision := runtime.host.CurrentRevision()
	if cast != nil {
		requiredRevision = cast.visibleRevision
	}
	result, err := runtime.host.StepProcess(ProcessStepCommand{
		Meta: ProcessCommandMeta{RequiredRevision: requiredRevision, ProcessID: process.ID},
		Motion: CarryMotionStep{
			Target:   target,
			Position: process.Motion.Position,
			Attached: false,
		},
	}, process.HostState)
	if err != nil {
		return err
	}
	process.HostState = result.State
	if cast != nil {
		cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
		process.visibleRevision = cast.visibleRevision
		return runtime.drainHostEvents(cast)
	} else {
		process.visibleRevision = maxRevision(process.visibleRevision, result.Commit.Revision)
	}
	return nil
}

// terminateProcess keeps carry cleanup ahead of any lifecycle callback. The
// attachment is process-owned, so clearing it first makes retrying or nested
// callbacks harmless even when the Host reports a detach failure.
func (runtime *Runtime) terminateProcess(cast *castInstance, process *ProcessInstance, cause StopCause, callbackEvent string) error {
	detachErr := runtime.detachMotionCarry(cast, process)
	var areaErr error
	if process != nil && process.Program != nil && int(process.TemplateIndex) < len(process.Program.processTemplates) {
		template := process.Program.processTemplates[process.TemplateIndex]
		if template.area != nil {
			areaErr = runtime.dispatchOwnedProcessSignals(process, stopAreaMembership(process, template.emitLeaveOnStop))
		}
	}
	var callbackErr error
	// Area callback completion suppresses only the remaining signals of that
	// Area process. Other processes owned by the cast still need their normal
	// terminal cleanup callbacks during the same stop sweep.
	callbackLive := process != nil && process.Status == ProcessRunning && !process.areaCallbackFinishedCast
	if areaErr == nil && callbackEvent != "" && callbackLive {
		callbackErr = runtime.runOwnedProcessCallback(process, callbackEvent)
	}
	stopErr := runtime.stopProcess(cast, process, cause)
	return errors.Join(detachErr, areaErr, callbackErr, stopErr)
}

func (runtime *Runtime) stopProcesses(cast *castInstance, includeCastScope bool) error {
	return runtime.stopScopedProcesses(cast, includeCastScope, includeCastScope)
}

func (runtime *Runtime) stopFinishingProcesses(cast *castInstance) error {
	return runtime.stopScopedProcesses(cast, true, false)
}

func (runtime *Runtime) stopScopedProcesses(cast *castInstance, includeCastScope, includeEntityScope bool) error {
	processIDs := make([]ProcessID, 0, len(runtime.processes))
	for processID, process := range runtime.processes {
		if process.CastID != cast.id || process.Status != ProcessRunning {
			continue
		}
		if process.Scope == ProcessScopePhase || includeCastScope && process.Scope == ProcessScopeCast || includeEntityScope && process.Scope == ProcessScopeEntity && !process.handedOff {
			processIDs = append(processIDs, processID)
		}
	}
	sort.Slice(processIDs, func(left, right int) bool { return processIDs[left] < processIDs[right] })
	var firstErr error
	for _, processID := range processIDs {
		process := runtime.processes[processID]
		callbackEvent := ""
		if process.Scope == ProcessScopeEntity {
			callbackEvent = "cancel"
		}
		if err := runtime.terminateProcess(cast, process, StopCauseCancel, callbackEvent); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
