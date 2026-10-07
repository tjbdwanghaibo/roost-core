package skill

import (
	"errors"
	"sync"
)

var (
	ErrAuthorityMismatch        = errors.New("skill: program and host authority mismatch")
	ErrProgramSemanticsMismatch = errors.New("skill: compiler semantics revision is unsupported")
	ErrCastInputInvalid         = errors.New("skill: cast input does not match program input layout")
	ErrProgramInvariant         = errors.New("skill: immutable program invariant failed")
	ErrAsyncFlowNotScheduled    = errors.New("skill: asynchronous flow is not scheduled")
	ErrReverseAdvance           = errors.New("skill: cannot advance runtime backwards")
	ErrCooldownActive           = errors.New("skill: skill is on cooldown")
	ErrGlobalCooldownActive     = errors.New("skill: caster is on global cooldown")
	ErrCasterBusy               = errors.New("skill: caster already has an active cast window")
	ErrCastInputRejected        = errors.New("skill: cast does not accept gameplay input in its current state")
)

// supportedCompilerSemanticsRevision gates programs and checkpoints to one
// compiler generation. Bumped to 2 for the v1.4/v1.5 semantics additions
// (cast exclusivity, global cooldown, window-tick expressions): they change
// every gameplay digest, so older programs, checkpoints, and composition
// contracts must be recompiled/rebuilt rather than silently failing digest
// resolution. See docs/skill-casting-and-combat.md for the migration note.
const supportedCompilerSemanticsRevision = "skillv2-compiler-2"

type RuntimeOptions struct {
	MatchSeed                          [32]byte
	SupportedCompilerSemanticsRevision string
	PassiveRouter                      PassiveRouter
	MaxPassiveActivationsPerTick       int
	MaxOwnedSpawns                     int
	MaxOwnedSpawnsPerOwner             int
	MaxOwnedSpawnsPerProgram           int
	MaxOwnedSpawnsPerTemplate          int
	TraceSink                          TraceSink
	TraceLimits                        TraceLimits
	// PresentationLimit bounds renderer-facing events retained for polling.
	PresentationLimit int
	// StateEventLimit bounds authoritative change events retained for sync.
	StateEventLimit int
	// StateMutationLimit bounds canonical, client-applicable state mutations.
	StateMutationLimit int
	// RuntimeEventLimit bounds diagnostic events retained by RuntimeEvents.
	RuntimeEventLimit int
	// CompletedCastLimit bounds inspectable terminal casts. Active or still
	// referenced casts (pending tasks, an active policy, a running or
	// stop_pending spawn) are never evicted; an evicted cast takes its stopped spawn records
	// with it.
	CompletedCastLimit int
	// RootEventLimit bounds once-per-root accounting after inactive roots have
	// been reclaimed. It must exceed MaxActiveCasts + MaxOwnedSpawns +
	// MaxStopPendingSpawns (the most roots casts and spawns can reference at
	// once); NewRuntime panics and RestoreRuntime fails otherwise. Default 8192.
	RootEventLimit int
	// CheckpointMaxBytes and CheckpointMaxRecords bound recovery input before
	// it can allocate unbounded object graphs.
	CheckpointMaxBytes   int
	CheckpointMaxRecords int
	MaxActiveCasts       int
	MaxAbilities         int
	MaxProcLedgerEntries int
	// CastEventLimit bounds per-cast diagnostic history returned by InspectCast.
	CastEventLimit int
	// SpawnStopRetryBackoff is the delay, in ticks, before the Runtime
	// retries a Host.StopSpawn that failed while a cast was failing; the
	// delay doubles after every failed retry, up to 64 times this value.
	// Default 4.
	SpawnStopRetryBackoff Tick
	// SpawnStopRetryLimit bounds the failed retries of one such stop. At the
	// limit the Runtime stops retrying, counts
	// skill.spawn.stop_retry_exhausted.total, logs a warning and keeps the
	// stop_pending record. Default 10 (about 64 seconds at 20 ticks per second
	// with the default backoff).
	SpawnStopRetryLimit int
	// MaxStopPendingSpawns bounds stop_pending records. Past the bound the
	// oldest exhausted record (else the oldest still retrying) is abandoned:
	// it moves to status abandoned (no more retries, no longer pins its cast),
	// counted as skill.spawn.abandoned.total and logged as an error; the
	// record is kept, the host may still run that spawn. Default 256.
	MaxStopPendingSpawns int
	// MaxAbandonedSpawns bounds abandoned records. Only at the end of Advance
	// the oldest ones past the bound are removed, counted as
	// skill.spawn.abandoned_pruned.total and logged; between two Advance calls
	// the count may exceed it. Default 1024.
	MaxAbandonedSpawns int
}

