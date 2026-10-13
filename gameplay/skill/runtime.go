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

// supportedCompilerSemanticsRevision 把 Program 与 checkpoint 限定在同一代编译器语义上。语义变化会改变
// 每个 gameplay digest，旧 Program、checkpoint 与组合契约必须重新编译 / 重建，而不是在解析 digest 时
// 静默失败；升级说明见 docs/skill/skill-casting-and-combat.md。
const supportedCompilerSemanticsRevision = "skillv2-compiler-2"

// RuntimeOptions 是 Runtime 的随机种子、被动路由与各项容量上限。零值选项取默认值（newRuntimeCore）；
// 上限之间的关系由 validateRootEventLimit 校验。
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
	// PresentationLimit 是保留待轮询的表现事件条数上限。
	PresentationLimit int
	// StateEventLimit 是保留待同步的权威状态变化事件条数上限。
	StateEventLimit int
	// StateMutationLimit 是可直接应用到客户端的规范化状态变更条数上限。
	StateMutationLimit int
	// RuntimeEventLimit 是 RuntimeEvents 保留的诊断事件条数上限。
	RuntimeEventLimit int
	// CompletedCastLimit 是可供查看的终态施法条数上限。仍在活动或仍被引用的施法（有排程任务、策略在生效、
	// 名下有 running 或 stop_pending 衍生物）不会被淘汰；淘汰一个施法时连同它名下已停衍生物的记录一起删。
	CompletedCastLimit int
	// RootEventLimit 是回收不活跃的根之后，按根计数（每根一次）的记账条数上限。它必须大于
	// MaxActiveCasts + MaxOwnedSpawns + MaxStopPendingSpawns + MaxQueuedTasks（施法、衍生物与排队任务
	// 同时能引用的根数上界），否则 NewRuntime panic、RestoreRuntime 返回错误。默认 8192。
	RootEventLimit int
	// MaxQueuedTasks 是钉住根事件的排队任务数上限：尚未执行的被动激活（PassiveRouter 路由或
	// ActivatePassive 排入）与 QueueExternalEvent 排入的外部事件。到上限时 QueueExternalEvent 与
	// ActivatePassive 返回 ErrQueuedTasksFull，路由来的被动候选被拒（passive_suppressed，Result 为
	// queue_full，计 skill.passive.dispatch_rejected.total{reason="queue_full"}）。默认 2048。
	MaxQueuedTasks int
	// MaxAbilityOverlays 限制整个 Runtime 的临时禁用覆盖数（含到期任务）。默认 10000；
	// 设置入口超限返回 ErrAbilityOverlaysFull，覆盖到期后释放额度，不占根事件额度。
	MaxAbilityOverlays int
	// CheckpointMaxBytes 与 CheckpointMaxRecords 在恢复输入分配对象图之前限制它的大小。
	CheckpointMaxBytes   int
	CheckpointMaxRecords int
	MaxActiveCasts       int
	MaxAbilities         int
	MaxProcLedgerEntries int
	// CastEventLimit 是 InspectCast 返回的单个施法诊断历史条数上限。
	CastEventLimit int
	// SpawnStopRetryBackoff 是 Host.StopSpawn 失败后 Runtime 重试前等待的 tick 数；每失败一次翻倍，
	// 最多到这个值的 64 倍。默认 4。
	SpawnStopRetryBackoff Tick
	// SpawnStopRetryLimit 是同一次停止的失败重试次数上限。到上限后不再重试，计
	// skill.spawn.stop_retry_exhausted.total、写一条 Warn，保留 stop_pending 记录。默认 10
	// （默认退避下、每秒 20 tick 约 64 秒）。
	SpawnStopRetryLimit int
	// MaxStopPendingSpawns 是 stop_pending 记录数上限。超过时放弃最早一条已重试耗尽的记录（没有则放弃
	// 最早一条仍在重试的）：记录转为 abandoned（不再重试、不再钉住所属施法），计
	// skill.spawn.abandoned.total、写一条 Error；记录保留，宿主侧那个衍生物可能仍在运行。默认 256。
	MaxStopPendingSpawns int
	// MaxAbandonedSpawns 是 abandoned 记录数上限。只在 Advance 末尾删除超出上限的最早几条，计
	// skill.spawn.abandoned_pruned.total 并记日志；两次 Advance 之间记录数可能暂时超过上限。默认 1024。
	MaxAbandonedSpawns int
}

