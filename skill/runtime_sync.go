package skill

import "sort"

type StateEvent struct {
	Sequence uint64       `json:"sequence"`
	Event    RuntimeEvent `json:"event"`
}

type StateEventBatch struct {
	Events         []StateEvent `json:"events,omitempty"`
	OldestSequence uint64       `json:"oldest_sequence"`
	LatestSequence uint64       `json:"latest_sequence"`
	CursorExpired  bool         `json:"cursor_expired"`
	More           bool         `json:"more"`
	Dropped        uint64       `json:"dropped"`
}

type CastStateSnapshot struct {
	ID              CastID          `json:"id"`
	ProgramID       string          `json:"program_id"`
	GameplayDigest  string          `json:"gameplay_digest"`
	Caster          EntityID        `json:"caster"`
	PrimaryTarget   EntityID        `json:"primary_target,omitempty"`
	Status          CastStatus      `json:"status"`
	CurrentPhase    PhaseIndex      `json:"current_phase"`
	VisibleRevision WorldRevision   `json:"visible_revision"`
	Failure         string          `json:"failure,omitempty"`
	WindowStage     CastWindowStage `json:"window_stage"`
	Committed       bool            `json:"committed"`
	StartTick       Tick            `json:"start_tick"`
	ElapsedTicks    Tick            `json:"elapsed_ticks"`
	PulseIndex      int64           `json:"pulse_index"`
	ReleaseReason   string          `json:"release_reason,omitempty"`
	Stock           int64           `json:"stock"`
	MaxStock        int64           `json:"max_stock"`
}

type CooldownStateSnapshot struct {
	Caster    EntityID `json:"caster"`
	ProgramID string   `json:"program_id"`
	DueTick   Tick     `json:"due_tick"`
	Remaining Tick     `json:"remaining"`
}

type SkillResourceSnapshot struct {
	Caster             EntityID `json:"caster"`
	ProgramID          string   `json:"program_id"`
	Stock              int64    `json:"stock"`
	MaxStock           int64    `json:"max_stock"`
	RechargeTicks      Tick     `json:"recharge_ticks"`
	RechargeDue        Tick     `json:"recharge_due"`
	RechargeScheduled  bool     `json:"recharge_scheduled"`
	RechargeGeneration uint64   `json:"recharge_generation"`
}

type AbilityOverlaySnapshot struct {
	ID      uint64 `json:"id"`
	DueTick Tick   `json:"due_tick"`
}

type AbilityStateSnapshot struct {
	Owner             EntityID                 `json:"owner"`
	Handle            AbilityHandle            `json:"handle"`
	Slot              int                      `json:"slot"`
	Tags              []GameplayTagHandle      `json:"tags,omitempty"`
	ProgramID         string                   `json:"program_id"`
	GameplayDigest    string                   `json:"gameplay_digest"`
	CooldownTotal     Tick                     `json:"cooldown_total"`
	CooldownRemaining Tick                     `json:"cooldown_remaining"`
	AmmoStock         int64                    `json:"ammo_stock"`
	AmmoMax           int64                    `json:"ammo_max"`
	CastActive        int                      `json:"cast_active"`
	LastCommitTick    Tick                     `json:"last_commit_tick"`
	LastFinishTick    Tick                     `json:"last_finish_tick"`
	Enabled           bool                     `json:"enabled"`
	Overlays          []AbilityOverlaySnapshot `json:"overlays,omitempty"`
}

type NumericPropertySnapshot struct {
	Property ProcessPropertyHandle `json:"property"`
	Base     int64                 `json:"base"`
	Current  int64                 `json:"current"`
	Tracking bool                  `json:"tracking"`
	Target   int64                 `json:"target,omitempty"`
	EndTick  Tick                  `json:"end_tick,omitempty"`
}

type ProcessStateSnapshot struct {
	ID              ProcessID                 `json:"id"`
	CastID          CastID                    `json:"cast_id"`
	TemplateIndex   ProcessTemplateIndex      `json:"template_index"`
	ProgramID       string                    `json:"program_id"`
	GameplayDigest  string                    `json:"gameplay_digest"`
	UnitTemplate    UnitTemplateHandle        `json:"unit_template"`
	Status          ProcessStatus             `json:"status"`
	Scope           ProcessScope              `json:"scope"`
	StartTick       Tick                      `json:"start_tick"`
	NextTick        Tick                      `json:"next_tick"`
	EndTick         Tick                      `json:"end_tick"`
	Owner           EntityID                  `json:"owner"`
	LifecycleEntity EntityID                  `json:"lifecycle_entity"`
	VisibleRevision WorldRevision             `json:"visible_revision"`
	HandedOff       bool                      `json:"handed_off"`
	Motion          MotionState               `json:"motion"`
	Numeric         []NumericPropertySnapshot `json:"numeric,omitempty"`
}