type CastInput struct {
	Caster        EntityID
	Target        EntityID
	Position      *Position
	Direction     *Direction
	StartPosition *Position
	EndPosition   *Position
	Path          []Position
}

type InputPayload struct {
	Target        EntityID
	Position      *Position
	Direction     *Direction
	StartPosition *Position
	EndPosition   *Position
	Path          []Position
}

type CastStatus string

const (
	CastRunning   CastStatus = "running"
	CastSuspended CastStatus = "suspended"
	CastFinished  CastStatus = "finished"
	CastFailed    CastStatus = "failed"
)

type CastWindowStage string

const (
	CastWindowPreparing  CastWindowStage = "preparing"
	CastWindowCommitted  CastWindowStage = "committed"
	CastWindowExecuting  CastWindowStage = "executing"
	CastWindowRecovering CastWindowStage = "recovering"
	CastWindowComplete   CastWindowStage = "complete"
	CastWindowCancelled  CastWindowStage = "cancelled"
)

type CastSnapshot struct {
	ID              CastID
	Caster          EntityID
	Status          CastStatus
	CurrentPhase    PhaseIndex
	VisibleRevision WorldRevision
	Failure         string
	Events          []RuntimeEvent
	EventsDropped   uint64
	WindowStage     CastWindowStage
	Committed       bool
	ElapsedTicks    Tick
	PulseIndex      int64
	ReleaseReason   string
	Stock           int64
	MaxStock        int64
}

type castInstance struct {
	id                 CastID
	program            *Program
	caster             EntityID
	primaryTarget      EntityID
	inputs             []RuntimeValue
	memory             []RuntimeValue
	locals             []RuntimeValue
	snapshots          map[int]RuntimeValue
	status             CastStatus
	currentPhase       PhaseIndex
	visibleRevision    WorldRevision
	failure            string
	events             []RuntimeEvent
	eventsDropped      uint64
	randomKey          [32]byte
	randomInvocations  map[RandomSiteIndex]uint64
	eventContext       EventContext
	phaseToken         uint64
	pendingTasks       int
	logicalFinished    bool
	areaCallbackFinish bool
	windowStage        CastWindowStage
	startTick          Tick
	committed          bool
	costsPaid          bool
	cooldownStarted    bool
	pulseIndex         int64
	releaseReason      string
	stock              int64
	maxStock           int64
	windowStartTick    Tick
	pendingRootEvent   string
	policyActive       bool
	cooldownOwner      EntityID
	ability            AbilityHandle
	abilityFinished    bool
	detachedSpawn      *SpawnInstance
	detachedEvent      EventContext
	// evalContext 是当前求值所在的上下文（eval_contexts.go）。零值是施法流程；采样、memory
	// 默认值、衍生物字段、状态默认值在求值期间临时切换（switchEvalContext），不进 checkpoint。
	evalContext evalContext
}

type cooldownKey struct {
	Caster EntityID
	Skill  string
}

type skillStateKey struct {
	Caster EntityID
	Skill  string
}

type skillState struct {
	stock              int64
	maxStock           int64
	rechargeTicks      Tick
	rechargeDue        Tick
	rechargeScheduled  bool
	rechargeGeneration uint64
}

