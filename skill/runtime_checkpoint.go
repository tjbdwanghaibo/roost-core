package skill

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// RuntimeCheckpointVersion 标识 checkpoint payload 的格式。格式每次变化（扩展或改名）都递增版本号，恢复只接受当前版本，
// 旧版本得到 ErrCheckpointUnsupported（排空后再升级）。3：衍生物（当时叫 process）记录加待停止重试状态
// （stop_pending 与 stop_retry_*），payload 加三项停止重试选项（RR-20261006-21 后续，2026-10-06；线上未部署，
// 不兼容版本 2）。4：process 全量改名为 spawn（衍生物），JSON 字段名随之改变（processes → spawns、
// next_process_id → next_spawn_id、owned_processes → owned_spawns 等，对照见
// docs/feature/REFACTOR-2026-10-06-skill-process-to-spawn.md）；维护者 2026-10-06 第十三轮决定，线上未部署，
// 不兼容版本 3。5：生成宿主单位的效果改名为召唤（summon），cast 值里召唤效果结果的类型 spawn_result →
// summon_result（字段名不变，变的是值；对照见 docs/feature/REFACTOR-2026-10-07-skill-summon-rename.md）；
// 维护者第十三轮决定，线上未部署，不兼容版本 4。6：衍生物记录只存一份（spawns），删掉重复存储“已移交、仍在运行”
// 那部分的 owned_spawns；恢复时按记录字段（status、handed_off）重新分区（docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md）；
// 维护者第十三轮“skill 衍生物两张表”，线上未部署，不兼容版本 5。7：衍生物新状态 abandoned（待停止超过
// MaxStopPendingSpawns 时放弃、不删记录），payload 加 max_abandoned_spawns；维护者第十三轮“待停止上限”选 B
// （docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md §11），线上未部署，不兼容版本 6。8：payload 加
// spawn_event_sequence（衍生物回调事件 ID 的计数，RR-20261006-53），任务删掉 spawn_step 一类与它的 spawn_id 字段
// （衍生物只由 advanceOwnedSpawns 推进，RR-20261006-51）；施放中衍生物的 next_tick 从此生效。线上未部署，不兼容版本 7。
const RuntimeCheckpointVersion uint32 = 8
const RuntimeCheckpointMaxBytes = 64 << 20
const RuntimeCheckpointMaxRecords = 1_000_000

var (
	ErrCheckpointCorrupt      = errors.New("skill: runtime checkpoint is corrupt")
	ErrCheckpointUnsupported  = errors.New("skill: runtime checkpoint version is unsupported")
	ErrCheckpointHostMismatch = errors.New("skill: runtime checkpoint does not match host state")
	ErrCheckpointProgram      = errors.New("skill: runtime checkpoint program cannot be resolved")
)

// RuntimeCheckpoint is a versioned, checksummed authoritative gameplay image.
// Delivery buffers (trace, presentation and state mutation queues) are not part
// of gameplay and are intentionally rebuilt after restore.
type RuntimeCheckpoint struct {
	Version  uint32 `json:"version"`
	Payload  []byte `json:"payload"`
	Checksum string `json:"checksum"`
}

// ProgramResolver supplies immutable compiled programs referenced by a
// checkpoint. Restore validates every returned program before publishing the
// recovered Runtime.
type ProgramResolver interface {
	ResolveProgram(id, gameplayDigest string) (*Program, error)
}

type ProgramResolverFunc func(id, gameplayDigest string) (*Program, error)

func (function ProgramResolverFunc) ResolveProgram(id, gameplayDigest string) (*Program, error) {
	return function(id, gameplayDigest)
}

type checkpointProgramRef struct {
	ID                 string            `json:"id"`
	GameplayDigest     string            `json:"gameplay_digest"`
	PresentationDigest string            `json:"presentation_digest"`
	SemanticsRevision  string            `json:"semantics_revision"`
	Authority          AuthorityIdentity `json:"authority"`
}

type runtimeCheckpointPayload struct {
	WorldRevision       WorldRevision     `json:"world_revision"`
	Authority           AuthorityIdentity `json:"authority"`
	MatchSeed           [32]byte          `json:"match_seed"`
	SemanticsRevision   string            `json:"semantics_revision"`
	MaxPassivePerTick   int               `json:"max_passive_per_tick"`
	MaxOwned            int               `json:"max_owned"`
	MaxOwnedPerOwner    int               `json:"max_owned_per_owner"`
	MaxOwnedPerProgram  int               `json:"max_owned_per_program"`
	MaxOwnedPerTemplate int               `json:"max_owned_per_template"`
	MaxActiveCasts      int               `json:"max_active_casts"`
	MaxAbilities        int               `json:"max_abilities"`
	CompletedCastLimit  int               `json:"completed_cast_limit"`
	// CompletedCastOrder is the completion order of the terminal casts, which
	// pruneCompletedCastsLocked evicts from the head. Casts are serialized in
	// id order, so rebuilding the queue from them replaced "oldest completion"
	// with "lowest id" and made retention diverge across a restore
	// (RR-20260910-04). Absent in checkpoints written before this field, and
	// restore then falls back to id order — what it did all along.
	CompletedCastOrder   []CastID `json:"completed_cast_order,omitempty"`
	RootEventLimit       int      `json:"root_event_limit"`
	MaxProcLedgerEntries int      `json:"max_proc_ledger_entries"`
	// 停止重试的三项选项随 checkpoint 走，恢复后的 Runtime 与原 Runtime 在同样的 tick 重试（版本 3）。
	SpawnStopRetryBackoff Tick                      `json:"spawn_stop_retry_backoff"`
	SpawnStopRetryLimit   int                       `json:"spawn_stop_retry_limit"`
	MaxStopPendingSpawns  int                       `json:"max_stop_pending_spawns"`
	MaxAbandonedSpawns    int                       `json:"max_abandoned_spawns"`
	CurrentTick           Tick                      `json:"current_tick"`
	EventCursor           EventCursor               `json:"event_cursor"`
	NextCastID            CastID                    `json:"next_cast_id"`
	NextTaskSequence      uint64                    `json:"next_task_sequence"`
	NextFrameID           FrameID                   `json:"next_frame_id"`
	NextSpawnID           SpawnID                   `json:"next_spawn_id"`
	SpawnEventSequence    uint64                    `json:"spawn_event_sequence"`
	NextPassiveActivation PassiveActivationID       `json:"next_passive_activation_id"`
	NextAbilityHandle     AbilityHandle             `json:"next_ability_handle"`
	NextAbilityOverlay    uint64                    `json:"next_ability_overlay"`
	PassiveCountTick      Tick                      `json:"passive_count_tick"`
	PassiveCount          int                       `json:"passive_count"`
	TraceSequence         uint64                    `json:"trace_sequence"`
	PresentationSequence  uint64                    `json:"presentation_sequence"`
	StateEventSequence    uint64                    `json:"state_event_sequence"`
	StateEventDropped     uint64                    `json:"state_event_dropped"`
	StateMutationSequence uint64                    `json:"state_mutation_sequence"`
	StateMutationDropped  uint64                    `json:"state_mutation_dropped"`
	StateMutationBaseline RuntimeStateSnapshot      `json:"state_mutation_baseline"`
	StateMutationReady    bool                      `json:"state_mutation_ready"`
	Casts                 []checkpointCast          `json:"casts"`
	Spawns                []checkpointSpawn         `json:"spawns"`
	Frames                []checkpointFrame         `json:"frames"`
	Tasks                 []checkpointTask          `json:"tasks"`
	Cooldowns             []checkpointCooldown      `json:"cooldowns"`
	SkillStates           []checkpointSkillState    `json:"skill_states"`
	ActivePolicies        []checkpointActivePolicy  `json:"active_policies"`
	ProcLedger            []checkpointProcLedger    `json:"proc_ledger"`
	RootEventCounts       []checkpointRootEvent     `json:"root_event_counts"`
	Abilities             []checkpointAbility       `json:"abilities"`
	AbilityByProgram      []checkpointAbilityLookup `json:"ability_by_program"`
}

