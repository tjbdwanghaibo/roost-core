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
	"reflect"
	"sort"
)

const RuntimeCheckpointVersion uint32 = 2
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
	WorldRevision         WorldRevision             `json:"world_revision"`
	Authority             AuthorityIdentity         `json:"authority"`
	MatchSeed             [32]byte                  `json:"match_seed"`
	SemanticsRevision     string                    `json:"semantics_revision"`
	MaxPassivePerTick     int                       `json:"max_passive_per_tick"`
	MaxOwned              int                       `json:"max_owned"`
	MaxOwnedPerOwner      int                       `json:"max_owned_per_owner"`
	MaxOwnedPerProgram    int                       `json:"max_owned_per_program"`
	MaxOwnedPerTemplate   int                       `json:"max_owned_per_template"`
	MaxActiveCasts        int                       `json:"max_active_casts"`
	MaxAbilities          int                       `json:"max_abilities"`
	CompletedCastLimit    int                       `json:"completed_cast_limit"`
	RootEventLimit        int                       `json:"root_event_limit"`
	MaxProcLedgerEntries  int                       `json:"max_proc_ledger_entries"`
	CurrentTick           Tick                      `json:"current_tick"`
	EventCursor           EventCursor               `json:"event_cursor"`
	NextCastID            CastID                    `json:"next_cast_id"`
	NextTaskSequence      uint64                    `json:"next_task_sequence"`
	NextFrameID           FrameID                   `json:"next_frame_id"`
	NextProcessID         ProcessID                 `json:"next_process_id"`
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
	Processes             []checkpointProcess       `json:"processes"`
	OwnedProcesses        []checkpointProcess       `json:"owned_processes"`
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

type checkpointProcess struct {
	ID                       ProcessID                    `json:"id"`
	CastID                   CastID                       `json:"cast_id"`
	TemplateIndex            ProcessTemplateIndex         `json:"template_index"`
	UnitTemplate             UnitTemplateHandle           `json:"unit_template"`
	Status                   ProcessStatus                `json:"status"`
	StartTick                Tick                         `json:"start_tick"`
	NextTick                 Tick                         `json:"next_tick"`
	EndTick                  Tick                         `json:"end_tick"`
	Scope                    ProcessScope                 `json:"scope"`
	HostState                ProcessHostState             `json:"host_state"`
	Motion                   MotionState                  `json:"motion"`
	Numeric                  checkpointProcessNumeric     `json:"numeric"`
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
}

type checkpointProcessNumeric struct {
	Initialized bool                        `json:"initialized"`
	Properties  []checkpointNumericProperty `json:"properties"`
}

