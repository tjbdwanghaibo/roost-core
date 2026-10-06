package skill

import (
	"errors"
	"sort"
)

type SpawnStatus string

const (
	SpawnRunning   SpawnStatus = "running"
	SpawnEnded     SpawnStatus = "ended"
	SpawnCancelled SpawnStatus = "cancelled"
	SpawnFailed    SpawnStatus = "failed"
	// SpawnStopPending 表示 Runtime 已请求停掉这个衍生物（任何停止入口），但宿主 StopSpawn 失败，衍生物仍在
	// 宿主侧运行。Runtime 不再推进它（不步进、不派发信号、不跑回调），只在之后的 tick 按退避重试 StopSpawn，
	// 成功后改成停止状态；状态机见 runtime_spawn_stop.go（停止入口统一，维护者 2026-10-07）。
	SpawnStopPending SpawnStatus = "stop_pending"
)

type SpawnScope string

const (
	SpawnScopePhase  SpawnScope = "phase"
	SpawnScopeCast   SpawnScope = "cast"
	SpawnScopeEntity SpawnScope = "entity"
)

type StopCause string

const (
	StopCauseEnd     StopCause = "end"
	StopCauseCancel  StopCause = "cancel"
	StopCauseFailure StopCause = "failure"
)

type MotionSpawnStage uint8