type checkpointCast struct {
	ID                 CastID                       `json:"id"`
	Program            checkpointProgramRef         `json:"program"`
	Caster             EntityID                     `json:"caster"`
	PrimaryTarget      EntityID                     `json:"primary_target"`
	Inputs             []checkpointRuntimeValue     `json:"inputs"`
	Memory             []checkpointRuntimeValue     `json:"memory"`
	Locals             []checkpointRuntimeValue     `json:"locals"`
	Snapshots          []checkpointValueEntry       `json:"snapshots"`
	Status             CastStatus                   `json:"status"`
	CurrentPhase       PhaseIndex                   `json:"current_phase"`
	VisibleRevision    WorldRevision                `json:"visible_revision"`
	Failure            string                       `json:"failure,omitempty"`
	RandomKey          [32]byte                     `json:"random_key"`
	RandomInvocations  []checkpointRandomInvocation `json:"random_invocations"`
	EventContext       checkpointEventContext       `json:"event_context"`
	PhaseToken         uint64                       `json:"phase_token"`
	PendingTasks       int                          `json:"pending_tasks"`
	LogicalFinished    bool                         `json:"logical_finished"`
	AreaCallbackFinish bool                         `json:"area_callback_finish"`
	WindowStage        CastWindowStage              `json:"window_stage"`
	StartTick          Tick                         `json:"start_tick"`
	Committed          bool                         `json:"committed"`
	CostsPaid          bool                         `json:"costs_paid"`
	CooldownStarted    bool                         `json:"cooldown_started"`
	PulseIndex         int64                        `json:"pulse_index"`
	ReleaseReason      string                       `json:"release_reason,omitempty"`
	Stock              int64                        `json:"stock"`
	MaxStock           int64                        `json:"max_stock"`
	WindowStartTick    Tick                         `json:"window_start_tick"`
	PendingRootEvent   string                       `json:"pending_root_event,omitempty"`
	PolicyActive       bool                         `json:"policy_active"`
	CooldownOwner      EntityID                     `json:"cooldown_owner"`
	Ability            AbilityHandle                `json:"ability"`
	AbilityFinished    bool                         `json:"ability_finished"`
}

type checkpointSpawn struct {
	ID                       SpawnID                      `json:"id"`
	CastID                   CastID                       `json:"cast_id"`
	TemplateIndex            SpawnTemplateIndex           `json:"template_index"`
	UnitTemplate             UnitTemplateHandle           `json:"unit_template"`
	Status                   SpawnStatus                  `json:"status"`
	StartTick                Tick                         `json:"start_tick"`
	NextTick                 Tick                         `json:"next_tick"`
	EndTick                  Tick                         `json:"end_tick"`
	Scope                    SpawnScope                   `json:"scope"`
	HostState                SpawnHostState               `json:"host_state"`
	Motion                   MotionState                  `json:"motion"`
	Numeric                  checkpointSpawnNumeric       `json:"numeric"`
	Owner                    EntityID                     `json:"owner"`
	LifecycleEntity          EntityID                     `json:"lifecycle_entity"`
	Program                  checkpointProgramRef         `json:"program"`
	DirectProgram            bool                         `json:"direct_program"`
	Inputs                   []checkpointRuntimeValue     `json:"inputs"`
	Memory                   []checkpointRuntimeValue     `json:"memory"`
	Locals                   []checkpointRuntimeValue     `json:"locals"`
	Snapshots                []checkpointValueEntry       `json:"snapshots"`
	RandomKey                [32]byte                     `json:"random_key"`
	RandomInvocations        []checkpointRandomInvocation `json:"random_invocations"`
	VisibleRevision          WorldRevision                `json:"visible_revision"`
	EventContext             checkpointEventContext       `json:"event_context"`
	AreaMembers              []checkpointAreaMember       `json:"area_members"`
	PhaseToken               uint64                       `json:"phase_token"`
	StopCause                StopCause                    `json:"stop_cause"`
	HandedOff                bool                         `json:"handed_off"`
	AreaCallbackFinishedCast bool                         `json:"area_callback_finished_cast"`
	// 只在 status 为 stop_pending 时出现（版本 3）。
	StopRetryAttempts  int  `json:"stop_retry_attempts,omitempty"`
	StopRetryTick      Tick `json:"stop_retry_tick,omitempty"`
	StopRetryExhausted bool `json:"stop_retry_exhausted,omitempty"`
}

type checkpointSpawnNumeric struct {
	Initialized bool                        `json:"initialized"`
	Properties  []checkpointNumericProperty `json:"properties"`
}

type checkpointNumericProperty struct {
	Property SpawnPropertyHandle      `json:"property"`
	Base     int64                    `json:"base"`
	Current  int64                    `json:"current"`
	Track    *numericTrackState       `json:"track,omitempty"`
	Stage    spawnPropertySlotStage   `json:"stage"`
	Variant  spawnPropertySlotVariant `json:"variant"`
	Field    spawnPropertySlotField   `json:"field"`
	Bound    bool                     `json:"bound"`
}

type checkpointRuntimeValue struct {
	Present      bool                         `json:"present"`
	Type         valueType                    `json:"type"`
	Integer      int64                        `json:"integer,omitempty"`
	Boolean      bool                         `json:"boolean,omitempty"`
	Text         string                       `json:"text,omitempty"`
	Entity       EntityID                     `json:"entity,omitempty"`
	Position     Position                     `json:"position"`
	Direction    Direction                    `json:"direction"`
	Hit          Hit                          `json:"hit"`
	Path         []Position                   `json:"path,omitempty"`
	Ability      AbilityRef                   `json:"ability"`
	Status       StatusInstanceRef            `json:"status"`
	Entities     []EntityID                   `json:"entities,omitempty"`
	Strings      []string                     `json:"strings,omitempty"`
	Snapshot     uint64                       `json:"snapshot,omitempty"`
	Spawn        SpawnID                      `json:"spawn,omitempty"`
	EffectResult *checkpointEffectResultValue `json:"effect_result,omitempty"`
}

type checkpointEffectResultValue struct {
	Type    resultType               `json:"type"`
	Outcome ResultOutcome            `json:"outcome"`
	Fields  []checkpointRuntimeValue `json:"fields"`
}

type checkpointValueEntry struct {
	Index int                    `json:"index"`
	Value checkpointRuntimeValue `json:"value"`
}
type checkpointRandomInvocation struct {
	Site  RandomSiteIndex `json:"site"`
	Count uint64          `json:"count"`
}
type checkpointAreaMember struct {
	Entity EntityID        `json:"entity"`
	State  AreaMemberState `json:"state"`
}
type checkpointFrame struct {
	ID     FrameID                  `json:"id"`
	Values []checkpointRuntimeValue `json:"values"`
}
type checkpointCooldown struct {
	Caster EntityID `json:"caster"`
	Skill  string   `json:"skill"`
	Due    Tick     `json:"due"`
}
type checkpointSkillState struct {
	Caster                     EntityID `json:"caster"`
	Skill                      string   `json:"skill"`
	Stock, MaxStock            int64
	RechargeTicks, RechargeDue Tick
	RechargeScheduled          bool
	RechargeGeneration         uint64
}
type checkpointActivePolicy struct {
	Caster EntityID `json:"caster"`
	Skill  string   `json:"skill"`
	CastID CastID   `json:"cast_id"`
}
type checkpointProcLedger struct {
	Root   EventID  `json:"root"`
	Caster EntityID `json:"caster"`
	Digest string   `json:"digest"`
}
type checkpointRootEvent struct {
	ID    EventID `json:"id"`
	Count int     `json:"count"`
}
type checkpointAbility struct {
	Owner                          EntityID             `json:"owner"`
	Handle                         AbilityHandle        `json:"handle"`
	Slot                           int                  `json:"slot"`
	Tags                           []GameplayTagHandle  `json:"tags"`
	Program                        checkpointProgramRef `json:"program"`
	CooldownTotal                  Tick                 `json:"cooldown_total"`
	AmmoStock, AmmoMax             int64
	CastActive                     int
	LastCommitTick, LastFinishTick Tick
	Overlays                       []checkpointOverlay `json:"overlays"`
}
type checkpointOverlay struct {
	ID  uint64 `json:"id"`
	Due Tick   `json:"due"`
}
type checkpointAbilityLookup struct {
	Caster EntityID      `json:"caster"`
	Skill  string        `json:"skill"`
	Handle AbilityHandle `json:"handle"`
}

type checkpointEventContext struct {
	EventContext
	GameplayTags []GameplayTagHandle `json:"gameplay_tags,omitempty"`
}