type ActivePolicySnapshot struct {
	Caster    EntityID `json:"caster"`
	ProgramID string   `json:"program_id"`
	CastID    CastID   `json:"cast_id"`
}

type PersistentStateSnapshot struct {
	Handle   StateHandle       `json:"handle"`
	Binding  StateScopeBinding `json:"binding"`
	Value    RuntimeValue      `json:"value"`
	DueTick  Tick              `json:"due_tick"`
	Sequence uint64            `json:"sequence"`
	ClearOn  []string          `json:"clear_on,omitempty"`
}

type RuntimeStateExtensionProvider interface {
	SkillPersistentStateSnapshot() []PersistentStateSnapshot
}

type RuntimeStateSnapshot struct {
	Tick                        Tick                      `json:"tick"`
	WorldRevision               WorldRevision             `json:"world_revision"`
	Casts                       []CastStateSnapshot       `json:"casts,omitempty"`
	Cooldowns                   []CooldownStateSnapshot   `json:"cooldowns,omitempty"`
	SkillResources              []SkillResourceSnapshot   `json:"skill_resources,omitempty"`
	Abilities                   []AbilityStateSnapshot    `json:"abilities,omitempty"`
	Processes                   []ProcessStateSnapshot    `json:"processes,omitempty"`
	ActivePolicies              []ActivePolicySnapshot    `json:"active_policies,omitempty"`
	PersistentStates            []PersistentStateSnapshot `json:"persistent_states,omitempty"`
	LatestStateEventSequence    uint64                    `json:"latest_state_event_sequence"`
	LatestStateMutationSequence uint64                    `json:"latest_state_mutation_sequence"`
	LatestPresentationSequence  uint64                    `json:"latest_presentation_sequence"`
}

func (runtime *Runtime) appendRuntimeEvent(event RuntimeEvent) {
	runtime.runtimeEvents = append(runtime.runtimeEvents, cloneRuntimeEvent(event))
	if overflow := len(runtime.runtimeEvents) - runtime.options.RuntimeEventLimit; overflow > 0 {
		copy(runtime.runtimeEvents, runtime.runtimeEvents[overflow:])
		runtime.runtimeEvents = runtime.runtimeEvents[:runtime.options.RuntimeEventLimit]
		runtime.runtimeEventDropped += uint64(overflow)
	}
	runtime.recordStateEvent(event)
}

func (runtime *Runtime) recordStateEvent(event RuntimeEvent) {
	runtime.stateEventSequence++
	runtime.stateEvents = append(runtime.stateEvents, StateEvent{Sequence: runtime.stateEventSequence, Event: cloneRuntimeEvent(event)})
	limit := runtime.options.StateEventLimit
	if overflow := len(runtime.stateEvents) - limit; overflow > 0 {
		copy(runtime.stateEvents, runtime.stateEvents[overflow:])
		runtime.stateEvents = runtime.stateEvents[:limit]
		runtime.stateEventDropped += uint64(overflow)
	}
}

func (runtime *Runtime) StateEvents(after uint64, limit int) StateEventBatch {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	batch := StateEventBatch{LatestSequence: runtime.stateEventSequence, Dropped: runtime.stateEventDropped}
	if len(runtime.stateEvents) == 0 {
		batch.OldestSequence = runtime.stateEventSequence + 1
		batch.CursorExpired = after < runtime.stateEventSequence
		return batch
	}
	batch.OldestSequence = runtime.stateEvents[0].Sequence
	if after+1 < batch.OldestSequence {
		batch.CursorExpired = true
		return batch
	}
	index := sort.Search(len(runtime.stateEvents), func(index int) bool { return runtime.stateEvents[index].Sequence > after })
	end := len(runtime.stateEvents)
	if limit > 0 && index+limit < end {
		end = index + limit
		batch.More = true
	}
	batch.Events = cloneStateEvents(runtime.stateEvents[index:end])
	return batch
}

func cloneStateEvents(events []StateEvent) []StateEvent {
	result := append([]StateEvent(nil), events...)
	for index := range result {
		result[index].Event = cloneRuntimeEvent(result[index].Event)
	}
	return result
}

func (runtime *Runtime) StateSnapshot() RuntimeStateSnapshot {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return runtime.stateSnapshotLocked()
}