type Runtime struct {
	mutex                   sync.Mutex
	host                    Host
	options                 RuntimeOptions
	casts                   map[CastID]*castInstance
	activeCastCount         int
	nextCastID              CastID
	eventCursor             EventCursor
	currentTick             Tick
	scheduler               *scheduler
	nextTaskSequence        uint64
	frames                  map[FrameID][]RuntimeValue
	nextFrameID             FrameID
	spawns                  spawnTable // 按分区存放衍生物记录，分区由记录字段决定（spawn_table.go）
	nextSpawnID             SpawnID
	spawnEventSequence      uint64 // 衍生物回调事件 ID 的计数（nextSpawnEventID），随 checkpoint 保存
	cooldowns               map[cooldownKey]Tick
	skillStates             map[skillStateKey]*skillState
	activePolicies          map[skillStateKey]CastID
	nextPassiveActivationID PassiveActivationID
	procLedger              map[procLedgerKey]struct{}
	rootEventCounts         map[EventID]int
	passiveCountTick        Tick
	passiveCount            int
	runtimeEvents           []RuntimeEvent
	runtimeEventDropped     uint64
	completedCastOrder      []CastID
	rootEventOrder          []EventID
	abilities               map[abilityKey]*abilityState
	abilityByProgram        map[skillStateKey]AbilityHandle
	nextAbilityHandle       AbilityHandle
	nextAbilityOverlay      uint64
	trace                   []TraceEvent
	traceSequence           uint64
	traceTruncated          bool
	traceFlushed            int
	presentationEvents      []PresentationEvent
	presentationSequence    uint64
	stateEvents             []StateEvent
	stateEventSequence      uint64
	stateEventDropped       uint64
	stateMutations          []StateMutation
	stateMutationSequence   uint64
	stateMutationDropped    uint64
	stateMutationBaseline   RuntimeStateSnapshot
	stateMutationReady      bool
	stateMutationDirty      bool
	stateMutationAllDirty   bool
	dirtyCooldowns          map[cooldownKey]struct{}
	dirtyResources          map[skillStateKey]struct{}
	dirtyAbilities          map[abilityKey]struct{}
	dirtyPolicies           map[skillStateKey]struct{}
	// hostAdmitted：能力需求已核对过、在 Host 声明的表里的 Program（B3 ③，runtime_host_capability.go）。
	hostAdmitted map[*Program]struct{}
}

// NewRuntime 构造一个从宿主当前事件前沿开始的 Runtime。上限之间的关系不成立时（RootEventLimit 不大于
// MaxActiveCasts + MaxOwnedSpawns + MaxStopPendingSpawns，见 validateRootEventLimit）以 ErrRuntimeLimitsInvalid panic：
// 这是装配期的配置错误，错误信息点出相关选项。
func NewRuntime(host Host, options RuntimeOptions) *Runtime {
	runtime := newRuntimeCore(host, options)
	if err := validateRootEventLimit(runtime.options); err != nil {
		panic(err)
	}
	// Fresh runtimes start at the host's current event frontier: everything
	// already in the queue predates this runtime and is never replayed, so it
	// may be compacted away. RestoreRuntime must NOT take this path — a
	// restored runtime resumes from the checkpoint's cursor and needs every
	// event after it preserved for replay.
	if host != nil {
		for _, event := range host.Events(0) {
			if event.Cursor > runtime.eventCursor {
				runtime.eventCursor = event.Cursor
			}
		}
		if compactor, ok := host.(HostEventCompactor); ok && runtime.eventCursor != 0 {
			compactor.CompactEventsThrough(runtime.eventCursor)
		}
	}
	return runtime
}