type checkpointNumericProperty struct {
	Property ProcessPropertyHandle      `json:"property"`
	Base     int64                      `json:"base"`
	Current  int64                      `json:"current"`
	Track    *numericTrackState         `json:"track,omitempty"`
	Stage    processPropertySlotStage   `json:"stage"`
	Variant  processPropertySlotVariant `json:"variant"`
	Field    processPropertySlotField   `json:"field"`
	Bound    bool                       `json:"bound"`
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
	Process      ProcessID                    `json:"process,omitempty"`
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
	ProcessID  ProcessID              `json:"process_id,omitempty"`
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
	options.MaxOwnedProcesses = payload.MaxOwned
	options.MaxOwnedProcessesPerOwner = payload.MaxOwnedPerOwner
	options.MaxOwnedProcessesPerProgram = payload.MaxOwnedPerProgram
	options.MaxOwnedProcessesPerTemplate = payload.MaxOwnedPerTemplate
	options.MaxActiveCasts = payload.MaxActiveCasts
	options.MaxAbilities = payload.MaxAbilities
	options.CompletedCastLimit = payload.CompletedCastLimit
	options.RootEventLimit = payload.RootEventLimit
	options.MaxProcLedgerEntries = payload.MaxProcLedgerEntries
	// newRuntimeCore, not NewRuntime: the fresh-runtime path fast-forwards
	// the event cursor to the host's frontier and compacts everything before
	// it — which would DELETE the events emitted between the checkpoint and
	// the crash before restoreCheckpointPayload rewinds the cursor to the
	// checkpoint value. Those events are exactly what a restored runtime must
	// replay. HostEventCompactor implementations must therefore retain all
	// events since the last successful checkpoint.
	runtime := newRuntimeCore(host, options)
	if err := runtime.restoreCheckpointPayload(payload, resolver); err != nil {
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
	result := checkpointRuntimeValue{Present: value.present, Type: value.typ, Integer: value.integer, Boolean: value.boolean, Text: value.text, Entity: value.entity, Position: value.position, Direction: value.direction, Hit: value.hit, Path: append([]Position(nil), value.path...), Ability: value.ability, Status: value.status, Entities: append([]EntityID(nil), value.entities...), Strings: append([]string(nil), value.strings...), Snapshot: value.snapshot.opaque, Process: value.process}
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
	result := RuntimeValue{present: value.Present, typ: value.Type, integer: value.Integer, boolean: value.Boolean, text: value.Text, entity: value.Entity, position: value.Position, direction: value.Direction, hit: value.Hit, path: append([]Position(nil), value.Path...), ability: value.Ability, status: value.Status, entities: append([]EntityID(nil), value.Entities...), strings: append([]string(nil), value.Strings...), snapshot: SnapshotToken{opaque: value.Snapshot}, process: value.Process}
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
	p := runtimeCheckpointPayload{WorldRevision: runtime.host.CurrentRevision(), Authority: runtime.host.AuthorityIdentity(), MatchSeed: runtime.options.MatchSeed, SemanticsRevision: runtime.options.SupportedCompilerSemanticsRevision, MaxPassivePerTick: runtime.options.MaxPassiveActivationsPerTick, MaxOwned: runtime.options.MaxOwnedProcesses, MaxOwnedPerOwner: runtime.options.MaxOwnedProcessesPerOwner, MaxOwnedPerProgram: runtime.options.MaxOwnedProcessesPerProgram, MaxOwnedPerTemplate: runtime.options.MaxOwnedProcessesPerTemplate, MaxActiveCasts: runtime.options.MaxActiveCasts, MaxAbilities: runtime.options.MaxAbilities, CompletedCastLimit: runtime.options.CompletedCastLimit, RootEventLimit: runtime.options.RootEventLimit, MaxProcLedgerEntries: runtime.options.MaxProcLedgerEntries, CurrentTick: runtime.currentTick, EventCursor: runtime.eventCursor, NextCastID: runtime.nextCastID, NextTaskSequence: runtime.nextTaskSequence, NextFrameID: runtime.nextFrameID, NextProcessID: runtime.nextProcessID, NextPassiveActivation: runtime.nextPassiveActivationID, NextAbilityHandle: runtime.nextAbilityHandle, NextAbilityOverlay: runtime.nextAbilityOverlay, PassiveCountTick: runtime.passiveCountTick, PassiveCount: runtime.passiveCount, TraceSequence: runtime.traceSequence, PresentationSequence: runtime.presentationSequence, StateEventSequence: runtime.stateEventSequence, StateEventDropped: runtime.stateEventDropped, StateMutationSequence: runtime.stateMutationSequence, StateMutationDropped: runtime.stateMutationDropped, StateMutationBaseline: runtime.stateMutationBaseline, StateMutationReady: runtime.stateMutationReady}
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
	resolveProcessProgram := func(process *ProcessInstance) *Program {
		if process == nil {
			return nil
		}
		if process.Program != nil {
			return process.Program
		}
		if cast := runtime.casts[process.CastID]; cast != nil {
			return cast.program
		}
		return nil
	}
	p.Processes, err = checkpointProcessMap(runtime.processes, resolveProcessProgram)
	if err != nil {
		return p, err
	}
	p.OwnedProcesses, err = checkpointProcessMap(runtime.ownedProcesses, resolveProcessProgram)
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
	for key, id := range runtime.activePolicies {
		p.ActivePolicies = append(p.ActivePolicies, checkpointActivePolicy{Caster: key.Caster, Skill: key.Skill, CastID: id})
	}
	for key := range runtime.procLedger {
		p.ProcLedger = append(p.ProcLedger, checkpointProcLedger{Root: key.Root, Caster: key.Caster, Digest: key.Digest})
	}
	for id, count := range runtime.rootEventCounts {
		p.RootEventCounts = append(p.RootEventCounts, checkpointRootEvent{ID: id, Count: count})
	}
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
	return p, nil
}

func checkpointProcessMap(values map[ProcessID]*ProcessInstance, programFor func(*ProcessInstance) *Program) ([]checkpointProcess, error) {
	ids := make([]int, 0, len(values))
	for id := range values {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	result := make([]checkpointProcess, 0, len(ids))
	for _, raw := range ids {
		process := values[ProcessID(raw)]
		program := programFor(process)
		if process == nil || program == nil {
			return nil, ErrCheckpointCorrupt
		}
		numeric := checkpointProcessNumeric{Initialized: process.Numeric.Initialized}
		for _, state := range process.Numeric.Properties {
			var track *numericTrackState
			if state.Track != nil {
				copy := *state.Track
				track = &copy
			}
			numeric.Properties = append(numeric.Properties, checkpointNumericProperty{Property: state.Property, Base: state.Base, Current: state.Current, Track: track, Stage: state.Binding.stage, Variant: state.Binding.variant, Field: state.Binding.field, Bound: state.Bound})
		}
		item := checkpointProcess{ID: process.ID, CastID: process.CastID, TemplateIndex: process.TemplateIndex, UnitTemplate: process.UnitTemplate, Status: process.Status, StartTick: process.StartTick, NextTick: process.NextTick, EndTick: process.EndTick, Scope: process.Scope, HostState: process.HostState, Motion: process.Motion, Numeric: numeric, Owner: process.Owner, LifecycleEntity: process.LifecycleEntity, Program: programCheckpointRef(program), DirectProgram: process.Program != nil, Inputs: checkpointValues(process.inputs), Memory: checkpointValues(process.memory), Locals: checkpointValues(process.locals), Snapshots: checkpointValueMap(process.snapshots), RandomKey: process.randomKey, RandomInvocations: checkpointRandom(process.randomInvocations), VisibleRevision: process.visibleRevision, EventContext: checkpointEvent(process.eventContext), PhaseToken: process.phaseToken, StopCause: process.stopCause, HandedOff: process.handedOff, AreaCallbackFinishedCast: process.areaCallbackFinishedCast}
		entities := make([]int, 0, len(process.AreaMembers))
		for entity := range process.AreaMembers {
			entities = append(entities, int(entity))
		}
		sort.Ints(entities)
		for _, entity := range entities {
			item.AreaMembers = append(item.AreaMembers, checkpointAreaMember{Entity: EntityID(entity), State: process.AreaMembers[EntityID(entity)]})
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
	case *phaseTimeoutTask:
		w.Kind = "phase_timeout"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
	case *chainHopTask:
		w.Kind = "chain_hop"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.Operation = t.Operation
		w.Hop = t.Hop
	case *processStepTask:
		w.Kind = "process_step"
		w.CastID = t.CastID
		w.PhaseToken = t.PhaseToken
		w.Frame = t.Frame
		w.ProcessID = t.ProcessID
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

func (runtime *Runtime) restoreCheckpointPayload(p runtimeCheckpointPayload, resolver ProgramResolver) error {
	runtime.currentTick = p.CurrentTick
	runtime.eventCursor = p.EventCursor
	runtime.nextCastID = p.NextCastID
	runtime.nextTaskSequence = p.NextTaskSequence
	runtime.nextFrameID = p.NextFrameID
	runtime.nextProcessID = p.NextProcessID
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
			runtime.completedCastOrder = append(runtime.completedCastOrder, item.ID)
		} else {
			runtime.activeCastCount++
		}
	}
	var err error
	runtime.processes, err = restoreCheckpointProcesses(p.Processes, resolver, p.Authority, p.SemanticsRevision, p.NextProcessID)
	if err != nil {
		return err
	}
	runtime.ownedProcesses, err = restoreCheckpointProcesses(p.OwnedProcesses, resolver, p.Authority, p.SemanticsRevision, p.NextProcessID)
	if err != nil {
		return err
	}
	ownedWire := make(map[ProcessID]checkpointProcess, len(p.OwnedProcesses))
	for _, item := range p.OwnedProcesses {
		ownedWire[item.ID] = item
	}
	for _, item := range p.Processes {
		owned, shared := runtime.ownedProcesses[item.ID]
		if !shared {
			continue
		}
		if owned == nil || !reflect.DeepEqual(item, ownedWire[item.ID]) {
			return ErrCheckpointCorrupt
		}
		runtime.ownedProcesses[item.ID] = runtime.processes[item.ID]
		delete(ownedWire, item.ID)
	}
	if len(ownedWire) != 0 {
		return ErrCheckpointCorrupt
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
	if runtime.activeCastCount > runtime.options.MaxActiveCasts || len(runtime.abilities) > runtime.options.MaxAbilities || len(runtime.completedCastOrder) > runtime.options.CompletedCastLimit || len(runtime.rootEventCounts) > runtime.options.RootEventLimit || len(runtime.procLedger) > runtime.options.MaxProcLedgerEntries {
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
	counts := []int{len(payload.Casts), len(payload.Processes), len(payload.OwnedProcesses), len(payload.Frames), len(payload.Tasks), len(payload.Cooldowns), len(payload.SkillStates), len(payload.ActivePolicies), len(payload.ProcLedger), len(payload.RootEventCounts), len(payload.Abilities), len(payload.AbilityByProgram)}
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
	return payload.SemanticsRevision != "" && payload.MaxPassivePerTick > 0 && payload.MaxOwned > 0 && payload.MaxOwnedPerOwner > 0 && payload.MaxOwnedPerProgram > 0 && payload.MaxOwnedPerTemplate > 0 && payload.MaxActiveCasts > 0 && payload.MaxAbilities > 0 && payload.CompletedCastLimit > 0 && payload.RootEventLimit > 0 && payload.MaxProcLedgerEntries > 0
}

func restoreCheckpointProcesses(values []checkpointProcess, resolver ProgramResolver, authority AuthorityIdentity, semantics string, nextID ProcessID) (map[ProcessID]*ProcessInstance, error) {
	result := make(map[ProcessID]*ProcessInstance, len(values))
	for _, item := range values {
		if item.ID == 0 || item.ID > nextID || result[item.ID] != nil {
			return nil, ErrCheckpointCorrupt
		}
		program, err := resolveCheckpointProgram(item.Program, resolver, authority, semantics)
		if err != nil {
			return nil, err
		}
		inputs, err := restoreCheckpointValues(item.Inputs)
		if err != nil {
			return nil, err
		}
		memory, err := restoreCheckpointValues(item.Memory)
		if err != nil {
			return nil, err
		}
		locals, err := restoreCheckpointValues(item.Locals)
		if err != nil {
			return nil, err
		}
		snapshots, err := restoreCheckpointValueMap(item.Snapshots)
		if err != nil {
			return nil, err
		}
		random, err := restoreCheckpointRandom(item.RandomInvocations)
		if err != nil {
			return nil, err
		}
		numeric := ProcessNumericState{Initialized: item.Numeric.Initialized}
		for _, state := range item.Numeric.Properties {
			var track *numericTrackState
			if state.Track != nil {
				copy := *state.Track
				track = &copy
			}
			numeric.Properties = append(numeric.Properties, numericPropertyState{Property: state.Property, Base: state.Base, Current: state.Current, Track: track, Binding: processPropertySlotBindingProgram{stage: state.Stage, variant: state.Variant, field: state.Field}, Bound: state.Bound})
		}
		var directProgram *Program
		if item.DirectProgram {
			directProgram = program
		}
		process := &ProcessInstance{ID: item.ID, CastID: item.CastID, TemplateIndex: item.TemplateIndex, UnitTemplate: item.UnitTemplate, Status: item.Status, StartTick: item.StartTick, NextTick: item.NextTick, EndTick: item.EndTick, Scope: item.Scope, HostState: item.HostState, Motion: item.Motion, Numeric: numeric, Owner: item.Owner, LifecycleEntity: item.LifecycleEntity, Program: directProgram, inputs: inputs, memory: memory, locals: locals, snapshots: snapshots, randomKey: item.RandomKey, randomInvocations: random, visibleRevision: item.VisibleRevision, eventContext: restoreCheckpointEvent(item.EventContext), AreaMembers: make(map[EntityID]AreaMemberState), phaseToken: item.PhaseToken, stopCause: item.StopCause, handedOff: item.HandedOff, areaCallbackFinishedCast: item.AreaCallbackFinishedCast}
		for _, member := range item.AreaMembers {
			if member.Entity == 0 {
				return nil, ErrCheckpointCorrupt
			}
			if _, ok := process.AreaMembers[member.Entity]; ok {
				return nil, ErrCheckpointCorrupt
			}
			process.AreaMembers[member.Entity] = member.State
		}
		result[item.ID] = process
	}
	return result, nil
}

func (runtime *Runtime) restoreCheckpointTask(w checkpointTask, resolver ProgramResolver, authority AuthorityIdentity, semantics string) (scheduledTask, error) {
	var payload scheduledTaskPayload
	switch w.Kind {
	case "flow":
		payload = &flowContinuationTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Operations: append([]OperationIndex(nil), w.Operations...)}
	case "repeat":
		payload = &repeatIterationTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Body: w.Body, IndexLocal: w.IndexLocal, Iteration: w.Iteration, Times: w.Times, Interval: w.Interval, Tail: append([]OperationIndex(nil), w.Tail...)}
	case "phase_timeout":
		payload = &phaseTimeoutTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame}
	case "chain_hop":
		payload = &chainHopTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, Operation: w.Operation, Hop: w.Hop}
	case "process_step":
		payload = &processStepTask{CastID: w.CastID, PhaseToken: w.PhaseToken, Frame: w.Frame, ProcessID: w.ProcessID}
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