// CastInput 是发起一次施法的输入：施法者与按 Program 输入布局冻结的目标、位置、方向与路径。
type CastInput struct {
	Caster        EntityID
	Target        EntityID
	Position      *Position
	Direction     *Direction
	StartPosition *Position
	EndPosition   *Position
	Path          []Position
}

// InputPayload 是施法进行中 UpdateInput 送入的输入载荷，字段与 CastInput 的输入部分一致。
type InputPayload struct {
	Target        EntityID
	Position      *Position
	Direction     *Direction
	StartPosition *Position
	EndPosition   *Position
	Path          []Position
}

// CastStatus 是施法的生命周期状态。
type CastStatus string

const (
	CastRunning   CastStatus = "running"
	CastSuspended CastStatus = "suspended"
	CastFinished  CastStatus = "finished"
	CastFailed    CastStatus = "failed"
)

// CastWindowStage 是施法窗口所处的阶段（准备、已提交、执行、恢复、完成、取消）。
type CastWindowStage string

const (
	CastWindowPreparing  CastWindowStage = "preparing"
	CastWindowCommitted  CastWindowStage = "committed"
	CastWindowExecuting  CastWindowStage = "executing"
	CastWindowRecovering CastWindowStage = "recovering"
	CastWindowComplete   CastWindowStage = "complete"
	CastWindowCancelled  CastWindowStage = "cancelled"
)

// CastSnapshot 是 InspectCast 返回的单个施法的只读快照。
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

// Runtime 推进一组技能施法、衍生物与被动路由。全部状态由 mutex 保护；公开方法各自加锁，
// 以 Locked 结尾的方法要求调用方已持锁。
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
	queuedTasks             int // 排程里的排队任务数（被动激活与外部事件，isQueuedTask），不超过 MaxQueuedTasks
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
	abilityOverlays         int // 活跃覆盖总数；到期扣减，恢复从记录重建。
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
	// hostAdmitted：能力需求已核对过、在 Host 声明的表里的 Program（runtime_host_capability.go）。
	hostAdmitted map[*Program]struct{}
}