// newRuntimeCore builds a runtime without touching the host's event queue.
func newRuntimeCore(host Host, options RuntimeOptions) *Runtime {
	if options.SupportedCompilerSemanticsRevision == "" {
		options.SupportedCompilerSemanticsRevision = supportedCompilerSemanticsRevision
	}
	if options.MaxPassiveActivationsPerTick <= 0 {
		options.MaxPassiveActivationsPerTick = 256
	}
	if options.MaxOwnedSpawns <= 0 {
		options.MaxOwnedSpawns = 128
	}
	if options.MaxOwnedSpawnsPerOwner <= 0 {
		options.MaxOwnedSpawnsPerOwner = options.MaxOwnedSpawns
	}
	if options.MaxOwnedSpawnsPerProgram <= 0 {
		options.MaxOwnedSpawnsPerProgram = options.MaxOwnedSpawns
	}
	if options.MaxOwnedSpawnsPerTemplate <= 0 {
		options.MaxOwnedSpawnsPerTemplate = options.MaxOwnedSpawns
	}
	if options.PresentationLimit <= 0 {
		options.PresentationLimit = 1024
	}
	if options.StateEventLimit <= 0 {
		options.StateEventLimit = 2048
	}
	if options.StateMutationLimit <= 0 {
		options.StateMutationLimit = 2048
	}
	if options.RuntimeEventLimit <= 0 {
		options.RuntimeEventLimit = 4096
	}
	if options.CompletedCastLimit <= 0 {
		options.CompletedCastLimit = 2048
	}
	if options.RootEventLimit <= 0 {
		options.RootEventLimit = 8192
	}
	if options.CheckpointMaxBytes <= 0 {
		options.CheckpointMaxBytes = 16 << 20
	} else if options.CheckpointMaxBytes > RuntimeCheckpointMaxBytes {
		options.CheckpointMaxBytes = RuntimeCheckpointMaxBytes
	}
	if options.CheckpointMaxRecords <= 0 {
		options.CheckpointMaxRecords = 200000
	} else if options.CheckpointMaxRecords > RuntimeCheckpointMaxRecords {
		options.CheckpointMaxRecords = RuntimeCheckpointMaxRecords
	}
	if options.MaxActiveCasts <= 0 {
		options.MaxActiveCasts = 4096
	}
	if options.MaxAbilities <= 0 {
		options.MaxAbilities = 10000
	}
	if options.MaxProcLedgerEntries <= 0 {
		options.MaxProcLedgerEntries = 262144
	}
	if options.CastEventLimit <= 0 {
		options.CastEventLimit = 256
	}
	if options.SpawnStopRetryBackoff <= 0 {
		options.SpawnStopRetryBackoff = 4
	}
	if options.SpawnStopRetryLimit <= 0 {
		options.SpawnStopRetryLimit = 10
	}
	if options.MaxStopPendingSpawns <= 0 {
		options.MaxStopPendingSpawns = 256
	}
	if options.MaxAbandonedSpawns <= 0 {
		options.MaxAbandonedSpawns = 1024
	}
	runtime := &Runtime{
		host: host, options: options,
		casts: make(map[CastID]*castInstance), scheduler: newScheduler(),
		frames: make(map[FrameID][]RuntimeValue), spawns: newSpawnTable(),
		cooldowns:   make(map[cooldownKey]Tick),
		skillStates: make(map[skillStateKey]*skillState), activePolicies: make(map[skillStateKey]CastID),
		procLedger: make(map[procLedgerKey]struct{}), rootEventCounts: make(map[EventID]int),
		abilities: make(map[abilityKey]*abilityState), abilityByProgram: make(map[skillStateKey]AbilityHandle),
		dirtyCooldowns: make(map[cooldownKey]struct{}), dirtyResources: make(map[skillStateKey]struct{}),
		dirtyAbilities: make(map[abilityKey]struct{}), dirtyPolicies: make(map[skillStateKey]struct{}),
	}
	runtime.stateMutationBaseline = runtime.stateSnapshotLocked()
	runtime.stateMutationReady = true
	return runtime
}