func (runtime *Runtime) stateSnapshotLocked() RuntimeStateSnapshot {
	revision := WorldRevision(0)
	if runtime.host != nil {
		revision = runtime.host.CurrentRevision()
	}
	snapshot := RuntimeStateSnapshot{Tick: runtime.currentTick, WorldRevision: revision, LatestStateEventSequence: runtime.stateEventSequence, LatestStateMutationSequence: runtime.stateMutationSequence, LatestPresentationSequence: runtime.presentationSequence,
		Casts: runtime.castsSnapshotLocked(), Cooldowns: runtime.cooldownsSnapshotLocked(), SkillResources: runtime.skillResourcesSnapshotLocked(), Abilities: runtime.abilitiesSnapshotLocked(), Processes: runtime.processesSnapshotLocked(), ActivePolicies: runtime.activePoliciesSnapshotLocked()}
	snapshot.PersistentStates = runtime.persistentStatesSnapshotLocked()
	return snapshot
}

// Per-domain snapshot builders. These are the single source of truth for
// snapshot entry construction: the full snapshot above and the incremental
// mutation commit (runtime_mutation.go) both build entries here, so the
// incremental baseline stays byte-identical with a full snapshot.

func (runtime *Runtime) castsSnapshotLocked() []CastStateSnapshot {
	result := make([]CastStateSnapshot, 0, len(runtime.casts))
	castIDs := make([]int, 0, len(runtime.casts))
	for id := range runtime.casts {
		castIDs = append(castIDs, int(id))
	}
	sort.Ints(castIDs)
	for _, rawID := range castIDs {
		cast := runtime.casts[CastID(rawID)]
		result = append(result, CastStateSnapshot{ID: cast.id, ProgramID: cast.program.id, GameplayDigest: cast.program.identity.gameplayDigest, Caster: cast.caster, PrimaryTarget: cast.primaryTarget, Status: cast.status, CurrentPhase: cast.currentPhase, VisibleRevision: cast.visibleRevision, Failure: cast.failure, WindowStage: cast.windowStage, Committed: cast.committed, StartTick: cast.startTick, ElapsedTicks: runtime.currentTick - cast.startTick, PulseIndex: cast.pulseIndex, ReleaseReason: cast.releaseReason, Stock: cast.stock, MaxStock: cast.maxStock})
	}
	return result
}

func (runtime *Runtime) cooldownSnapshotLocked(key cooldownKey, due Tick) CooldownStateSnapshot {
	remaining := due - runtime.currentTick
	if remaining < 0 {
		remaining = 0
	}
	return CooldownStateSnapshot{Caster: key.Caster, ProgramID: key.Skill, DueTick: due, Remaining: remaining}
}

func (runtime *Runtime) cooldownsSnapshotLocked() []CooldownStateSnapshot {
	result := make([]CooldownStateSnapshot, 0, len(runtime.cooldowns))
	for key, due := range runtime.cooldowns {
		result = append(result, runtime.cooldownSnapshotLocked(key, due))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Caster != result[j].Caster {
			return result[i].Caster < result[j].Caster
		}
		return result[i].ProgramID < result[j].ProgramID
	})
	return result
}

func skillResourceSnapshot(key skillStateKey, state *skillState) SkillResourceSnapshot {
	return SkillResourceSnapshot{Caster: key.Caster, ProgramID: key.Skill, Stock: state.stock, MaxStock: state.maxStock, RechargeTicks: state.rechargeTicks, RechargeDue: state.rechargeDue, RechargeScheduled: state.rechargeScheduled, RechargeGeneration: state.rechargeGeneration}
}

func (runtime *Runtime) skillResourcesSnapshotLocked() []SkillResourceSnapshot {
	result := make([]SkillResourceSnapshot, 0, len(runtime.skillStates))
	for key, state := range runtime.skillStates {
		result = append(result, skillResourceSnapshot(key, state))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Caster != result[j].Caster {
			return result[i].Caster < result[j].Caster
		}
		return result[i].ProgramID < result[j].ProgramID
	})
	return result
}