const (
	MotionStageOutbound MotionSpawnStage = iota + 1
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
	Stage              MotionSpawnStage
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

type SpawnNumericState struct {
	Initialized bool
	Properties  []numericPropertyState
}

type SpawnInstance struct {
	ID                SpawnID
	CastID            CastID
	TemplateIndex     SpawnTemplateIndex
	UnitTemplate      UnitTemplateHandle
	Status            SpawnStatus
	StartTick         Tick
	NextTick          Tick
	EndTick           Tick
	Scope             SpawnScope
	HostState         SpawnHostState
	Motion            MotionState
	Numeric           SpawnNumericState
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

	// 以下三项只在 Status == SpawnStopPending 时有意义（runtime_spawn_stop.go）：
	// stopRetryAttempts 是进入待停止之后失败的重试次数，stopRetryTick 是下一次重试的 tick，
	// stopRetryExhausted 表示已到 SpawnStopRetryLimit、已告警、不再自动重试（记录保留）。
	stopRetryAttempts  int
	stopRetryTick      Tick
	stopRetryExhausted bool
}

// liveOnHost 报告衍生物在宿主侧是否还在运行：运行中，或停止失败、等待 Runtime 重试停止。
// 两种状态都钉住所属 cast、占用 owned 衍生物容量，Shutdown / RemoveProgram 都要停它。
func (spawn *SpawnInstance) liveOnHost() bool {
	return spawn != nil && (spawn.Status == SpawnRunning || spawn.Status == SpawnStopPending)
}

type SpawnSignalKind string

const (
	SpawnSignalHit        SpawnSignalKind = "hit"
	SpawnSignalCollision  SpawnSignalKind = "collision"
	SpawnSignalTargetLost SpawnSignalKind = "target_lost"
	SpawnSignalTransition SpawnSignalKind = "transition"
	SpawnSignalLeave      SpawnSignalKind = "leave"
	SpawnSignalEnter      SpawnSignalKind = "enter"
	SpawnSignalTick       SpawnSignalKind = "tick"
	SpawnSignalEnd        SpawnSignalKind = "end"
	SpawnSignalCancel     SpawnSignalKind = "cancel"
)

type SpawnSignal struct {
	Kind            SpawnSignalKind
	Target          EntityID
	Distance        int64
	ContactOrdinal  uint64
	MembershipTicks int64
	EnterCount      int64
}

func normalizeSpawnSignals(signals []SpawnSignal) []SpawnSignal {
	result := append([]SpawnSignal(nil), signals...)
	sort.SliceStable(result, func(left, right int) bool {
		leftRank, rightRank := spawnSignalRank(result[left].Kind), spawnSignalRank(result[right].Kind)
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
			return spawnSignalRankWithinContact(result[left].Kind) < spawnSignalRankWithinContact(result[right].Kind)
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return result[left].Target < result[right].Target
	})
	return result
}

func spawnSignalRank(kind SpawnSignalKind) int {
	switch kind {
	case SpawnSignalHit, SpawnSignalCollision:
		return 0
	case SpawnSignalTargetLost:
		return 1
	case SpawnSignalTransition:
		return 2
	case SpawnSignalLeave:
		return 3
	case SpawnSignalEnter:
		return 4
	case SpawnSignalTick:
		return 5
	case SpawnSignalEnd:
		return 6
	case SpawnSignalCancel:
		return 7
	default:
		return 8
	}
}

func spawnSignalRankWithinContact(kind SpawnSignalKind) int {
	if kind == SpawnSignalHit {
		return 0
	}
	return 1
}

func spawnStatusForStop(cause StopCause) SpawnStatus {
	switch cause {
	case StopCauseEnd:
		return SpawnEnded
	case StopCauseFailure:
		return SpawnFailed
	default:
		return SpawnCancelled
	}
}

// stopSpawn 让宿主停掉衍生物，只由 terminateSpawn 调用（停止入口一律经 requestSpawnStop）。宿主出错时衍生物
// 状态不变（运行中或待停止），由 requestSpawnStop 转入 / 留在待停止。
func (runtime *Runtime) stopSpawn(cast *castInstance, spawn *SpawnInstance, cause StopCause) error {
	if !spawn.liveOnHost() {
		return nil
	}
	stopAreaMembership(spawn, false)
	detachErr := runtime.detachMotionCarry(cast, spawn)
	requiredRevision := runtime.host.CurrentRevision()
	if cast != nil {
		requiredRevision = cast.visibleRevision
	}
	receipt, err := runtime.host.StopSpawn(SpawnStopCommand{Meta: SpawnCommandMeta{
		RequiredRevision: requiredRevision,
		SpawnID:          spawn.ID,
	}}, spawn.HostState)
	if err != nil {
		return errors.Join(detachErr, err)
	}
	spawn.Status = spawnStatusForStop(cause)
	spawn.stopCause = cause
	spawn.stopRetryAttempts, spawn.stopRetryTick, spawn.stopRetryExhausted = 0, 0, false
	spawn.HostState.Active = false
	runtime.emitSpawnPresentation(cast, spawn, PresentationSpawnStop, "", cause, receipt.Revision)
	if spawn.Scope == SpawnScopeEntity {
		spawn.Program = nil
		spawn.inputs = nil
		spawn.memory = nil
		spawn.locals = nil
		spawn.snapshots = nil
		spawn.randomInvocations = nil
	}
	if cast != nil {
		cast.visibleRevision = maxRevision(cast.visibleRevision, receipt.Revision)
		detachErr = errors.Join(detachErr, runtime.drainHostEvents(cast))
	}
	return detachErr
}

func (runtime *Runtime) detachMotionCarry(cast *castInstance, spawn *SpawnInstance) error {
	if spawn == nil || !spawn.Motion.CarryAttached || spawn.Motion.CarryTarget == 0 {
		return nil
	}
	target := spawn.Motion.CarryTarget
	spawn.Motion.CarryTarget = 0
	spawn.Motion.CarryAttached = false
	requiredRevision := runtime.host.CurrentRevision()
	if cast != nil {
		requiredRevision = cast.visibleRevision
	}
	result, err := runtime.host.StepSpawn(SpawnStepCommand{
		Meta: SpawnCommandMeta{RequiredRevision: requiredRevision, SpawnID: spawn.ID},
		Motion: CarryMotionStep{
			Target:   target,
			Position: spawn.Motion.Position,
			Attached: false,
		},
	}, spawn.HostState)
	if err != nil {
		return err
	}
	spawn.HostState = result.State
	if cast != nil {
		cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
		spawn.visibleRevision = cast.visibleRevision
		return runtime.drainHostEvents(cast)
	} else {
		spawn.visibleRevision = maxRevision(spawn.visibleRevision, result.Commit.Revision)
	}
	return nil
}

// terminateSpawn 是 requestSpawnStop 的“停止中”一步，不直接调用。carry 解除排在任何回调之前：
// 挂载归衍生物所有，先清掉它，宿主报解除失败时重试或嵌套回调也无害。区域离开信号与回调只对 running 的衍生物
// 跑一次；待停止的衍生物再进来时只剩宿主 StopSpawn。
func (runtime *Runtime) terminateSpawn(cast *castInstance, spawn *SpawnInstance, cause StopCause, callbackEvent string) error {
	detachErr := runtime.detachMotionCarry(cast, spawn)
	var areaErr error
	if spawn != nil && spawn.Program != nil && int(spawn.TemplateIndex) < len(spawn.Program.spawnTemplates) {
		template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
		if template.area != nil {
			areaErr = runtime.dispatchOwnedSpawnSignals(spawn, stopAreaMembership(spawn, template.emitLeaveOnStop))
		}
	}
	var callbackErr error
	// Area callback completion suppresses only the remaining signals of that
	// Area spawn. Other spawns owned by the cast still need their normal
	// terminal cleanup callbacks during the same stop sweep.
	callbackLive := spawn != nil && spawn.Status == SpawnRunning && !spawn.areaCallbackFinishedCast
	if areaErr == nil && callbackEvent != "" && callbackLive {
		callbackErr = runtime.runOwnedSpawnCallback(spawn, callbackEvent)
	}
	stopErr := runtime.stopSpawn(cast, spawn, cause)
	return errors.Join(detachErr, areaErr, callbackErr, stopErr)
}

func (runtime *Runtime) stopSpawns(cast *castInstance, includeCastScope bool) error {
	return runtime.stopScopedSpawns(cast, includeCastScope, includeCastScope)
}

func (runtime *Runtime) stopFinishingSpawns(cast *castInstance) error {
	return runtime.stopScopedSpawns(cast, true, false)
}

func (runtime *Runtime) stopScopedSpawns(cast *castInstance, includeCastScope, includeEntityScope bool) error {
	spawnIDs := make([]SpawnID, 0, len(runtime.spawns))
	for spawnID, spawn := range runtime.spawns {
		if spawn.CastID != cast.id || spawn.Status != SpawnRunning {
			continue
		}
		if spawn.Scope == SpawnScopePhase || includeCastScope && spawn.Scope == SpawnScopeCast || includeEntityScope && spawn.Scope == SpawnScopeEntity && !spawn.handedOff {
			spawnIDs = append(spawnIDs, spawnID)
		}
	}
	sort.Slice(spawnIDs, func(left, right int) bool { return spawnIDs[left] < spawnIDs[right] })
	var firstErr error
	for _, spawnID := range spawnIDs {
		spawn := runtime.spawns[spawnID]
		callbackEvent := ""
		if spawn.Scope == SpawnScopeEntity {
			callbackEvent = "cancel"
		}
		if err := runtime.requestSpawnStop(cast, spawn, StopCauseCancel, callbackEvent); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