func (runtime *Runtime) Activate(program *Program, input CastInput) (CastID, error) {
	return runtime.Start(program, input)
}

func (runtime *Runtime) Start(program *Program, input CastInput) (CastID, error) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if program == nil || runtime.host == nil {
		return 0, ErrProgramInvariant
	}
	if runtime.activeCastCount >= runtime.options.MaxActiveCasts {
		return 0, ErrRuntimeCapacityExceeded
	}
	if program.compilerSemanticsRevision != runtime.options.SupportedCompilerSemanticsRevision {
		return 0, ErrProgramSemanticsMismatch
	}
	if !authorityMatches(program.authority, runtime.host.AuthorityIdentity()) {
		return 0, ErrAuthorityMismatch
	}
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	return runtime.startLocked(program, input, nil)
}

func (runtime *Runtime) startLocked(program *Program, input CastInput, parentEvent *EventContext) (CastID, error) {
	if program == nil || runtime.host == nil {
		return 0, ErrProgramInvariant
	}
	if runtime.activeCastCount >= runtime.options.MaxActiveCasts {
		return 0, ErrRuntimeCapacityExceeded
	}
	if program.compilerSemanticsRevision != runtime.options.SupportedCompilerSemanticsRevision {
		return 0, ErrProgramSemanticsMismatch
	}
	if !authorityMatches(program.authority, runtime.host.AuthorityIdentity()) {
		return 0, ErrAuthorityMismatch
	}
	if err := runtime.admitHostCapabilitiesLocked(program); err != nil {
		return 0, err
	}
	inputs, err := freezeCastInput(program, input, runtime.host)
	if err != nil {
		return 0, err
	}
	ability, abilityErr := runtime.ensureAbilityLocked(input.Caster, program)
	if abilityErr != nil {
		return 0, abilityErr
	}
	policyKey := skillStateKey{Caster: input.Caster, Skill: program.id}
	if program.cast.mode == castModeToggle {
		if activeID := runtime.activePolicies[policyKey]; activeID != 0 {
			active := runtime.casts[activeID]
			if active != nil && active.policyActive {
				return activeID, runtime.releaseCast(active, "toggle_off")
			}
			delete(runtime.activePolicies, policyKey)
			runtime.touchActivePolicyLocked(policyKey)
		}
	}
	if parentEvent == nil && program.activationKind == "active" {
		// Root activations respect caster exclusivity and the global
		// cooldown; proc- and passive-triggered casts bypass both.
		if !program.cast.concurrent && runtime.casterWindowBusyLocked(input.Caster) {
			return 0, ErrCasterBusy
		}
		if runtime.cooldowns[cooldownKey{Caster: input.Caster, Skill: globalCooldownProgramID}] > runtime.currentTick {
			return 0, ErrGlobalCooldownActive
		}
	}
	tentativeID := runtime.nextCastID + 1
	cast := &castInstance{
		id: tentativeID, program: program, caster: input.Caster, primaryTarget: input.Target,
		inputs: inputs, memory: make([]RuntimeValue, len(program.memory)), locals: make([]RuntimeValue, len(program.locals)),
		snapshots: make(map[int]RuntimeValue), status: CastRunning, currentPhase: program.initialPhase,
		visibleRevision: runtime.host.CurrentRevision(), randomInvocations: make(map[RandomSiteIndex]uint64), phaseToken: 1,
	}
	cast.ability = ability.handle
	cast.cooldownOwner = input.Caster
	if parentEvent != nil && program.cooldownScope == "target" && parentEvent.Target != 0 {
		cast.cooldownOwner = parentEvent.Target
	}
	cast.randomKey = deriveCastRandomKey(runtime.options.MatchSeed, program.identity.gameplayDigest, input.Caster, uint64(tentativeID))
	if parentEvent == nil {
		cast.eventContext = newRootEvent(EventID(tentativeID))
	} else {
		// proc 施放自己的事件 ID 低 32 位为 0：第 i 号效果的事件是 castID<<32 | (i+1)（effectEventContext），之前这里用
		// castID<<32 | 1，与第 0 号效果撞号，第 0 号效果事件的父事件是它自己（RR-20261006-54）。
		cast.eventContext = deriveEvent(*parentEvent, EventID(uint64(tentativeID)<<32))
		cast.eventContext.ProcDepth = parentEvent.ProcDepth + 1
	}
	cast.eventContext.Source, cast.eventContext.Owner, cast.eventContext.Target, cast.eventContext.SkillID, cast.eventContext.CastID = input.Caster, input.Caster, input.Target, program.id, tentativeID
	// memory 默认值在 memory_default 上下文里求值：读不到别的 memory（RR-20261005-NC-280）。
	cast.switchEvalContext(evalMemoryDefault)
	for index, slot := range program.memory {
		value, evalErr := runtime.evalValue(cast, slot.defaultValue)
		if evalErr != nil {
			return 0, evalErr
		}
		cast.memory[index] = value
	}
	cast.switchEvalContext(evalCastFlow)
	if err := runtime.captureSnapshots(cast, snapshotCastStart); err != nil {
		return 0, err
	}
	runtime.nextCastID = tentativeID
	runtime.casts[cast.id] = cast
	runtime.activeCastCount++
	runtime.recordTrace(TraceEvent{Kind: TraceCastActivated, Tick: runtime.currentTick, CastID: cast.id})
	runtime.markAbilityCastStarted(cast)
	if err := runtime.prepareCast(cast); err != nil {
		runtime.failCastLocked(cast, err)
		if !cast.committed && !runtime.castHasRunningSpawnLocked(cast.id) && !runtime.castHasAbandonedSpawnLocked(cast.id) {
			// 未提交的失败启动对调用方等于“没有施法”：删掉 cast 并把 ID 还给下一个 cast。
			// failCastLocked 已撤掉它名下的全部排程任务、停掉它起的衍生物；已停衍生物的记录随 cast 一起删，
			// 复用 ID 才安全（NC-110；衍生物记录见 RR-20261006-21）。
			runtime.forgetCastSpawnsLocked(cast.id)
			delete(runtime.casts, cast.id)
			runtime.forgetCompletedCastLocked(cast.id)
			runtime.nextCastID--
			return 0, err
		}
		// 已提交，或有衍生物停不下来（宿主 StopSpawn 失败、记录已标成待停止）：cast 留作 failed 终态、不还 ID，
		// 待停止的衍生物记录继续挂在一个存在的 cast 名下，由之后的 tick 重试停止，停掉后按 RR-23 回收。
		// 名下有已放弃的记录（它比 cast 活得久）同样不还 ID，免得这条记录挂到下一个 cast 名下。
		return cast.id, err
	}
	runtime.recordTrace(TraceEvent{Kind: TraceCastPrepared, Tick: runtime.currentTick, CastID: cast.id})
	return cast.id, nil
}

func (runtime *Runtime) CastCount() int {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return len(runtime.casts)
}

func (runtime *Runtime) ActiveCastCount() int {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return runtime.activeCastCount
}

func (runtime *Runtime) InspectCast(id CastID) (CastSnapshot, bool) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	cast, ok := runtime.casts[id]
	if !ok {
		return CastSnapshot{}, false
	}
	return CastSnapshot{
		ID: cast.id, Caster: cast.caster, Status: cast.status, CurrentPhase: cast.currentPhase,
		VisibleRevision: cast.visibleRevision, Failure: cast.failure, Events: cloneRuntimeEvents(cast.events),
		EventsDropped: cast.eventsDropped,
		WindowStage:   cast.windowStage, Committed: cast.committed, ElapsedTicks: runtime.currentTick - cast.startTick,
		PulseIndex: cast.pulseIndex, ReleaseReason: cast.releaseReason, Stock: cast.stock, MaxStock: cast.maxStock,
	}, true
}