type checkpointTask struct {
	DueTick    Tick                   `json:"due_tick"`
	Sequence   uint64                 `json:"sequence"`
	Kind       string                 `json:"kind"`
	CastID     CastID                 `json:"cast_id,omitempty"`
	PhaseToken uint64                 `json:"phase_token,omitempty"`
	Frame      FrameID                `json:"frame,omitempty"`
	Operations []OperationIndex       `json:"operations,omitempty"`
	Body       OperationIndex         `json:"body,omitempty"`
	IndexLocal LocalIndex             `json:"index_local,omitempty"`
	Iteration  int64                  `json:"iteration,omitempty"`
	Times      int64                  `json:"times,omitempty"`
	Interval   Tick                   `json:"interval,omitempty"`
	Tail       []OperationIndex       `json:"tail,omitempty"`
	Operation  OperationIndex         `json:"operation,omitempty"`
	Hop        int                    `json:"hop,omitempty"`
	PulseIndex int64                  `json:"pulse_index,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
	Caster     EntityID               `json:"caster,omitempty"`
	Skill      string                 `json:"skill,omitempty"`
	Generation uint64                 `json:"generation,omitempty"`
	PassiveID  PassiveActivationID    `json:"passive_id,omitempty"`
	Program    checkpointProgramRef   `json:"program"`
	Event      checkpointEventContext `json:"event"`
	Owner      EntityID               `json:"owner,omitempty"`
	Ability    AbilityHandle          `json:"ability,omitempty"`
	OverlayID  uint64                 `json:"overlay_id,omitempty"`
}

func (runtime *Runtime) Checkpoint() (RuntimeCheckpoint, error) {
	if runtime == nil {
		return RuntimeCheckpoint{}, ErrCheckpointCorrupt
	}
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if runtime.host == nil {
		return RuntimeCheckpoint{}, ErrCheckpointHostMismatch
	}
	payload, err := runtime.checkpointPayloadLocked()
	if err != nil {
		return RuntimeCheckpoint{}, err
	}
	if runtime.host.CurrentRevision() != payload.WorldRevision || !authorityMatches(payload.Authority, runtime.host.AuthorityIdentity()) {
		return RuntimeCheckpoint{}, ErrCheckpointHostMismatch
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return RuntimeCheckpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if len(data) > runtime.options.CheckpointMaxBytes || checkpointRecordCount(payload) > runtime.options.CheckpointMaxRecords {
		return RuntimeCheckpoint{}, ErrCheckpointCorrupt
	}
	digest := sha256.Sum256(data)
	return RuntimeCheckpoint{Version: RuntimeCheckpointVersion, Payload: data, Checksum: hex.EncodeToString(digest[:])}, nil
}

func RestoreRuntime(host Host, options RuntimeOptions, checkpoint RuntimeCheckpoint, resolver ProgramResolver) (*Runtime, error) {
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
	if checkpoint.Version != RuntimeCheckpointVersion {
		return nil, ErrCheckpointUnsupported
	}
	if host == nil || resolver == nil || len(checkpoint.Payload) == 0 || len(checkpoint.Payload) > options.CheckpointMaxBytes {
		return nil, ErrCheckpointCorrupt
	}
	digest := sha256.Sum256(checkpoint.Payload)
	if subtle.ConstantTimeCompare([]byte(checkpoint.Checksum), []byte(hex.EncodeToString(digest[:]))) != 1 {
		return nil, ErrCheckpointCorrupt
	}
	if err := rejectDuplicateKeysWithLimits(checkpoint.Payload, ParseLimits{MaxBytes: options.CheckpointMaxBytes, MaxDepth: 128, MaxTokens: options.CheckpointMaxRecords * 64, MaxStringBytes: 1 << 20, MaxContainerEntries: options.CheckpointMaxRecords}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	var payload runtimeCheckpointPayload
	decoder := json.NewDecoder(bytes.NewReader(checkpoint.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckpointCorrupt, err)
	}
	if checkpointRecordCount(payload) > options.CheckpointMaxRecords {
		return nil, ErrCheckpointCorrupt
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrCheckpointCorrupt
	}
	if host.CurrentRevision() != payload.WorldRevision || !authorityMatches(payload.Authority, host.AuthorityIdentity()) {
		return nil, ErrCheckpointHostMismatch
	}
	if !validCheckpointRuntimeLimits(payload) {
		return nil, ErrCheckpointCorrupt
	}
	options.MatchSeed = payload.MatchSeed
	options.SupportedCompilerSemanticsRevision = payload.SemanticsRevision
	options.MaxPassiveActivationsPerTick = payload.MaxPassivePerTick
	options.MaxOwnedSpawns = payload.MaxOwned
	options.MaxOwnedSpawnsPerOwner = payload.MaxOwnedPerOwner
	options.MaxOwnedSpawnsPerProgram = payload.MaxOwnedPerProgram
	options.MaxOwnedSpawnsPerTemplate = payload.MaxOwnedPerTemplate
	options.MaxActiveCasts = payload.MaxActiveCasts
	options.MaxAbilities = payload.MaxAbilities
	options.CompletedCastLimit = payload.CompletedCastLimit
	options.RootEventLimit = payload.RootEventLimit
	options.MaxProcLedgerEntries = payload.MaxProcLedgerEntries
	options.SpawnStopRetryBackoff = payload.SpawnStopRetryBackoff
	options.SpawnStopRetryLimit = payload.SpawnStopRetryLimit
	options.MaxStopPendingSpawns = payload.MaxStopPendingSpawns
	options.MaxAbandonedSpawns = payload.MaxAbandonedSpawns
	// newRuntimeCore, not NewRuntime: the fresh-runtime path fast-forwards
	// the event cursor to the host's frontier and compacts everything before
	// it — which would DELETE the events emitted between the checkpoint and
	// the crash before restoreCheckpointPayload rewinds the cursor to the
	// checkpoint value. Those events are exactly what a restored runtime must
	// replay. HostEventCompactor implementations must therefore retain all
	// events since the last successful checkpoint.
	runtime := newRuntimeCore(host, options)
	// 恢复出来的每个 Program 同样要在 Host 声明的能力表里（B3 ③）。
	if err := runtime.restoreCheckpointPayload(payload, hostCheckedResolver{resolver: resolver, host: host}); err != nil {
		return nil, err
	}
	if host.CurrentRevision() != payload.WorldRevision || !authorityMatches(payload.Authority, host.AuthorityIdentity()) {
		return nil, ErrCheckpointHostMismatch
	}
	return runtime, nil
}

func programCheckpointRef(program *Program) checkpointProgramRef {
	if program == nil {
		return checkpointProgramRef{}
	}
	return checkpointProgramRef{ID: program.id, GameplayDigest: program.identity.gameplayDigest, PresentationDigest: program.identity.presentationDigest, SemanticsRevision: program.compilerSemanticsRevision, Authority: program.authority}
}

func resolveCheckpointProgram(ref checkpointProgramRef, resolver ProgramResolver, authority AuthorityIdentity, semantics string) (*Program, error) {
	if ref.ID == "" || ref.GameplayDigest == "" {
		return nil, ErrCheckpointProgram
	}
	program, err := resolver.ResolveProgram(ref.ID, ref.GameplayDigest)
	if err != nil || program == nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCheckpointProgram, ref.ID, err)
	}
	actual := programCheckpointRef(program)
	if actual != ref || actual.SemanticsRevision != semantics || !authorityMatches(program.authority, authority) {
		return nil, fmt.Errorf("%w: identity mismatch for %s", ErrCheckpointProgram, ref.ID)
	}
	return program, nil
}

func checkpointEvent(value EventContext) checkpointEventContext {
	return checkpointEventContext{EventContext: value, GameplayTags: value.GameplayTags()}
}
func restoreCheckpointEvent(value checkpointEventContext) EventContext {
	result := value.EventContext
	result.gameplayTags = normalizeGameplayTagHandles(value.GameplayTags)
	return result
}

func checkpointValue(value RuntimeValue) checkpointRuntimeValue {
	result := checkpointRuntimeValue{Present: value.present, Type: value.typ, Integer: value.integer, Boolean: value.boolean, Text: value.text, Entity: value.entity, Position: value.position, Direction: value.direction, Hit: value.hit, Path: append([]Position(nil), value.path...), Ability: value.ability, Status: value.status, Entities: append([]EntityID(nil), value.entities...), Strings: append([]string(nil), value.strings...), Snapshot: value.snapshot.opaque, Spawn: value.spawn}
	if value.typ.Base == valueKindEffectResult {
		fields := make([]checkpointRuntimeValue, len(value.effectResult.fields))
		for index := range value.effectResult.fields {
			fields[index] = checkpointValue(value.effectResult.fields[index])
		}
		result.EffectResult = &checkpointEffectResultValue{Type: value.effectResult.typ, Outcome: value.effectResult.outcome, Fields: fields}
	}
	return result
}

func restoreCheckpointValue(value checkpointRuntimeValue) (RuntimeValue, error) {
	if value.Type.Base > valueKindEffectResult || value.Type.Base == valueKindInvalid && value.Present {
		return RuntimeValue{}, ErrCheckpointCorrupt
	}
	result := RuntimeValue{present: value.Present, typ: value.Type, integer: value.Integer, boolean: value.Boolean, text: value.Text, entity: value.Entity, position: value.Position, direction: value.Direction, hit: value.Hit, path: append([]Position(nil), value.Path...), ability: value.Ability, status: value.Status, entities: append([]EntityID(nil), value.Entities...), strings: append([]string(nil), value.Strings...), snapshot: SnapshotToken{opaque: value.Snapshot}, spawn: value.Spawn}
	if value.Type.Base == valueKindEffectResult {
		if value.EffectResult == nil {
			return RuntimeValue{}, ErrCheckpointCorrupt
		}
		fields, err := restoreCheckpointValues(value.EffectResult.Fields)
		if err != nil {
			return RuntimeValue{}, err
		}
		result.effectResult = runtimeEffectResultValue{typ: value.EffectResult.Type, outcome: value.EffectResult.Outcome, fields: fields}
	} else if value.EffectResult != nil {
		return RuntimeValue{}, ErrCheckpointCorrupt
	}
	return result, nil
}

func checkpointValues(values []RuntimeValue) []checkpointRuntimeValue {
	result := make([]checkpointRuntimeValue, len(values))
	for index := range values {
		result[index] = checkpointValue(values[index])
	}
	return result
}
func restoreCheckpointValues(values []checkpointRuntimeValue) ([]RuntimeValue, error) {
	result := make([]RuntimeValue, len(values))
	for index := range values {
		value, err := restoreCheckpointValue(values[index])
		if err != nil {
			return nil, err
		}
		result[index] = value
	}
	return result, nil
}

func checkpointValueMap(values map[int]RuntimeValue) []checkpointValueEntry {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	result := make([]checkpointValueEntry, 0, len(keys))
	for _, key := range keys {
		result = append(result, checkpointValueEntry{Index: key, Value: checkpointValue(values[key])})
	}
	return result
}
func restoreCheckpointValueMap(values []checkpointValueEntry) (map[int]RuntimeValue, error) {
	result := make(map[int]RuntimeValue, len(values))
	for _, entry := range values {
		if _, exists := result[entry.Index]; exists {
			return nil, ErrCheckpointCorrupt
		}
		value, err := restoreCheckpointValue(entry.Value)
		if err != nil {
			return nil, err
		}
		result[entry.Index] = value
	}
	return result, nil
}

func checkpointRandom(values map[RandomSiteIndex]uint64) []checkpointRandomInvocation {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, int(key))
	}
	sort.Ints(keys)
	result := make([]checkpointRandomInvocation, 0, len(keys))
	for _, key := range keys {
		result = append(result, checkpointRandomInvocation{Site: RandomSiteIndex(key), Count: values[RandomSiteIndex(key)]})
	}
	return result
}
func restoreCheckpointRandom(values []checkpointRandomInvocation) (map[RandomSiteIndex]uint64, error) {
	result := make(map[RandomSiteIndex]uint64, len(values))
	for _, entry := range values {
		if _, ok := result[entry.Site]; ok {
			return nil, ErrCheckpointCorrupt
		}
		result[entry.Site] = entry.Count
	}
	return result, nil
}

func (runtime *Runtime) checkpointPayloadLocked() (runtimeCheckpointPayload, error) {
	if !runtime.stateMutationReady || !runtimeSnapshotsEqual(runtime.stateMutationBaseline, runtime.stateSnapshotLocked()) {
		return runtimeCheckpointPayload{}, ErrCheckpointHostMismatch
	}
	p := runtimeCheckpointPayload{WorldRevision: runtime.host.CurrentRevision(), Authority: runtime.host.AuthorityIdentity(), MatchSeed: runtime.options.MatchSeed, SemanticsRevision: runtime.options.SupportedCompilerSemanticsRevision, MaxPassivePerTick: runtime.options.MaxPassiveActivationsPerTick, MaxOwned: runtime.options.MaxOwnedSpawns, MaxOwnedPerOwner: runtime.options.MaxOwnedSpawnsPerOwner, MaxOwnedPerProgram: runtime.options.MaxOwnedSpawnsPerProgram, MaxOwnedPerTemplate: runtime.options.MaxOwnedSpawnsPerTemplate, MaxActiveCasts: runtime.options.MaxActiveCasts, MaxAbilities: runtime.options.MaxAbilities, CompletedCastLimit: runtime.options.CompletedCastLimit, RootEventLimit: runtime.options.RootEventLimit, MaxProcLedgerEntries: runtime.options.MaxProcLedgerEntries, SpawnStopRetryBackoff: runtime.options.SpawnStopRetryBackoff, SpawnStopRetryLimit: runtime.options.SpawnStopRetryLimit, MaxStopPendingSpawns: runtime.options.MaxStopPendingSpawns, MaxAbandonedSpawns: runtime.options.MaxAbandonedSpawns, CurrentTick: runtime.currentTick, EventCursor: runtime.eventCursor, NextCastID: runtime.nextCastID, NextTaskSequence: runtime.nextTaskSequence, NextFrameID: runtime.nextFrameID, NextSpawnID: runtime.nextSpawnID, SpawnEventSequence: runtime.spawnEventSequence, NextPassiveActivation: runtime.nextPassiveActivationID, NextAbilityHandle: runtime.nextAbilityHandle, NextAbilityOverlay: runtime.nextAbilityOverlay, PassiveCountTick: runtime.passiveCountTick, PassiveCount: runtime.passiveCount, TraceSequence: runtime.traceSequence, PresentationSequence: runtime.presentationSequence, StateEventSequence: runtime.stateEventSequence, StateEventDropped: runtime.stateEventDropped, StateMutationSequence: runtime.stateMutationSequence, StateMutationDropped: runtime.stateMutationDropped, StateMutationBaseline: runtime.stateMutationBaseline, StateMutationReady: runtime.stateMutationReady}
	p.CompletedCastOrder = append([]CastID(nil), runtime.completedCastOrder...)
	castIDs := make([]int, 0, len(runtime.casts))
	for id := range runtime.casts {
		castIDs = append(castIDs, int(id))
	}
	sort.Ints(castIDs)
	for _, raw := range castIDs {
		c := runtime.casts[CastID(raw)]
		p.Casts = append(p.Casts, checkpointCast{ID: c.id, Program: programCheckpointRef(c.program), Caster: c.caster, PrimaryTarget: c.primaryTarget, Inputs: checkpointValues(c.inputs), Memory: checkpointValues(c.memory), Locals: checkpointValues(c.locals), Snapshots: checkpointValueMap(c.snapshots), Status: c.status, CurrentPhase: c.currentPhase, VisibleRevision: c.visibleRevision, Failure: c.failure, RandomKey: c.randomKey, RandomInvocations: checkpointRandom(c.randomInvocations), EventContext: checkpointEvent(c.eventContext), PhaseToken: c.phaseToken, PendingTasks: c.pendingTasks, LogicalFinished: c.logicalFinished, AreaCallbackFinish: c.areaCallbackFinish, WindowStage: c.windowStage, StartTick: c.startTick, Committed: c.committed, CostsPaid: c.costsPaid, CooldownStarted: c.cooldownStarted, PulseIndex: c.pulseIndex, ReleaseReason: c.releaseReason, Stock: c.stock, MaxStock: c.maxStock, WindowStartTick: c.windowStartTick, PendingRootEvent: c.pendingRootEvent, PolicyActive: c.policyActive, CooldownOwner: c.cooldownOwner, Ability: c.ability, AbilityFinished: c.abilityFinished})
	}
	var err error
	resolveSpawnProgram := func(spawn *SpawnInstance) *Program {
		if spawn == nil {
			return nil
		}
		if spawn.Program != nil {
			return spawn.Program
		}
		if cast := runtime.casts[spawn.CastID]; cast != nil {
			return cast.program
		}
		return nil
	}
	p.Spawns, err = checkpointSpawnTable(&runtime.spawns, resolveSpawnProgram)
	if err != nil {
		return p, err
	}
	frameIDs := make([]int, 0, len(runtime.frames))
	for id := range runtime.frames {
		frameIDs = append(frameIDs, int(id))
	}
	sort.Ints(frameIDs)
	for _, raw := range frameIDs {
		p.Frames = append(p.Frames, checkpointFrame{ID: FrameID(raw), Values: checkpointValues(runtime.frames[FrameID(raw)])})
	}
	for _, task := range runtime.scheduler.tasks {
		wire, taskErr := checkpointScheduledTask(task)
		if taskErr != nil {
			return p, taskErr
		}
		p.Tasks = append(p.Tasks, wire)
	}
	sort.Slice(p.Tasks, func(i, j int) bool {
		if p.Tasks[i].DueTick != p.Tasks[j].DueTick {
			return p.Tasks[i].DueTick < p.Tasks[j].DueTick
		}
		return p.Tasks[i].Sequence < p.Tasks[j].Sequence
	})
	for key, due := range runtime.cooldowns {
		p.Cooldowns = append(p.Cooldowns, checkpointCooldown{Caster: key.Caster, Skill: key.Skill, Due: due})
	}
	sort.Slice(p.Cooldowns, func(i, j int) bool {
		if p.Cooldowns[i].Caster != p.Cooldowns[j].Caster {
			return p.Cooldowns[i].Caster < p.Cooldowns[j].Caster
		}
		return p.Cooldowns[i].Skill < p.Cooldowns[j].Skill
	})
	for key, state := range runtime.skillStates {
		p.SkillStates = append(p.SkillStates, checkpointSkillState{Caster: key.Caster, Skill: key.Skill, Stock: state.stock, MaxStock: state.maxStock, RechargeTicks: state.rechargeTicks, RechargeDue: state.rechargeDue, RechargeScheduled: state.rechargeScheduled, RechargeGeneration: state.rechargeGeneration})
	}
	sort.Slice(p.SkillStates, func(i, j int) bool {
		if p.SkillStates[i].Caster != p.SkillStates[j].Caster {
			return p.SkillStates[i].Caster < p.SkillStates[j].Caster
		}
		return p.SkillStates[i].Skill < p.SkillStates[j].Skill
	})
	// 下面四个列表来自 map，按键排序写出，同一状态的 checkpoint 字节确定（O7，维护者第十二轮
	// 决定）。以前按 map 迭代顺序写出；恢复与顺序无关，所以旧的乱序 checkpoint 照常可读。
	for key, id := range runtime.activePolicies {
		p.ActivePolicies = append(p.ActivePolicies, checkpointActivePolicy{Caster: key.Caster, Skill: key.Skill, CastID: id})
	}
	sort.Slice(p.ActivePolicies, func(i, j int) bool {
		return skillStateKeyLess(p.ActivePolicies[i].Caster, p.ActivePolicies[i].Skill, p.ActivePolicies[j].Caster, p.ActivePolicies[j].Skill)
	})
	for key := range runtime.procLedger {
		p.ProcLedger = append(p.ProcLedger, checkpointProcLedger{Root: key.Root, Caster: key.Caster, Digest: key.Digest})
	}
	sort.Slice(p.ProcLedger, func(i, j int) bool {
		if p.ProcLedger[i].Root != p.ProcLedger[j].Root {
			return p.ProcLedger[i].Root < p.ProcLedger[j].Root
		}
		return skillStateKeyLess(p.ProcLedger[i].Caster, p.ProcLedger[i].Digest, p.ProcLedger[j].Caster, p.ProcLedger[j].Digest)
	})
	for id, count := range runtime.rootEventCounts {
		p.RootEventCounts = append(p.RootEventCounts, checkpointRootEvent{ID: id, Count: count})
	}
	sort.Slice(p.RootEventCounts, func(i, j int) bool { return p.RootEventCounts[i].ID < p.RootEventCounts[j].ID })
	for _, state := range runtime.abilities {
		a := checkpointAbility{Owner: state.owner, Handle: state.handle, Slot: state.slot, Tags: append([]GameplayTagHandle(nil), state.tags...), Program: programCheckpointRef(state.program), CooldownTotal: state.cooldownTotal, AmmoStock: state.ammoStock, AmmoMax: state.ammoMax, CastActive: state.castActive, LastCommitTick: state.lastCommitTick, LastFinishTick: state.lastFinishTick}
		for id, due := range state.overlays {
			a.Overlays = append(a.Overlays, checkpointOverlay{ID: id, Due: due})
		}
		sort.Slice(a.Overlays, func(i, j int) bool { return a.Overlays[i].ID < a.Overlays[j].ID })
		p.Abilities = append(p.Abilities, a)
	}
	sort.Slice(p.Abilities, func(i, j int) bool {
		if p.Abilities[i].Owner != p.Abilities[j].Owner {
			return p.Abilities[i].Owner < p.Abilities[j].Owner
		}
		return p.Abilities[i].Handle < p.Abilities[j].Handle
	})
	for key, handle := range runtime.abilityByProgram {
		p.AbilityByProgram = append(p.AbilityByProgram, checkpointAbilityLookup{Caster: key.Caster, Skill: key.Skill, Handle: handle})
	}
	sort.Slice(p.AbilityByProgram, func(i, j int) bool {
		return skillStateKeyLess(p.AbilityByProgram[i].Caster, p.AbilityByProgram[i].Skill, p.AbilityByProgram[j].Caster, p.AbilityByProgram[j].Skill)
	})
	return p, nil
}

// skillStateKeyLess 是 (caster, name) 键的写出顺序：先 caster，再名字。
func skillStateKeyLess(leftCaster EntityID, leftName string, rightCaster EntityID, rightName string) bool {
	if leftCaster != rightCaster {
		return leftCaster < rightCaster
	}
	return leftName < rightName
}

// checkpointSpawnTable 按 ID 升序写出全部分区的记录，每条一份；分区不进 checkpoint，恢复时由字段决定。
func checkpointSpawnTable(table *spawnTable, programFor func(*SpawnInstance) *Program) ([]checkpointSpawn, error) {
	ids := table.sortedIDs(nil)
	result := make([]checkpointSpawn, 0, len(ids))
	for _, id := range ids {
		spawn := table.get(id)
		program := programFor(spawn)
		if spawn == nil || program == nil {
			return nil, ErrCheckpointCorrupt
		}
		numeric := checkpointSpawnNumeric{Initialized: spawn.Numeric.Initialized}
		for _, state := range spawn.Numeric.Properties {
			var track *numericTrackState
			if state.Track != nil {
				copy := *state.Track
				track = &copy
			}
			numeric.Properties = append(numeric.Properties, checkpointNumericProperty{Property: state.Property, Base: state.Base, Current: state.Current, Track: track, Stage: state.Binding.stage, Variant: state.Binding.variant, Field: state.Binding.field, Bound: state.Bound})
		}
		item := checkpointSpawn{ID: spawn.ID, CastID: spawn.CastID, TemplateIndex: spawn.TemplateIndex, UnitTemplate: spawn.UnitTemplate, Status: spawn.Status, StartTick: spawn.StartTick, NextTick: spawn.NextTick, EndTick: spawn.EndTick, Scope: spawn.Scope, HostState: spawn.HostState, Motion: spawn.Motion, Numeric: numeric, Owner: spawn.Owner, LifecycleEntity: spawn.LifecycleEntity, Program: programCheckpointRef(program), DirectProgram: spawn.Program != nil, Inputs: checkpointValues(spawn.inputs), Memory: checkpointValues(spawn.memory), Locals: checkpointValues(spawn.locals), Snapshots: checkpointValueMap(spawn.snapshots), RandomKey: spawn.randomKey, RandomInvocations: checkpointRandom(spawn.randomInvocations), VisibleRevision: spawn.visibleRevision, EventContext: checkpointEvent(spawn.eventContext), PhaseToken: spawn.phaseToken, StopCause: spawn.stopCause, HandedOff: spawn.handedOff, AreaCallbackFinishedCast: spawn.areaCallbackFinishedCast, StopRetryAttempts: spawn.stopRetryAttempts, StopRetryTick: spawn.stopRetryTick, StopRetryExhausted: spawn.stopRetryExhausted}
		entities := make([]int, 0, len(spawn.AreaMembers))
		for entity := range spawn.AreaMembers {
			entities = append(entities, int(entity))
		}
		sort.Ints(entities)
		for _, entity := range entities {
			item.AreaMembers = append(item.AreaMembers, checkpointAreaMember{Entity: EntityID(entity), State: spawn.AreaMembers[EntityID(entity)]})
		}
		result = append(result, item)
	}
	return result, nil
}

func checkpointScheduledTask(task scheduledTask) (checkpointTask, error) {
	w := checkpointTask{DueTick: task.DueTick, Sequence: task.Sequence}
	switch t := task.Payload.(type) {
	case *flowContinuationTask:
		w.Kind = "flow"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.Operations = append([]OperationIndex(nil), t.Operations...)
	case *repeatIterationTask:
		w.Kind = "repeat"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.Body = t.Body
		w.IndexLocal = t.IndexLocal
		w.Iteration = t.Iteration
		w.Times = t.Times
		w.Interval = t.Interval
		w.Tail = append([]OperationIndex(nil), t.Tail...)
	case *chainHopTask:
		w.Kind = "chain_hop"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.Operation = t.Operation
		w.Hop = t.Hop
	case *castCommitTask:
		w.Kind = "cast_commit"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
	case *castExecuteTask:
		w.Kind = "cast_execute"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
	case *castRecoveryTask:
		w.Kind = "cast_recovery"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
	case *castPulseTask:
		w.Kind = "cast_pulse"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.PulseIndex = t.PulseIndex
	case *castAutoReleaseTask:
		w.Kind = "cast_auto_release"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.Reason = t.Reason
	case *ammoRechargeTask:
		w.Kind = "ammo_recharge"
		w.Caster = t.Caster
		w.Skill = t.Skill
		w.Generation = t.Generation
	case *passiveActivationTask:
		w.Kind = "passive_activation"
		w.PassiveID = t.ID
		w.Program = programCheckpointRef(t.Program)
		w.Event = checkpointEvent(t.Event)
		w.Owner = t.Owner
		w.Ability = t.Ability
	case *externalEventTask:
		w.Kind = "external_event"
		w.Event = checkpointEvent(t.Event)
	case *abilityOverlayExpiryTask:
		w.Kind = "ability_overlay_expiry"
		w.Owner = t.Owner
		w.Ability = t.Ability
		w.OverlayID = t.OverlayID
		w.Event = checkpointEvent(t.Context)
	default:
		return w, ErrCheckpointCorrupt
	}
	return w, nil
}

// restoreCompletedCastOrder rebuilds the completion queue from the checkpoint.
//
// The recorded order wins, filtered to the casts that actually came back
// terminal, so a stale or hand-edited order cannot resurrect or duplicate an
// entry. Anything terminal the order does not mention is appended in id order:
// that covers a checkpoint written before the field existed, and keeps the
// result deterministic either way.
func restoreCompletedCastOrder(recorded []CastID, completed map[CastID]bool, casts []checkpointCast) []CastID {
	order := make([]CastID, 0, len(completed))
	placed := make(map[CastID]bool, len(completed))
	for _, id := range recorded {
		if completed[id] && !placed[id] {
			order = append(order, id)
			placed[id] = true
		}
	}
	// p.Casts is serialized in id order, so this append is id-ordered too.
	for _, item := range casts {
		if completed[item.ID] && !placed[item.ID] {
			order = append(order, item.ID)
			placed[item.ID] = true
		}
	}
	return order
}

func (runtime *Runtime) restoreCheckpointPayload(p runtimeCheckpointPayload, resolver ProgramResolver) error {
	runtime.currentTick = p.CurrentTick
	runtime.eventCursor = p.EventCursor
	runtime.nextCastID = p.NextCastID
	runtime.nextTaskSequence = p.NextTaskSequence
	runtime.nextFrameID = p.NextFrameID
	runtime.nextSpawnID = p.NextSpawnID
	runtime.spawnEventSequence = p.SpawnEventSequence
	runtime.nextPassiveActivationID = p.NextPassiveActivation
	runtime.nextAbilityHandle = p.NextAbilityHandle
	runtime.nextAbilityOverlay = p.NextAbilityOverlay
	runtime.passiveCountTick = p.PassiveCountTick
	runtime.passiveCount = p.PassiveCount
	runtime.traceSequence = p.TraceSequence
	runtime.presentationSequence = p.PresentationSequence
	runtime.stateEventSequence = p.StateEventSequence
	runtime.stateEventDropped = p.StateEventDropped
	runtime.stateMutationSequence = p.StateMutationSequence
	runtime.stateMutationDropped = p.StateMutationDropped
	completed := make(map[CastID]bool, len(p.Casts))
	for _, item := range p.Casts {
		if item.ID == 0 || item.ID > p.NextCastID || runtime.casts[item.ID] != nil {
			return ErrCheckpointCorrupt
		}
		program, err := resolveCheckpointProgram(item.Program, resolver, p.Authority, p.SemanticsRevision)
		if err != nil {
			return err
		}
		inputs, err := restoreCheckpointValues(item.Inputs)
		if err != nil {
			return err
		}
		memory, err := restoreCheckpointValues(item.Memory)
		if err != nil {
			return err
		}
		locals, err := restoreCheckpointValues(item.Locals)
		if err != nil {
			return err
		}
		snapshots, err := restoreCheckpointValueMap(item.Snapshots)
		if err != nil {
			return err
		}
		random, err := restoreCheckpointRandom(item.RandomInvocations)
		if err != nil {
			return err
		}
		runtime.casts[item.ID] = &castInstance{id: item.ID, program: program, caster: item.Caster, primaryTarget: item.PrimaryTarget, inputs: inputs, memory: memory, locals: locals, snapshots: snapshots, status: item.Status, currentPhase: item.CurrentPhase, visibleRevision: item.VisibleRevision, failure: item.Failure, randomKey: item.RandomKey, randomInvocations: random, eventContext: restoreCheckpointEvent(item.EventContext), phaseToken: item.PhaseToken, pendingTasks: item.PendingTasks, logicalFinished: item.LogicalFinished, areaCallbackFinish: item.AreaCallbackFinish, windowStage: item.WindowStage, startTick: item.StartTick, committed: item.Committed, costsPaid: item.CostsPaid, cooldownStarted: item.CooldownStarted, pulseIndex: item.PulseIndex, releaseReason: item.ReleaseReason, stock: item.Stock, maxStock: item.MaxStock, windowStartTick: item.WindowStartTick, pendingRootEvent: item.PendingRootEvent, policyActive: item.PolicyActive, cooldownOwner: item.CooldownOwner, ability: item.Ability, abilityFinished: item.AbilityFinished}
		if item.Status == CastFinished || item.Status == CastFailed {
			completed[item.ID] = true
		} else {
			runtime.activeCastCount++
		}
	}
	runtime.completedCastOrder = restoreCompletedCastOrder(p.CompletedCastOrder, completed, p.Casts)
	if err := restoreCheckpointSpawns(&runtime.spawns, p.Spawns, resolver, p.Authority, p.SemanticsRevision, p.NextSpawnID); err != nil {
		return err
	}
	for _, frame := range p.Frames {
		if frame.ID == 0 || frame.ID > p.NextFrameID {
			return ErrCheckpointCorrupt
		}
		if _, ok := runtime.frames[frame.ID]; ok {
			return ErrCheckpointCorrupt
		}
		values, err := restoreCheckpointValues(frame.Values)
		if err != nil {
			return err
		}
		runtime.frames[frame.ID] = values
	}
	sequences := make(map[uint64]struct{}, len(p.Tasks))
	frameReferences := make(map[FrameID]struct{}, len(p.Frames))
	pendingByCast := make(map[CastID]int)
	runtime.scheduler = &scheduler{}
	for _, wire := range p.Tasks {
		if wire.DueTick < runtime.currentTick || wire.Sequence == 0 || wire.Sequence > p.NextTaskSequence {
			return ErrCheckpointCorrupt
		}
		if _, ok := sequences[wire.Sequence]; ok {
			return ErrCheckpointCorrupt
		}
		sequences[wire.Sequence] = struct{}{}
		task, err := runtime.restoreCheckpointTask(wire, resolver, p.Authority, p.SemanticsRevision)
		if err != nil {
			return err
		}
		runtime.scheduler.tasks = append(runtime.scheduler.tasks, task)
		if frame := task.Payload.frameID(); frame != 0 {
			if _, duplicate := frameReferences[frame]; duplicate {
				return ErrCheckpointCorrupt
			}
			frameReferences[frame] = struct{}{}
		}
		if castID, _ := scheduledTaskIdentity(task.Payload); castID != 0 {
			pendingByCast[castID]++
		}
	}
	if len(frameReferences) != len(runtime.frames) {
		return ErrCheckpointCorrupt
	}
	for id, cast := range runtime.casts {
		if cast.pendingTasks != pendingByCast[id] {
			return ErrCheckpointCorrupt
		}
	}
	heap.Init(&runtime.scheduler.tasks)
	for _, item := range p.Cooldowns {
		key := cooldownKey{Caster: item.Caster, Skill: item.Skill}
		if key.Caster == 0 || key.Skill == "" {
			return ErrCheckpointCorrupt
		}
		if _, ok := runtime.cooldowns[key]; ok {
			return ErrCheckpointCorrupt
		}
		runtime.cooldowns[key] = item.Due
	}
	for _, item := range p.SkillStates {
		key := skillStateKey{Caster: item.Caster, Skill: item.Skill}
		if key.Caster == 0 || key.Skill == "" || item.Stock < 0 || item.MaxStock < item.Stock {
			return ErrCheckpointCorrupt
		}
		if _, ok := runtime.skillStates[key]; ok {
			return ErrCheckpointCorrupt
		}
		runtime.skillStates[key] = &skillState{stock: item.Stock, maxStock: item.MaxStock, rechargeTicks: item.RechargeTicks, rechargeDue: item.RechargeDue, rechargeScheduled: item.RechargeScheduled, rechargeGeneration: item.RechargeGeneration}
	}
	for _, item := range p.ActivePolicies {
		key := skillStateKey{Caster: item.Caster, Skill: item.Skill}
		if runtime.casts[item.CastID] == nil {
			return ErrCheckpointCorrupt
		}
		if _, ok := runtime.activePolicies[key]; ok {
			return ErrCheckpointCorrupt
		}
		runtime.activePolicies[key] = item.CastID
	}
	for _, item := range p.ProcLedger {
		key := procLedgerKey{Root: item.Root, Caster: item.Caster, Digest: item.Digest}
		if _, ok := runtime.procLedger[key]; ok {
			return ErrCheckpointCorrupt
		}
		runtime.procLedger[key] = struct{}{}
	}
	for _, item := range p.RootEventCounts {
		if item.Count < 0 {
			return ErrCheckpointCorrupt
		}
		if _, ok := runtime.rootEventCounts[item.ID]; ok {
			return ErrCheckpointCorrupt
		}
		runtime.rootEventCounts[item.ID] = item.Count
		runtime.rootEventOrder = append(runtime.rootEventOrder, item.ID)
	}
	sort.Slice(runtime.rootEventOrder, func(i, j int) bool { return runtime.rootEventOrder[i] < runtime.rootEventOrder[j] })
	for _, item := range p.Abilities {
		key := abilityKey{owner: item.Owner, handle: item.Handle}
		if item.Owner == 0 || item.Handle == 0 || runtime.abilities[key] != nil {
			return ErrCheckpointCorrupt
		}
		program, err := resolveCheckpointProgram(item.Program, resolver, p.Authority, p.SemanticsRevision)
		if err != nil {
			return err
		}
		state := &abilityState{owner: item.Owner, handle: item.Handle, slot: item.Slot, tags: normalizeGameplayTagHandles(item.Tags), program: program, cooldownTotal: item.CooldownTotal, ammoStock: item.AmmoStock, ammoMax: item.AmmoMax, castActive: item.CastActive, lastCommitTick: item.LastCommitTick, lastFinishTick: item.LastFinishTick, overlays: make(map[uint64]Tick)}
		for _, overlay := range item.Overlays {
			if overlay.ID == 0 || overlay.ID > p.NextAbilityOverlay {
				return ErrCheckpointCorrupt
			}
			if _, ok := state.overlays[overlay.ID]; ok {
				return ErrCheckpointCorrupt
			}
			state.overlays[overlay.ID] = overlay.Due
		}
		runtime.abilities[key] = state
	}
	for _, item := range p.AbilityByProgram {
		key := skillStateKey{Caster: item.Caster, Skill: item.Skill}
		if _, ok := runtime.abilityByProgram[key]; ok || runtime.abilities[abilityKey{owner: item.Caster, handle: item.Handle}] == nil {
			return ErrCheckpointCorrupt
		}
		runtime.abilityByProgram[key] = item.Handle
	}
	if len(runtime.abilityByProgram) != len(runtime.abilities) {
		return ErrCheckpointCorrupt
	}
	if runtime.activeCastCount > runtime.options.MaxActiveCasts || len(runtime.abilities) > runtime.options.MaxAbilities || !runtime.completedCastOrderWithinLimitLocked() || len(runtime.rootEventCounts) > runtime.options.RootEventLimit || len(runtime.procLedger) > runtime.options.MaxProcLedgerEntries || !runtime.stopPendingRecordsValidLocked() {
		return ErrCheckpointCorrupt
	}
	for key, state := range runtime.abilities {
		if state == nil || runtime.abilityByProgram[skillStateKey{Caster: key.owner, Skill: state.program.id}] != key.handle {
			return ErrCheckpointCorrupt
		}
	}
	if !p.StateMutationReady || p.StateMutationBaseline.LatestStateMutationSequence != p.StateMutationSequence || p.StateMutationBaseline.Tick > p.CurrentTick || p.StateMutationBaseline.WorldRevision > p.WorldRevision {
		return ErrCheckpointCorrupt
	}
	if !runtimeSnapshotsEqual(p.StateMutationBaseline, runtime.stateSnapshotLocked()) {
		return ErrCheckpointHostMismatch
	}
	runtime.stateMutationBaseline = p.StateMutationBaseline
	runtime.stateMutationReady = p.StateMutationReady
	runtime.clearStateMutationWritePointsLocked()
	runtime.stateMutationDirty = false
	return nil
}

func runtimeSnapshotsEqual(left, right RuntimeStateSnapshot) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func checkpointRecordCount(payload runtimeCheckpointPayload) int {
	counts := []int{len(payload.Casts), len(payload.Spawns), len(payload.Frames), len(payload.Tasks), len(payload.Cooldowns), len(payload.SkillStates), len(payload.ActivePolicies), len(payload.ProcLedger), len(payload.RootEventCounts), len(payload.Abilities), len(payload.AbilityByProgram)}
	maximum := int(^uint(0) >> 1)
	total := 0
	for _, count := range counts {
		if count > maximum-total {
			return maximum
		}
		total += count
	}
	return total
}

func validCheckpointRuntimeLimits(payload runtimeCheckpointPayload) bool {
	return payload.SemanticsRevision != "" && payload.MaxPassivePerTick > 0 && payload.MaxOwned > 0 && payload.MaxOwnedPerOwner > 0 && payload.MaxOwnedPerProgram > 0 && payload.MaxOwnedPerTemplate > 0 && payload.MaxActiveCasts > 0 && payload.MaxAbilities > 0 && payload.CompletedCastLimit > 0 && payload.RootEventLimit > 0 && payload.MaxProcLedgerEntries > 0 && payload.SpawnStopRetryBackoff > 0 && payload.SpawnStopRetryLimit > 0 && payload.MaxStopPendingSpawns > 0 && payload.MaxAbandonedSpawns > 0
}

// restoreCheckpointSpawns 恢复衍生物记录，按字段放进分区（spawnTable.add）。分区由 status 与 handed_off 决定，
// 所以两者要合法：status 是已知值，只有 entity 衍生物会移交（handoffEntitySpawns）。已放弃的记录可能比所属 cast
// 活得久，checkpoint 只能从记录自带的 Program 解析程序，所以它必须带 direct_program（live Runtime 只放弃仍在运行、
// Program 未释放的 entity 衍生物）。已放弃的条数不按 MaxAbandonedSpawns 核对：两次 Advance 之间可以暂时超限，
// 恢复后的下一次 Advance 末尾照样清理。
func restoreCheckpointSpawns(table *spawnTable, values []checkpointSpawn, resolver ProgramResolver, authority AuthorityIdentity, semantics string, nextID SpawnID) error {
	for _, item := range values {
		if item.ID == 0 || item.ID > nextID || !validSpawnStatus(item.Status) || item.HandedOff && item.Scope != SpawnScopeEntity || item.Status == SpawnAbandoned && !item.DirectProgram {
			return ErrCheckpointCorrupt
		}
		program, err := resolveCheckpointProgram(item.Program, resolver, authority, semantics)
		if err != nil {
			return err
		}
		inputs, err := restoreCheckpointValues(item.Inputs)
		if err != nil {
			return err
		}
		memory, err := restoreCheckpointValues(item.Memory)
		if err != nil {
			return err
		}
		locals, err := restoreCheckpointValues(item.Locals)
		if err != nil {
			return err
		}
		snapshots, err := restoreCheckpointValueMap(item.Snapshots)
		if err != nil {
			return err
		}
		random, err := restoreCheckpointRandom(item.RandomInvocations)
		if err != nil {
			return err
		}
		numeric := SpawnNumericState{Initialized: item.Numeric.Initialized}
		for _, state := range item.Numeric.Properties {
			var track *numericTrackState
			if state.Track != nil {
				copy := *state.Track
				track = &copy
			}
			numeric.Properties = append(numeric.Properties, numericPropertyState{Property: state.Property, Base: state.Base, Current: state.Current, Track: track, Binding: spawnPropertySlotBindingProgram{stage: state.Stage, variant: state.Variant, field: state.Field}, Bound: state.Bound})
		}
		var directProgram *Program
		if item.DirectProgram {
			directProgram = program
		}
		spawn := &SpawnInstance{ID: item.ID, CastID: item.CastID, TemplateIndex: item.TemplateIndex, UnitTemplate: item.UnitTemplate, Status: item.Status, StartTick: item.StartTick, NextTick: item.NextTick, EndTick: item.EndTick, Scope: item.Scope, HostState: item.HostState, Motion: item.Motion, Numeric: numeric, Owner: item.Owner, LifecycleEntity: item.LifecycleEntity, Program: directProgram, inputs: inputs, memory: memory, locals: locals, snapshots: snapshots, randomKey: item.RandomKey, randomInvocations: random, visibleRevision: item.VisibleRevision, eventContext: restoreCheckpointEvent(item.EventContext), AreaMembers: make(map[EntityID]AreaMemberState), phaseToken: item.PhaseToken, stopCause: item.StopCause, handedOff: item.HandedOff, areaCallbackFinishedCast: item.AreaCallbackFinishedCast, stopRetryAttempts: item.StopRetryAttempts, stopRetryTick: item.StopRetryTick, stopRetryExhausted: item.StopRetryExhausted}
		for _, member := range item.AreaMembers {
			if member.Entity == 0 {
				return ErrCheckpointCorrupt
			}
			if _, ok := spawn.AreaMembers[member.Entity]; ok {
				return ErrCheckpointCorrupt
			}
			spawn.AreaMembers[member.Entity] = member.State
		}
		if !table.add(spawn) {
			return ErrCheckpointCorrupt
		}
	}
	return nil
}

func (runtime *Runtime) restoreCheckpointTask(w checkpointTask, resolver ProgramResolver, authority AuthorityIdentity, semantics string) (scheduledTask, error) {
	var payload scheduledTaskPayload
	switch w.Kind {
	case "flow":
		payload = &flowContinuationTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Operations: append([]OperationIndex(nil), w.Operations...)}
	case "repeat":
		payload = &repeatIterationTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Body: w.Body, IndexLocal: w.IndexLocal, Iteration: w.Iteration, Times: w.Times, Interval: w.Interval, Tail: append([]OperationIndex(nil), w.Tail...)}
	case "phase_timeout":
		// phase 计时保持方向 B（B3④）：timeout_ticks > 0 编译期拒绝，Runtime 从不调度这类任务，
		// 任务类型已删除。旧 checkpoint 里出现它说明来源不可信或格式不符，按 corrupt 拒绝、不迁移
		// （RR-20261006-03，O20）。
		return scheduledTask{}, ErrCheckpointCorrupt
	case "chain_hop":
		payload = &chainHopTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Operation: w.Operation, Hop: w.Hop}
	case "spawn_step":
		// 衍生物只由 advanceOwnedSpawns 按记录的 NextTick 推进（RR-20261006-51），这类任务已删除：它在生产路径上
		// 从未被创建，checkpoint 里出现它说明来源不可信或格式不符，按 corrupt 拒绝；接受它会让同一个衍生物在一个
		// tick 里走两步。
		return scheduledTask{}, ErrCheckpointCorrupt
	case "cast_commit":
		payload = &castCommitTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame}
	case "cast_execute":
		payload = &castExecuteTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame}
	case "cast_recovery":
		payload = &castRecoveryTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame}
	case "cast_pulse":
		payload = &castPulseTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, PulseIndex: w.PulseIndex}
	case "cast_auto_release":
		payload = &castAutoReleaseTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Reason: w.Reason}
	case "ammo_recharge":
		payload = &ammoRechargeTask{Caster: w.Caster, Skill: w.Skill, Generation: w.Generation}
	case "passive_activation":
		program, err := resolveCheckpointProgram(w.Program, resolver, authority, semantics)
		if err != nil {
			return scheduledTask{}, err
		}
		payload = &passiveActivationTask{ID: w.PassiveID, Program: program, Event: restoreCheckpointEvent(w.Event), Owner: w.Owner, Ability: w.Ability}
	case "external_event":
		payload = &externalEventTask{Event: restoreCheckpointEvent(w.Event)}
	case "ability_overlay_expiry":
		payload = &abilityOverlayExpiryTask{Owner: w.Owner, Ability: w.Ability, OverlayID: w.OverlayID, Context: restoreCheckpointEvent(w.Event)}
	default:
		return scheduledTask{}, ErrCheckpointCorrupt
	}
	frame := payload.frameID()
	if frame != 0 {
		if runtime.frames[frame] == nil || runtime.casts[w.CastID] == nil {
			return scheduledTask{}, ErrCheckpointCorrupt
		}
	}
	return scheduledTask{DueTick: w.DueTick, Sequence: w.Sequence, Payload: payload}, nil
}

// completedCastOrderWithinLimitLocked 核对恢复出的完成队列满足 pruneCompletedCastsLocked 的不变量：不超过
// CompletedCastLimit，或者超出部分全是仍被引用、不能回收的 cast（排程任务、policy、运行中或待停止的衍生物）。
// 之前直接要求不超过上限：被钉住的 cast 多于上限时（例如上限 3、4 个移交后仍在运行的召唤），live Runtime
// 合法保留着它们，checkpoint 却恢复不了（RR-20261006-30）。
func (runtime *Runtime) completedCastOrderWithinLimitLocked() bool {
	if len(runtime.completedCastOrder) <= runtime.options.CompletedCastLimit {
		return true
	}
	for _, id := range runtime.completedCastOrder {
		if runtime.castEvictableLocked(runtime.casts[id]) {
			return false
		}
	}
	return true
}

// stopPendingRecordsValidLocked 核对待停止重试状态：只有待停止分区的衍生物带重试字段，次数不超过上限，
// 到上限才算 exhausted，条目数不超过 MaxStopPendingSpawns。待停止的记录不会被当成已移交推进：分区由 status 决定。
func (runtime *Runtime) stopPendingRecordsValidLocked() bool {
	valid := true
	runtime.spawns.each(func(spawn *SpawnInstance) {
		if spawn.Status == SpawnStopPending {
			valid = valid && spawn.stopRetryAttempts >= 0 && spawn.stopRetryAttempts <= runtime.options.SpawnStopRetryLimit && spawn.stopRetryExhausted == (spawn.stopRetryAttempts == runtime.options.SpawnStopRetryLimit)
		} else {
			valid = valid && spawn.stopRetryAttempts == 0 && spawn.stopRetryTick == 0 && !spawn.stopRetryExhausted
		}
	})
	return valid && runtime.spawns.count(spawnStopPending) <= runtime.options.MaxStopPendingSpawns
}
