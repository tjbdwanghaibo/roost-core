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
	MaxOwnedProcesses                  int
	MaxOwnedProcessesPerOwner          int
	MaxOwnedProcessesPerProgram        int
	MaxOwnedProcessesPerTemplate       int
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
	// referenced casts are never evicted.
	CompletedCastLimit int
	// RootEventLimit bounds once-per-root accounting after inactive roots have
	// been reclaimed.
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
	detachedProcess    *ProcessInstance
	detachedEvent      EventContext
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
	processes               map[ProcessID]*ProcessInstance
	ownedProcesses          map[ProcessID]*ProcessInstance
	nextProcessID           ProcessID
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
}

func NewRuntime(host Host, options RuntimeOptions) *Runtime {
	runtime := newRuntimeCore(host, options)
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
	if options.MaxOwnedProcesses <= 0 {
		options.MaxOwnedProcesses = 128
	}
	if options.MaxOwnedProcessesPerOwner <= 0 {
		options.MaxOwnedProcessesPerOwner = options.MaxOwnedProcesses
	}
	if options.MaxOwnedProcessesPerProgram <= 0 {
		options.MaxOwnedProcessesPerProgram = options.MaxOwnedProcesses
	}
	if options.MaxOwnedProcessesPerTemplate <= 0 {
		options.MaxOwnedProcessesPerTemplate = options.MaxOwnedProcesses
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
	runtime := &Runtime{
		host: host, options: options,
		casts: make(map[CastID]*castInstance), scheduler: newScheduler(),
		frames: make(map[FrameID][]RuntimeValue), processes: make(map[ProcessID]*ProcessInstance), ownedProcesses: make(map[ProcessID]*ProcessInstance),
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
		cast.eventContext = deriveEvent(*parentEvent, EventID((uint64(tentativeID)<<32)|1))
		cast.eventContext.ProcDepth = parentEvent.ProcDepth + 1
	}
	cast.eventContext.Source, cast.eventContext.Owner, cast.eventContext.Target, cast.eventContext.SkillID, cast.eventContext.CastID = input.Caster, input.Caster, input.Target, program.id, tentativeID
	for index, slot := range program.memory {
		value, evalErr := runtime.evalValue(cast, slot.defaultValue)
		if evalErr != nil {
			return 0, evalErr
		}
		cast.memory[index] = value
	}
	if err := runtime.captureSnapshots(cast, snapshotCastStart); err != nil {
		return 0, err
	}
	runtime.nextCastID = tentativeID
	runtime.casts[cast.id] = cast
	runtime.activeCastCount++
	runtime.recordTrace(TraceEvent{Kind: TraceCastActivated, Tick: runtime.currentTick, CastID: cast.id})
	runtime.markAbilityCastStarted(cast)
	if err := runtime.prepareCast(cast); err != nil {
		cast.status, cast.failure = CastFailed, err.Error()
		_ = runtime.stopProcesses(cast, true)
		runtime.markAbilityCastFinished(cast)
		if !cast.committed {
			delete(runtime.casts, cast.id)
			runtime.nextCastID--
			return 0, err
		}
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