func (runtime *Runtime) abilitySnapshotLocked(state *abilityState) AbilityStateSnapshot {
	remaining := runtime.cooldowns[cooldownKey{Caster: state.owner, Skill: state.program.id}] - runtime.currentTick
	if remaining < 0 {
		remaining = 0
	}
	ability := AbilityStateSnapshot{Owner: state.owner, Handle: state.handle, Slot: state.slot, Tags: append([]GameplayTagHandle(nil), state.tags...), ProgramID: state.program.id, GameplayDigest: state.program.identity.gameplayDigest, CooldownTotal: state.cooldownTotal, CooldownRemaining: remaining, AmmoStock: state.ammoStock, AmmoMax: state.ammoMax, CastActive: state.castActive, LastCommitTick: state.lastCommitTick, LastFinishTick: state.lastFinishTick, Enabled: len(state.overlays) == 0}
	for id, due := range state.overlays {
		ability.Overlays = append(ability.Overlays, AbilityOverlaySnapshot{ID: id, DueTick: due})
	}
	sort.Slice(ability.Overlays, func(i, j int) bool { return ability.Overlays[i].ID < ability.Overlays[j].ID })
	return ability
}

func (runtime *Runtime) abilitiesSnapshotLocked() []AbilityStateSnapshot {
	result := make([]AbilityStateSnapshot, 0, len(runtime.abilities))
	for _, state := range runtime.abilities {
		result = append(result, runtime.abilitySnapshotLocked(state))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		return result[i].Handle < result[j].Handle
	})
	return result
}

func (runtime *Runtime) processesSnapshotLocked() []ProcessStateSnapshot {
	result := make([]ProcessStateSnapshot, 0, len(runtime.processes))
	processIDs := make([]int, 0, len(runtime.processes))
	for id := range runtime.processes {
		processIDs = append(processIDs, int(id))
	}
	sort.Ints(processIDs)
	for _, rawID := range processIDs {
		process := runtime.processes[ProcessID(rawID)]
		programID, digest := "", ""
		if process.Program != nil {
			programID, digest = process.Program.id, process.Program.identity.gameplayDigest
		}
		view := ProcessStateSnapshot{ID: process.ID, CastID: process.CastID, TemplateIndex: process.TemplateIndex, ProgramID: programID, GameplayDigest: digest, UnitTemplate: process.UnitTemplate, Status: process.Status, Scope: process.Scope, StartTick: process.StartTick, NextTick: process.NextTick, EndTick: process.EndTick, Owner: process.Owner, LifecycleEntity: process.LifecycleEntity, VisibleRevision: process.visibleRevision, HandedOff: process.handedOff, Motion: process.Motion}
		for _, property := range process.Numeric.Properties {
			item := NumericPropertySnapshot{Property: property.Property, Base: property.Base, Current: property.Current}
			if property.Track != nil {
				item.Tracking = true
				item.Target = property.Track.Target
				item.EndTick = property.Track.StartTick + property.Track.OverTicks
			}
			view.Numeric = append(view.Numeric, item)
		}
		result = append(result, view)
	}
	return result
}

func (runtime *Runtime) activePoliciesSnapshotLocked() []ActivePolicySnapshot {
	result := make([]ActivePolicySnapshot, 0, len(runtime.activePolicies))
	for key, castID := range runtime.activePolicies {
		result = append(result, ActivePolicySnapshot{Caster: key.Caster, ProgramID: key.Skill, CastID: castID})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Caster != result[j].Caster {
			return result[i].Caster < result[j].Caster
		}
		return result[i].ProgramID < result[j].ProgramID
	})
	return result
}

func (runtime *Runtime) persistentStatesSnapshotLocked() []PersistentStateSnapshot {
	provider, ok := runtime.host.(RuntimeStateExtensionProvider)
	if !ok {
		return nil
	}
	states := provider.SkillPersistentStateSnapshot()
	result := append([]PersistentStateSnapshot(nil), states...)
	for index := range result {
		result[index].Value = cloneStateRuntimeValue(result[index].Value)
		result[index].ClearOn = append([]string(nil), result[index].ClearOn...)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Sequence != result[j].Sequence {
			return result[i].Sequence < result[j].Sequence
		}
		leftHandle, rightHandle := result[i].Handle, result[j].Handle
		if leftHandle.GameplayDigest != rightHandle.GameplayDigest {
			return leftHandle.GameplayDigest < rightHandle.GameplayDigest
		}
		if leftHandle.Slot != rightHandle.Slot {
			return leftHandle.Slot < rightHandle.Slot
		}
		if leftHandle.Shared != rightHandle.Shared {
			return leftHandle.Shared < rightHandle.Shared
		}
		left, right := result[i].Binding, result[j].Binding
		if left.Owner != right.Owner {
			return left.Owner < right.Owner
		}
		if left.Subject != right.Subject {
			return left.Subject < right.Subject
		}
		return left.Team < right.Team
	})
	return result
}