// NewRuntime 构造一个从宿主当前事件前沿开始的 Runtime。上限之间的关系不成立时（RootEventLimit 不大于
// MaxActiveCasts + MaxOwnedSpawns + MaxStopPendingSpawns + MaxQueuedTasks，见 validateRootEventLimit）以
// ErrRuntimeLimitsInvalid panic：这是装配期的配置错误，错误信息点出全部相关选项与当前值。
func NewRuntime(host Host, options RuntimeOptions) *Runtime {
	runtime := newRuntimeCore(host, options)
	if err := validateRootEventLimit(runtime.options); err != nil {
		panic(err)
	}
	// 新 Runtime 从宿主当前的事件前沿开始：队列里已有的事件早于这个 Runtime、永不回放，可以压缩掉。
	// RestoreRuntime 不能走这里——恢复的 Runtime 从 checkpoint 的游标继续，之后的事件都要留着回放。
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

// newRuntimeCore 补齐选项默认值并构造 Runtime，不碰宿主的事件队列。
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
	if options.MaxAbilityOverlays <= 0 {
		options.MaxAbilityOverlays = 10000
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
	if options.MaxQueuedTasks <= 0 {
		options.MaxQueuedTasks = 2048
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

// Activate 是 Start 的别名。
func (runtime *Runtime) Activate(program *Program, input CastInput) (CastID, error) {
	return runtime.Start(program, input)
}

// Start 发起一次主动施法：核对语义修订、权威身份与 Host 能力，冻结输入、求 memory 默认值并完成准备阶段。
// 准备阶段失败且尚未提交时，施法被删除、ID 归还，返回 0 与错误；已提交（或名下有停不下来的衍生物）时
// 施法留作 failed 终态，返回它的 ID 与错误。
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

// startLocked 启动一次施法（主动施法与 proc 共用，调用方持有 Runtime 锁）：准入程序 → 冻结输入、取能力状态 →
// 切换型技能再次启动即释放 → 根施法的独占与公共冷却 → 建立施法实例并求 memory 默认值与施法开始快照 → 登记 →
// 准备。准备失败时按是否已提交决定撤销还是保留 failed 终态（见 abandonUnpreparedCastLocked）。
func (runtime *Runtime) startLocked(program *Program, input CastInput, parentEvent *EventContext) (CastID, error) {
	if err := runtime.admitCastProgramLocked(program); err != nil {
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
	if program.cast.mode == castModeToggle {
		if activeID, released, err := runtime.releaseActiveToggleLocked(skillStateKey{Caster: input.Caster, Skill: program.id}); released {
			return activeID, err
		}
	}
	if parentEvent == nil && program.activationKind == "active" {
		// 根施法遵守施法者独占与公共冷却；proc 与被动触发的施法两者都不受限。
		if err := runtime.rootCastBlockedLocked(program, input.Caster); err != nil {
			return 0, err
		}
	}
	cast := runtime.newCastInstance(program, input, inputs, ability, parentEvent)
	if err := runtime.initializeCastValues(cast); err != nil {
		return 0, err
	}
	runtime.registerCastLocked(cast)
	if err := runtime.prepareCast(cast); err != nil {
		return runtime.abandonUnpreparedCastLocked(cast, err)
	}
	runtime.recordTrace(TraceEvent{Kind: TraceCastPrepared, Tick: runtime.currentTick, CastID: cast.id})
	return cast.id, nil
}

// admitCastProgramLocked 核对程序能在本 Runtime 上启动：容量、编译语义修订、权威身份与 Host 能力。
func (runtime *Runtime) admitCastProgramLocked(program *Program) error {
	if program == nil || runtime.host == nil {
		return ErrProgramInvariant
	}
	if runtime.activeCastCount >= runtime.options.MaxActiveCasts {
		return ErrRuntimeCapacityExceeded
	}
	if program.compilerSemanticsRevision != runtime.options.SupportedCompilerSemanticsRevision {
		return ErrProgramSemanticsMismatch
	}
	if !authorityMatches(program.authority, runtime.host.AuthorityIdentity()) {
		return ErrAuthorityMismatch
	}
	return runtime.admitHostCapabilitiesLocked(program)
}

// releaseActiveToggleLocked 处理切换型技能的再次启动：施法者已有生效中的同技能施法时释放它（toggle_off），
// released 为 true，调用方返回该施法的 ID 与释放结果；登记的施法已不在生效时清掉登记，继续正常启动。
func (runtime *Runtime) releaseActiveToggleLocked(policyKey skillStateKey) (activeID CastID, released bool, err error) {
	activeID = runtime.activePolicies[policyKey]
	if activeID == 0 {
		return 0, false, nil
	}
	active := runtime.casts[activeID]
	if active != nil && active.policyActive {
		return activeID, true, runtime.releaseCast(active, "toggle_off")
	}
	delete(runtime.activePolicies, policyKey)
	runtime.touchActivePolicyLocked(policyKey)
	return 0, false, nil
}

// rootCastBlockedLocked 检查根施法的施法者独占窗口与公共冷却。
func (runtime *Runtime) rootCastBlockedLocked(program *Program, caster EntityID) error {
	if !program.cast.concurrent && runtime.casterWindowBusyLocked(caster) {
		return ErrCasterBusy
	}
	if runtime.cooldowns[cooldownKey{Caster: caster, Skill: globalCooldownProgramID}] > runtime.currentTick {
		return ErrGlobalCooldownActive
	}
	return nil
}

// newCastInstance 以下一个施法 ID（暂定，登记时才占用）建立施法实例：冻结的输入、memory 与局部变量槽、
// 冷却归属、随机键与事件上下文。
func (runtime *Runtime) newCastInstance(program *Program, input CastInput, inputs []RuntimeValue, ability *abilityState, parentEvent *EventContext) *castInstance {
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
		// proc 施放自己的事件 ID 低 32 位为 0：第 i 号效果的事件是 castID<<32 | (i+1)（effectEventContext），
		// 用 | 1 会与第 0 号效果撞号，让第 0 号效果事件的父事件成为它自己。
		cast.eventContext = deriveEvent(*parentEvent, EventID(uint64(tentativeID)<<32))
		cast.eventContext.ProcDepth = parentEvent.ProcDepth + 1
	}
	cast.eventContext.Source, cast.eventContext.Owner, cast.eventContext.Target, cast.eventContext.SkillID, cast.eventContext.CastID = input.Caster, input.Caster, input.Target, program.id, tentativeID
	return cast
}

// initializeCastValues 求 memory 默认值并拍施法开始快照。memory 默认值在 memory_default 上下文里求值：读不到别的 memory。
func (runtime *Runtime) initializeCastValues(cast *castInstance) error {
	cast.switchEvalContext(evalMemoryDefault)
	for index, slot := range cast.program.memory {
		value, evalErr := runtime.evalValue(cast, slot.defaultValue)
		if evalErr != nil {
			return evalErr
		}
		cast.memory[index] = value
	}
	cast.switchEvalContext(evalCastFlow)
	return runtime.captureSnapshots(cast, snapshotCastStart)
}

// registerCastLocked 占用暂定 ID 并登记施法：计入活动施法、记 trace、标记能力的施法开始。
func (runtime *Runtime) registerCastLocked(cast *castInstance) {
	runtime.nextCastID = cast.id
	runtime.casts[cast.id] = cast
	runtime.activeCastCount++
	runtime.recordTrace(TraceEvent{Kind: TraceCastActivated, Tick: runtime.currentTick, CastID: cast.id})
	runtime.markAbilityCastStarted(cast)
}

// abandonUnpreparedCastLocked 处理准备阶段失败的施法：先按失败终态收尾，再决定撤销还是保留。
func (runtime *Runtime) abandonUnpreparedCastLocked(cast *castInstance, err error) (CastID, error) {
	runtime.failCastLocked(cast, err)
	if !cast.committed && !runtime.castHasRunningSpawnLocked(cast.id) && !runtime.castHasAbandonedSpawnLocked(cast.id) {
		// 未提交的失败启动对调用方等于“没有施法”：删掉 cast 并把 ID 还给下一个 cast。
		// failCastLocked 已撤掉它名下的全部排程任务、停掉它起的衍生物；已停衍生物的记录随 cast 一起删，
		// 复用 ID 才安全。
		runtime.forgetCastSpawnsLocked(cast.id)
		delete(runtime.casts, cast.id)
		runtime.forgetCompletedCastLocked(cast.id)
		runtime.nextCastID--
		return 0, err
	}
	// 已提交，或有衍生物停不下来（宿主 StopSpawn 失败、记录已标成待停止）：cast 留作 failed 终态、不还 ID，
	// 待停止的衍生物记录继续挂在一个存在的 cast 名下，由之后的 tick 重试停止，停掉后按终态施法的规则回收
	// （runtime_retention.go）。
	// 名下有已放弃的记录（它比 cast 活得久）同样不还 ID，免得这条记录挂到下一个 cast 名下。
	return cast.id, err
}

// CastCount 返回 Runtime 当前保留的施法数（含终态施法）。
func (runtime *Runtime) CastCount() int {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return len(runtime.casts)
}

// ActiveCastCount 返回仍在活动的施法数。
func (runtime *Runtime) ActiveCastCount() int {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return runtime.activeCastCount
}

// InspectCast 返回指定施法的快照；施法不存在（或已被淘汰）时第二个返回值为 false。
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
