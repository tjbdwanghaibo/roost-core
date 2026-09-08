package skill

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
)

var ErrStateMutationInvalid = errors.New("skill: state mutation is invalid")

type StateMutationKind string

const (
	StateMutationClock            StateMutationKind = "clock"
	StateMutationCastUpsert       StateMutationKind = "cast_upsert"
	StateMutationCastRemove       StateMutationKind = "cast_remove"
	StateMutationCooldownUpsert   StateMutationKind = "cooldown_upsert"
	StateMutationCooldownRemove   StateMutationKind = "cooldown_remove"
	StateMutationResourceUpsert   StateMutationKind = "resource_upsert"
	StateMutationResourceRemove   StateMutationKind = "resource_remove"
	StateMutationAbilityUpsert    StateMutationKind = "ability_upsert"
	StateMutationAbilityRemove    StateMutationKind = "ability_remove"
	StateMutationProcessUpsert    StateMutationKind = "process_upsert"
	StateMutationProcessRemove    StateMutationKind = "process_remove"
	StateMutationPolicyUpsert     StateMutationKind = "policy_upsert"
	StateMutationPolicyRemove     StateMutationKind = "policy_remove"
	StateMutationPersistentUpsert StateMutationKind = "persistent_upsert"
	StateMutationPersistentRemove StateMutationKind = "persistent_remove"
)

// StateMutation is a closed, canonical patch union. Folding these mutations
// over a RuntimeStateSnapshot must reproduce a later snapshot exactly.
type StateMutation struct {
	Sequence                   uint64                   `json:"sequence"`
	Tick                       Tick                     `json:"tick"`
	WorldRevision              WorldRevision            `json:"world_revision"`
	LatestStateEventSequence   uint64                   `json:"latest_state_event_sequence"`
	LatestPresentationSequence uint64                   `json:"latest_presentation_sequence"`
	Kind                       StateMutationKind        `json:"kind"`
	CastID                     CastID                   `json:"cast_id,omitempty"`
	Caster                     EntityID                 `json:"caster,omitempty"`
	Owner                      EntityID                 `json:"owner,omitempty"`
	ProgramID                  string                   `json:"program_id,omitempty"`
	AbilityHandle              AbilityHandle            `json:"ability_handle,omitempty"`
	ProcessID                  ProcessID                `json:"process_id,omitempty"`
	StateHandle                StateHandle              `json:"state_handle,omitempty"`
	Binding                    StateScopeBinding        `json:"binding,omitempty"`
	Cast                       *CastStateSnapshot       `json:"cast,omitempty"`
	Cooldown                   *CooldownStateSnapshot   `json:"cooldown,omitempty"`
	Resource                   *SkillResourceSnapshot   `json:"resource,omitempty"`
	Ability                    *AbilityStateSnapshot    `json:"ability,omitempty"`
	Process                    *ProcessStateSnapshot    `json:"process,omitempty"`
	Policy                     *ActivePolicySnapshot    `json:"policy,omitempty"`
	Persistent                 *PersistentStateSnapshot `json:"persistent,omitempty"`
}

type StateMutationBatch struct {
	Mutations      []StateMutation `json:"mutations,omitempty"`
	OldestSequence uint64          `json:"oldest_sequence"`
	LatestSequence uint64          `json:"latest_sequence"`
	CursorExpired  bool            `json:"cursor_expired"`
	More           bool            `json:"more"`
	Dropped        uint64          `json:"dropped"`
}

func (runtime *Runtime) StateDeltas(after uint64, limit int) StateMutationBatch {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	batch := StateMutationBatch{LatestSequence: runtime.stateMutationSequence, Dropped: runtime.stateMutationDropped}
	if len(runtime.stateMutations) == 0 {
		batch.OldestSequence = runtime.stateMutationSequence + 1
		batch.CursorExpired = after < runtime.stateMutationSequence
		return batch
	}
	batch.OldestSequence = runtime.stateMutations[0].Sequence
	if after+1 < batch.OldestSequence {
		batch.CursorExpired = true
		return batch
	}
	index := sort.Search(len(runtime.stateMutations), func(index int) bool { return runtime.stateMutations[index].Sequence > after })
	end := len(runtime.stateMutations)
	if limit > 0 && index+limit < end {
		end = index + limit
		batch.More = true
	}
	batch.Mutations = cloneStateMutations(runtime.stateMutations[index:end])
	return batch
}

// stateMutationVerifyIncremental, when set (tests only), replays every
// incremental commit against the reference full-snapshot diff and panics on
// any divergence — the equivalence gate for the write-point fast path.
var stateMutationVerifyIncremental bool

func (runtime *Runtime) commitStateMutationsLocked() {
	if !runtime.stateMutationDirty {
		return
	}
	runtime.pruneCompletedCastsLocked()
	runtime.stateMutationDirty = false
	if !runtime.stateMutationReady || runtime.stateMutationAllDirty {
		current := runtime.stateSnapshotLocked()
		runtime.clearStateMutationWritePointsLocked()
		if !runtime.stateMutationReady {
			runtime.stateMutationBaseline = current
			runtime.stateMutationReady = true
			return
		}
		mutations := diffRuntimeState(runtime.stateMutationBaseline, current)
		runtime.appendStateMutationsLocked(mutations, current.Tick, current.WorldRevision, current.LatestStateEventSequence, current.LatestPresentationSequence)
		current.LatestStateMutationSequence = runtime.stateMutationSequence
		runtime.stateMutationBaseline = current
		return
	}
	var referenceBaseline RuntimeStateSnapshot
	if stateMutationVerifyIncremental {
		referenceBaseline = cloneSnapshotDomains(runtime.stateMutationBaseline)
	}
	revision := WorldRevision(0)
	if runtime.host != nil {
		revision = runtime.host.CurrentRevision()
	}
	mutations := runtime.diffWritePointsLocked(revision)
	runtime.appendStateMutationsLocked(mutations, runtime.currentTick, revision, runtime.stateEventSequence, runtime.presentationSequence)
	baseline := &runtime.stateMutationBaseline
	baseline.Tick, baseline.WorldRevision = runtime.currentTick, revision
	baseline.LatestStateEventSequence = runtime.stateEventSequence
	baseline.LatestPresentationSequence = runtime.presentationSequence
	baseline.LatestStateMutationSequence = runtime.stateMutationSequence
	if stateMutationVerifyIncremental {
		runtime.verifyIncrementalCommitLocked(referenceBaseline, mutations)
	}
}

func (runtime *Runtime) appendStateMutationsLocked(mutations []StateMutation, tick Tick, revision WorldRevision, eventSequence, presentationSequence uint64) {
	for index := range mutations {
		runtime.stateMutationSequence++
		mutations[index].Sequence = runtime.stateMutationSequence
		mutations[index].Tick = tick
		mutations[index].WorldRevision = revision
		mutations[index].LatestStateEventSequence = eventSequence
		mutations[index].LatestPresentationSequence = presentationSequence
		runtime.stateMutations = append(runtime.stateMutations, cloneStateMutation(mutations[index]))
	}
	if overflow := len(runtime.stateMutations) - runtime.options.StateMutationLimit; overflow > 0 {
		copy(runtime.stateMutations, runtime.stateMutations[overflow:])
		runtime.stateMutations = runtime.stateMutations[:runtime.options.StateMutationLimit]
		runtime.stateMutationDropped += uint64(overflow)
	}
}

// diffWritePointsLocked computes the window's mutations from the recorded
// write points and advances the baseline in place, keeping it byte-identical
// with a full stateSnapshotLocked (the checkpoint invariant). Cooldowns,
// skill resources, abilities and active policies are key-tracked at their
// write sites; casts, processes and persistent states mutate through too many
// deep write points to record safely, so those domains are rebuilt wholesale
// and diffed exactly like the full path.
func (runtime *Runtime) diffWritePointsLocked(revision WorldRevision) []StateMutation {
	baseline := &runtime.stateMutationBaseline
	clockChanged := baseline.Tick != runtime.currentTick || baseline.WorldRevision != revision ||
		baseline.LatestStateEventSequence != runtime.stateEventSequence ||
		baseline.LatestPresentationSequence != runtime.presentationSequence
	if deltaTick := runtime.currentTick - baseline.Tick; deltaTick != 0 {
		// Clock-derived fields, advanced arithmetically. Ticks are monotonic,
		// so max(0, due-tick) folds to max(0, remaining-delta) exactly.
		for index := range baseline.Cooldowns {
			if remaining := baseline.Cooldowns[index].Remaining - deltaTick; remaining > 0 {
				baseline.Cooldowns[index].Remaining = remaining
			} else {
				baseline.Cooldowns[index].Remaining = 0
			}
		}
		for index := range baseline.Abilities {
			if remaining := baseline.Abilities[index].CooldownRemaining - deltaTick; remaining > 0 {
				baseline.Abilities[index].CooldownRemaining = remaining
			} else {
				baseline.Abilities[index].CooldownRemaining = 0
			}
		}
	}
	var result []StateMutation
	casts := runtime.castsSnapshotLocked()
	result = diffCastStates(result, baseline.Casts, casts)
	baseline.Casts = casts
	processes := runtime.processesSnapshotLocked()
	result = diffProcessStates(result, baseline.Processes, processes)
	baseline.Processes = processes
	persistent := runtime.persistentStatesSnapshotLocked()
	result = diffPersistentStates(result, baseline.PersistentStates, persistent)
	baseline.PersistentStates = persistent
	result = runtime.applyCooldownWritePointsLocked(result)
	result = runtime.applySkillResourceWritePointsLocked(result)
	result = runtime.applyAbilityWritePointsLocked(result)
	result = runtime.applyActivePolicyWritePointsLocked(result)
	if clockChanged {
		result = append(result, StateMutation{Kind: StateMutationClock})
	}
	sort.SliceStable(result, func(i, j int) bool { return mutationSortKey(result[i]) < mutationSortKey(result[j]) })
	return result
}

func (runtime *Runtime) applyCooldownWritePointsLocked(result []StateMutation) []StateMutation {
	if len(runtime.dirtyCooldowns) == 0 {
		return result
	}
	baseline := &runtime.stateMutationBaseline
	for key := range runtime.dirtyCooldowns {
		index := sort.Search(len(baseline.Cooldowns), func(i int) bool {
			if baseline.Cooldowns[i].Caster != key.Caster {
				return baseline.Cooldowns[i].Caster >= key.Caster
			}
			return baseline.Cooldowns[i].ProgramID >= key.Skill
		})
		found := index < len(baseline.Cooldowns) && baseline.Cooldowns[index].Caster == key.Caster && baseline.Cooldowns[index].ProgramID == key.Skill
		due, live := runtime.cooldowns[key]
		if !live {
			if found {
				baseline.Cooldowns = append(baseline.Cooldowns[:index], baseline.Cooldowns[index+1:]...)
				result = append(result, StateMutation{Kind: StateMutationCooldownRemove, Caster: key.Caster, ProgramID: key.Skill})
				runtime.refreshBaselineAbilityCooldownLocked(key, 0)
			}
			continue
		}
		value := runtime.cooldownSnapshotLocked(key, due)
		if !found || !cooldownStateEqualIgnoringClock(baseline.Cooldowns[index], value) {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationCooldownUpsert, Caster: key.Caster, ProgramID: key.Skill, Cooldown: &copyValue})
		}
		if found {
			baseline.Cooldowns[index] = value
		} else {
			baseline.Cooldowns = append(baseline.Cooldowns, CooldownStateSnapshot{})
			copy(baseline.Cooldowns[index+1:], baseline.Cooldowns[index:])
			baseline.Cooldowns[index] = value
		}
		runtime.refreshBaselineAbilityCooldownLocked(key, value.Remaining)
	}
	clear(runtime.dirtyCooldowns)
	return result
}

// refreshBaselineAbilityCooldownLocked mirrors the snapshot rule that an
// ability's CooldownRemaining is derived from its owner's cooldown entry.
func (runtime *Runtime) refreshBaselineAbilityCooldownLocked(key cooldownKey, remaining Tick) {
	abilities := runtime.stateMutationBaseline.Abilities
	for index := range abilities {
		if abilities[index].Owner == key.Caster && abilities[index].ProgramID == key.Skill {
			abilities[index].CooldownRemaining = remaining
		}
	}
}

func (runtime *Runtime) applySkillResourceWritePointsLocked(result []StateMutation) []StateMutation {
	if len(runtime.dirtyResources) == 0 {
		return result
	}
	baseline := &runtime.stateMutationBaseline
	for key := range runtime.dirtyResources {
		index := sort.Search(len(baseline.SkillResources), func(i int) bool {
			if baseline.SkillResources[i].Caster != key.Caster {
				return baseline.SkillResources[i].Caster >= key.Caster
			}
			return baseline.SkillResources[i].ProgramID >= key.Skill
		})
		found := index < len(baseline.SkillResources) && baseline.SkillResources[index].Caster == key.Caster && baseline.SkillResources[index].ProgramID == key.Skill
		state, live := runtime.skillStates[key]
		if !live {
			if found {
				baseline.SkillResources = append(baseline.SkillResources[:index], baseline.SkillResources[index+1:]...)
				result = append(result, StateMutation{Kind: StateMutationResourceRemove, Caster: key.Caster, ProgramID: key.Skill})
			}
			continue
		}
		value := skillResourceSnapshot(key, state)
		if !found || baseline.SkillResources[index] != value {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationResourceUpsert, Caster: key.Caster, ProgramID: key.Skill, Resource: &copyValue})
		}
		if found {
			baseline.SkillResources[index] = value
		} else {
			baseline.SkillResources = append(baseline.SkillResources, SkillResourceSnapshot{})
			copy(baseline.SkillResources[index+1:], baseline.SkillResources[index:])
			baseline.SkillResources[index] = value
		}
	}
	clear(runtime.dirtyResources)
	return result
}

func (runtime *Runtime) applyAbilityWritePointsLocked(result []StateMutation) []StateMutation {
	if len(runtime.dirtyAbilities) == 0 {
		return result
	}
	baseline := &runtime.stateMutationBaseline
	for key := range runtime.dirtyAbilities {
		index := sort.Search(len(baseline.Abilities), func(i int) bool {
			if baseline.Abilities[i].Owner != key.owner {
				return baseline.Abilities[i].Owner >= key.owner
			}
			return baseline.Abilities[i].Handle >= key.handle
		})
		found := index < len(baseline.Abilities) && baseline.Abilities[index].Owner == key.owner && baseline.Abilities[index].Handle == key.handle
		state, live := runtime.abilities[key]
		if !live || state == nil {
			if found {
				baseline.Abilities = append(baseline.Abilities[:index], baseline.Abilities[index+1:]...)
				result = append(result, StateMutation{Kind: StateMutationAbilityRemove, Owner: key.owner, AbilityHandle: key.handle})
			}
			continue
		}
		value := runtime.abilitySnapshotLocked(state)
		if !found || !abilityStateEqualIgnoringClock(baseline.Abilities[index], value) {
			copyValue := cloneAbilityState(value)
			result = append(result, StateMutation{Kind: StateMutationAbilityUpsert, Owner: key.owner, AbilityHandle: key.handle, Ability: &copyValue})
		}
		if found {
			baseline.Abilities[index] = value
		} else {
			baseline.Abilities = append(baseline.Abilities, AbilityStateSnapshot{})
			copy(baseline.Abilities[index+1:], baseline.Abilities[index:])
			baseline.Abilities[index] = value
		}
	}
	clear(runtime.dirtyAbilities)
	return result
}

func (runtime *Runtime) applyActivePolicyWritePointsLocked(result []StateMutation) []StateMutation {
	if len(runtime.dirtyPolicies) == 0 {
		return result
	}
	baseline := &runtime.stateMutationBaseline
	for key := range runtime.dirtyPolicies {
		index := sort.Search(len(baseline.ActivePolicies), func(i int) bool {
			if baseline.ActivePolicies[i].Caster != key.Caster {
				return baseline.ActivePolicies[i].Caster >= key.Caster
			}
			return baseline.ActivePolicies[i].ProgramID >= key.Skill
		})
		found := index < len(baseline.ActivePolicies) && baseline.ActivePolicies[index].Caster == key.Caster && baseline.ActivePolicies[index].ProgramID == key.Skill
		castID, live := runtime.activePolicies[key]
		if !live {
			if found {
				baseline.ActivePolicies = append(baseline.ActivePolicies[:index], baseline.ActivePolicies[index+1:]...)
				result = append(result, StateMutation{Kind: StateMutationPolicyRemove, Caster: key.Caster, ProgramID: key.Skill})
			}
			continue
		}
		value := ActivePolicySnapshot{Caster: key.Caster, ProgramID: key.Skill, CastID: castID}
		if !found || baseline.ActivePolicies[index] != value {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationPolicyUpsert, Caster: key.Caster, ProgramID: key.Skill, Policy: &copyValue})
		}
		if found {
			baseline.ActivePolicies[index] = value
		} else {
			baseline.ActivePolicies = append(baseline.ActivePolicies, ActivePolicySnapshot{})
			copy(baseline.ActivePolicies[index+1:], baseline.ActivePolicies[index:])
			baseline.ActivePolicies[index] = value
		}
	}
	clear(runtime.dirtyPolicies)
	return result
}

func (runtime *Runtime) clearStateMutationWritePointsLocked() {
	runtime.stateMutationAllDirty = false
	clear(runtime.dirtyCooldowns)
	clear(runtime.dirtyResources)
	clear(runtime.dirtyAbilities)
	clear(runtime.dirtyPolicies)
}

// Write-point recorders. Every mutation of a key-tracked domain must call the
// matching recorder before the window commits; a missed call surfaces as an
// ErrCheckpointHostMismatch (baseline divergence) and is caught in tests by
// stateMutationVerifyIncremental.
func (runtime *Runtime) touchCooldownLocked(key cooldownKey) {
	runtime.dirtyCooldowns[key] = struct{}{}
}

func (runtime *Runtime) touchSkillResourceLocked(key skillStateKey) {
	runtime.dirtyResources[key] = struct{}{}
}

func (runtime *Runtime) touchAbilityLocked(key abilityKey) {
	runtime.dirtyAbilities[key] = struct{}{}
}

func (runtime *Runtime) touchActivePolicyLocked(key skillStateKey) {
	runtime.dirtyPolicies[key] = struct{}{}
}

func cloneSnapshotDomains(snapshot RuntimeStateSnapshot) RuntimeStateSnapshot {
	snapshot.Casts = append([]CastStateSnapshot(nil), snapshot.Casts...)
	snapshot.Cooldowns = append([]CooldownStateSnapshot(nil), snapshot.Cooldowns...)
	snapshot.SkillResources = append([]SkillResourceSnapshot(nil), snapshot.SkillResources...)
	snapshot.Abilities = append([]AbilityStateSnapshot(nil), snapshot.Abilities...)
	snapshot.Processes = append([]ProcessStateSnapshot(nil), snapshot.Processes...)
	snapshot.ActivePolicies = append([]ActivePolicySnapshot(nil), snapshot.ActivePolicies...)
	snapshot.PersistentStates = append([]PersistentStateSnapshot(nil), snapshot.PersistentStates...)
	return snapshot
}

func (runtime *Runtime) verifyIncrementalCommitLocked(before RuntimeStateSnapshot, got []StateMutation) {
	current := runtime.stateSnapshotLocked()
	if !runtimeSnapshotsEqual(runtime.stateMutationBaseline, current) {
		panic("skill: incremental mutation baseline diverged from full snapshot")
	}
	expected := diffRuntimeState(before, current)
	if len(expected) != len(got) {
		panic(fmt.Sprintf("skill: incremental mutations diverged: %d mutations, reference has %d", len(got), len(expected)))
	}
	for index := range expected {
		expected[index].Sequence = got[index].Sequence
		expected[index].Tick = got[index].Tick
		expected[index].WorldRevision = got[index].WorldRevision
		expected[index].LatestStateEventSequence = got[index].LatestStateEventSequence
		expected[index].LatestPresentationSequence = got[index].LatestPresentationSequence
		if !reflect.DeepEqual(expected[index], got[index]) {
			panic(fmt.Sprintf("skill: incremental mutation %d diverged: got %+v, reference %+v", index, got[index], expected[index]))
		}
	}
}

func (runtime *Runtime) beginStateMutationLocked() {
	runtime.stateMutationDirty = true
}

// CaptureExternalState records changes made by an authoritative extension
// provider outside a Runtime API call. Normal Runtime mutations are captured
// automatically before their public method returns.
func (runtime *Runtime) CaptureExternalState() {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	runtime.stateMutationAllDirty = true
	runtime.commitStateMutationsLocked()
}

func diffRuntimeState(before, after RuntimeStateSnapshot) []StateMutation {
	var result []StateMutation
	result = diffCastStates(result, before.Casts, after.Casts)
	result = diffCooldownStates(result, before.Cooldowns, after.Cooldowns)
	result = diffSkillResourceStates(result, before.SkillResources, after.SkillResources)
	result = diffAbilityStates(result, before.Abilities, after.Abilities)
	result = diffProcessStates(result, before.Processes, after.Processes)
	result = diffActivePolicyStates(result, before.ActivePolicies, after.ActivePolicies)
	result = diffPersistentStates(result, before.PersistentStates, after.PersistentStates)
	if before.Tick != after.Tick || before.WorldRevision != after.WorldRevision || before.LatestStateEventSequence != after.LatestStateEventSequence || before.LatestPresentationSequence != after.LatestPresentationSequence {
		result = append(result, StateMutation{Kind: StateMutationClock})
	}
	sort.SliceStable(result, func(i, j int) bool { return mutationSortKey(result[i]) < mutationSortKey(result[j]) })
	return result
}

func diffCastStates(result []StateMutation, before, after []CastStateSnapshot) []StateMutation {
	beforeCasts := make(map[CastID]CastStateSnapshot, len(before))
	for _, value := range before {
		beforeCasts[value.ID] = value
	}
	afterCasts := make(map[CastID]CastStateSnapshot, len(after))
	for _, value := range after {
		afterCasts[value.ID] = value
		if previous, ok := beforeCasts[value.ID]; !ok || !castStateEqualIgnoringClock(previous, value) {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationCastUpsert, CastID: value.ID, Cast: &copyValue})
		}
	}
	for _, value := range before {
		if _, ok := afterCasts[value.ID]; !ok {
			result = append(result, StateMutation{Kind: StateMutationCastRemove, CastID: value.ID})
		}
	}
	return result
}

func diffCooldownStates(result []StateMutation, before, after []CooldownStateSnapshot) []StateMutation {
	for left, right := 0, 0; left < len(before) || right < len(after); {
		if left == len(before) {
			value := after[right]
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationCooldownUpsert, Caster: value.Caster, ProgramID: value.ProgramID, Cooldown: &copyValue})
			right++
			continue
		}
		if right == len(after) {
			value := before[left]
			result = append(result, StateMutation{Kind: StateMutationCooldownRemove, Caster: value.Caster, ProgramID: value.ProgramID})
			left++
			continue
		}
		previous, value := before[left], after[right]
		order := compareSkillState(previous.Caster, previous.ProgramID, value.Caster, value.ProgramID)
		if order < 0 {
			result = append(result, StateMutation{Kind: StateMutationCooldownRemove, Caster: previous.Caster, ProgramID: previous.ProgramID})
			left++
			continue
		}
		if order > 0 {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationCooldownUpsert, Caster: value.Caster, ProgramID: value.ProgramID, Cooldown: &copyValue})
			right++
			continue
		}
		if !cooldownStateEqualIgnoringClock(previous, value) {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationCooldownUpsert, Caster: value.Caster, ProgramID: value.ProgramID, Cooldown: &copyValue})
		}
		left++
		right++
	}
	return result
}

type skillDiffKey struct {
	entity  EntityID
	program string
}

func diffSkillResourceStates(result []StateMutation, before, after []SkillResourceSnapshot) []StateMutation {
	beforeResources := make(map[skillDiffKey]SkillResourceSnapshot, len(before))
	for _, value := range before {
		beforeResources[skillDiffKey{value.Caster, value.ProgramID}] = value
	}
	afterResources := make(map[skillDiffKey]SkillResourceSnapshot, len(after))
	for _, value := range after {
		key := skillDiffKey{value.Caster, value.ProgramID}
		afterResources[key] = value
		if previous, ok := beforeResources[key]; !ok || previous != value {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationResourceUpsert, Caster: value.Caster, ProgramID: value.ProgramID, Resource: &copyValue})
		}
	}
	for key := range beforeResources {
		if _, ok := afterResources[key]; !ok {
			result = append(result, StateMutation{Kind: StateMutationResourceRemove, Caster: key.entity, ProgramID: key.program})
		}
	}
	return result
}

func diffAbilityStates(result []StateMutation, before, after []AbilityStateSnapshot) []StateMutation {
	type abilityKeyView struct {
		owner  EntityID
		handle AbilityHandle
	}
	beforeAbilities := make(map[abilityKeyView]AbilityStateSnapshot, len(before))
	for _, value := range before {
		beforeAbilities[abilityKeyView{value.Owner, value.Handle}] = value
	}
	afterAbilities := make(map[abilityKeyView]AbilityStateSnapshot, len(after))
	for _, value := range after {
		key := abilityKeyView{value.Owner, value.Handle}
		afterAbilities[key] = value
		if previous, ok := beforeAbilities[key]; !ok || !abilityStateEqualIgnoringClock(previous, value) {
			copyValue := cloneAbilityState(value)
			result = append(result, StateMutation{Kind: StateMutationAbilityUpsert, Owner: value.Owner, AbilityHandle: value.Handle, Ability: &copyValue})
		}
	}
	for key := range beforeAbilities {
		if _, ok := afterAbilities[key]; !ok {
			result = append(result, StateMutation{Kind: StateMutationAbilityRemove, Owner: key.owner, AbilityHandle: key.handle})
		}
	}
	return result
}

func diffProcessStates(result []StateMutation, before, after []ProcessStateSnapshot) []StateMutation {
	beforeProcesses := make(map[ProcessID]ProcessStateSnapshot, len(before))
	for _, value := range before {
		beforeProcesses[value.ID] = value
	}
	afterProcesses := make(map[ProcessID]ProcessStateSnapshot, len(after))
	for _, value := range after {
		afterProcesses[value.ID] = value
		if previous, ok := beforeProcesses[value.ID]; !ok || !reflect.DeepEqual(previous, value) {
			copyValue := cloneProcessState(value)
			result = append(result, StateMutation{Kind: StateMutationProcessUpsert, ProcessID: value.ID, Process: &copyValue})
		}
	}
	for id := range beforeProcesses {
		if _, ok := afterProcesses[id]; !ok {
			result = append(result, StateMutation{Kind: StateMutationProcessRemove, ProcessID: id})
		}
	}
	return result
}

func diffActivePolicyStates(result []StateMutation, before, after []ActivePolicySnapshot) []StateMutation {
	beforePolicies := make(map[skillDiffKey]ActivePolicySnapshot, len(before))
	for _, value := range before {
		beforePolicies[skillDiffKey{value.Caster, value.ProgramID}] = value
	}
	afterPolicies := make(map[skillDiffKey]ActivePolicySnapshot, len(after))
	for _, value := range after {
		key := skillDiffKey{value.Caster, value.ProgramID}
		afterPolicies[key] = value
		if previous, ok := beforePolicies[key]; !ok || previous != value {
			copyValue := value
			result = append(result, StateMutation{Kind: StateMutationPolicyUpsert, Caster: value.Caster, ProgramID: value.ProgramID, Policy: &copyValue})
		}
	}
	for key := range beforePolicies {
		if _, ok := afterPolicies[key]; !ok {
			result = append(result, StateMutation{Kind: StateMutationPolicyRemove, Caster: key.entity, ProgramID: key.program})
		}
	}
	return result
}

func diffPersistentStates(result []StateMutation, before, after []PersistentStateSnapshot) []StateMutation {
	type persistentKey struct {
		handle  StateHandle
		binding StateScopeBinding
	}
	beforePersistent := make(map[persistentKey]PersistentStateSnapshot, len(before))
	for _, value := range before {
		beforePersistent[persistentKey{value.Handle, value.Binding}] = value
	}
	afterPersistent := make(map[persistentKey]PersistentStateSnapshot, len(after))
	for _, value := range after {
		key := persistentKey{value.Handle, value.Binding}
		afterPersistent[key] = value
		if previous, ok := beforePersistent[key]; !ok || !reflect.DeepEqual(previous, value) {
			copyValue := clonePersistentState(value)
			result = append(result, StateMutation{Kind: StateMutationPersistentUpsert, StateHandle: value.Handle, Binding: value.Binding, Persistent: &copyValue})
		}
	}
	for _, value := range before {
		if _, ok := afterPersistent[persistentKey{value.Handle, value.Binding}]; !ok {
			result = append(result, StateMutation{Kind: StateMutationPersistentRemove, StateHandle: value.Handle, Binding: value.Binding})
		}
	}
	return result
}

func castStateEqualIgnoringClock(left, right CastStateSnapshot) bool {
	left.ElapsedTicks, right.ElapsedTicks = 0, 0
	return reflect.DeepEqual(left, right)
}

func cooldownStateEqualIgnoringClock(left, right CooldownStateSnapshot) bool {
	left.Remaining, right.Remaining = 0, 0
	return left == right
}

func compareSkillState(leftEntity EntityID, leftProgram string, rightEntity EntityID, rightProgram string) int {
	if leftEntity < rightEntity {
		return -1
	}
	if leftEntity > rightEntity {
		return 1
	}
	if leftProgram < rightProgram {
		return -1
	}
	if leftProgram > rightProgram {
		return 1
	}
	return 0
}

func abilityStateEqualIgnoringClock(left, right AbilityStateSnapshot) bool {
	left.CooldownRemaining, right.CooldownRemaining = 0, 0
	return reflect.DeepEqual(left, right)
}

func mutationSortKey(value StateMutation) string {
	// Every identity field a mutation kind can carry must be part of this
	// key: two distinct mutations with equal keys would leave their relative
	// order to map iteration, breaking cross-run sequence determinism.
	return string(value.Kind) + ":" + value.ProgramID + ":" + value.StateHandle.GameplayDigest + ":" + formatMutationID(uint64(value.CastID), uint64(value.Caster), uint64(value.Owner), uint64(value.AbilityHandle), uint64(value.ProcessID), uint64(value.StateHandle.Slot), uint64(value.StateHandle.Shared), uint64(value.Binding.Owner), uint64(value.Binding.Subject), value.Binding.Team)
}

func formatMutationID(values ...uint64) string {
	const digits = "0123456789abcdef"
	buffer := make([]byte, 0, len(values)*17)
	for _, value := range values {
		for shift := 60; shift >= 0; shift -= 4 {
			buffer = append(buffer, digits[value>>uint(shift)&15])
		}
		buffer = append(buffer, ':')
	}
	return string(buffer)
}

func cloneStateMutations(values []StateMutation) []StateMutation {
	result := make([]StateMutation, len(values))
	for index := range values {
		result[index] = cloneStateMutation(values[index])
	}
	return result
}

func cloneStateMutation(value StateMutation) StateMutation {
	if value.Cast != nil {
		copyValue := *value.Cast
		value.Cast = &copyValue
	}
	if value.Cooldown != nil {
		copyValue := *value.Cooldown
		value.Cooldown = &copyValue
	}
	if value.Resource != nil {
		copyValue := *value.Resource
		value.Resource = &copyValue
	}
	if value.Ability != nil {
		copyValue := cloneAbilityState(*value.Ability)
		value.Ability = &copyValue
	}
	if value.Process != nil {
		copyValue := cloneProcessState(*value.Process)
		value.Process = &copyValue
	}
	if value.Policy != nil {
		copyValue := *value.Policy
		value.Policy = &copyValue
	}
	if value.Persistent != nil {
		copyValue := clonePersistentState(*value.Persistent)
		value.Persistent = &copyValue
	}
	return value
}

func cloneAbilityState(value AbilityStateSnapshot) AbilityStateSnapshot {
	value.Tags = append([]GameplayTagHandle(nil), value.Tags...)
	value.Overlays = append([]AbilityOverlaySnapshot(nil), value.Overlays...)
	return value
}

func cloneProcessState(value ProcessStateSnapshot) ProcessStateSnapshot {
	value.Numeric = append([]NumericPropertySnapshot(nil), value.Numeric...)
	return value
}

func clonePersistentState(value PersistentStateSnapshot) PersistentStateSnapshot {
	value.Value = cloneStateRuntimeValue(value.Value)
	value.ClearOn = append([]string(nil), value.ClearOn...)
	return value
}

// ApplyStateMutation is the canonical client reducer used by tests and
// production consumers.
func ApplyStateMutation(snapshot *RuntimeStateSnapshot, mutation StateMutation) error {
	if snapshot == nil || mutation.Sequence == 0 || mutation.Kind == "" {
		return ErrStateMutationInvalid
	}
	snapshot.Tick, snapshot.WorldRevision = mutation.Tick, mutation.WorldRevision
	snapshot.LatestStateEventSequence = mutation.LatestStateEventSequence
	snapshot.LatestPresentationSequence = mutation.LatestPresentationSequence
	snapshot.LatestStateMutationSequence = mutation.Sequence
	switch mutation.Kind {
	case StateMutationClock:
		recomputeClockDerivedState(snapshot)
	case StateMutationCastUpsert:
		if mutation.Cast == nil {
			return ErrStateMutationInvalid
		}
		snapshot.Casts = upsertBy(snapshot.Casts, *mutation.Cast, func(v CastStateSnapshot) CastID { return v.ID })
	case StateMutationCastRemove:
		snapshot.Casts = removeBy(snapshot.Casts, mutation.CastID, func(v CastStateSnapshot) CastID { return v.ID })
	case StateMutationCooldownUpsert:
		if mutation.Cooldown == nil {
			return ErrStateMutationInvalid
		}
		snapshot.Cooldowns = upsertSkillState(snapshot.Cooldowns, *mutation.Cooldown, func(v CooldownStateSnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
		setAbilityCooldownRemaining(snapshot, mutation.Cooldown.Caster, mutation.Cooldown.ProgramID, mutation.Cooldown.Remaining)
	case StateMutationCooldownRemove:
		snapshot.Cooldowns = removeSkillState(snapshot.Cooldowns, mutation.Caster, mutation.ProgramID, func(v CooldownStateSnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
		setAbilityCooldownRemaining(snapshot, mutation.Caster, mutation.ProgramID, 0)
	case StateMutationResourceUpsert:
		if mutation.Resource == nil {
			return ErrStateMutationInvalid
		}
		snapshot.SkillResources = upsertSkillState(snapshot.SkillResources, *mutation.Resource, func(v SkillResourceSnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
	case StateMutationResourceRemove:
		snapshot.SkillResources = removeSkillState(snapshot.SkillResources, mutation.Caster, mutation.ProgramID, func(v SkillResourceSnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
	case StateMutationAbilityUpsert:
		if mutation.Ability == nil {
			return ErrStateMutationInvalid
		}
		value := cloneAbilityState(*mutation.Ability)
		snapshot.Abilities = upsertAbility(snapshot.Abilities, value)
	case StateMutationAbilityRemove:
		snapshot.Abilities = removeAbility(snapshot.Abilities, mutation.Owner, mutation.AbilityHandle)
	case StateMutationProcessUpsert:
		if mutation.Process == nil {
			return ErrStateMutationInvalid
		}
		value := cloneProcessState(*mutation.Process)
		snapshot.Processes = upsertBy(snapshot.Processes, value, func(v ProcessStateSnapshot) ProcessID { return v.ID })
	case StateMutationProcessRemove:
		snapshot.Processes = removeBy(snapshot.Processes, mutation.ProcessID, func(v ProcessStateSnapshot) ProcessID { return v.ID })
	case StateMutationPolicyUpsert:
		if mutation.Policy == nil {
			return ErrStateMutationInvalid
		}
		snapshot.ActivePolicies = upsertSkillState(snapshot.ActivePolicies, *mutation.Policy, func(v ActivePolicySnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
	case StateMutationPolicyRemove:
		snapshot.ActivePolicies = removeSkillState(snapshot.ActivePolicies, mutation.Caster, mutation.ProgramID, func(v ActivePolicySnapshot) (EntityID, string) { return v.Caster, v.ProgramID })
	case StateMutationPersistentUpsert:
		if mutation.Persistent == nil {
			return ErrStateMutationInvalid
		}
		value := clonePersistentState(*mutation.Persistent)
		snapshot.PersistentStates = upsertPersistent(snapshot.PersistentStates, value)
	case StateMutationPersistentRemove:
		snapshot.PersistentStates = removePersistent(snapshot.PersistentStates, mutation.StateHandle, mutation.Binding)
	default:
		return ErrStateMutationInvalid
	}
	sortRuntimeStateSnapshot(snapshot)
	return nil
}

func recomputeClockDerivedState(snapshot *RuntimeStateSnapshot) {
	for index := range snapshot.Casts {
		elapsed := snapshot.Tick - snapshot.Casts[index].StartTick
		if elapsed < 0 {
			elapsed = 0
		}
		snapshot.Casts[index].ElapsedTicks = elapsed
	}
	remaining := make(map[skillKeyForApply]Tick, len(snapshot.Cooldowns))
	for index := range snapshot.Cooldowns {
		value := snapshot.Cooldowns[index].DueTick - snapshot.Tick
		if value < 0 {
			value = 0
		}
		snapshot.Cooldowns[index].Remaining = value
		remaining[skillKeyForApply{snapshot.Cooldowns[index].Caster, snapshot.Cooldowns[index].ProgramID}] = value
	}
	for index := range snapshot.Abilities {
		snapshot.Abilities[index].CooldownRemaining = remaining[skillKeyForApply{snapshot.Abilities[index].Owner, snapshot.Abilities[index].ProgramID}]
	}
}

type skillKeyForApply struct {
	entity  EntityID
	program string
}

func setAbilityCooldownRemaining(snapshot *RuntimeStateSnapshot, owner EntityID, program string, remaining Tick) {
	for index := range snapshot.Abilities {
		if snapshot.Abilities[index].Owner == owner && snapshot.Abilities[index].ProgramID == program {
			snapshot.Abilities[index].CooldownRemaining = remaining
		}
	}
}

type orderedID interface{ ~uint64 }

func upsertBy[T any, K orderedID](values []T, value T, key func(T) K) []T {
	for i := range values {
		if key(values[i]) == key(value) {
			values[i] = value
			return values
		}
	}
	return append(values, value)
}
func removeBy[T any, K orderedID](values []T, target K, key func(T) K) []T {
	result := values[:0]
	for _, value := range values {
		if key(value) != target {
			result = append(result, value)
		}
	}
	return result
}
func upsertSkillState[T any](values []T, value T, key func(T) (EntityID, string)) []T {
	entity, program := key(value)
	for i := range values {
		e, p := key(values[i])
		if e == entity && p == program {
			values[i] = value
			return values
		}
	}
	return append(values, value)
}
func removeSkillState[T any](values []T, entity EntityID, program string, key func(T) (EntityID, string)) []T {
	result := values[:0]
	for _, value := range values {
		e, p := key(value)
		if e != entity || p != program {
			result = append(result, value)
		}
	}
	return result
}
func upsertAbility(values []AbilityStateSnapshot, value AbilityStateSnapshot) []AbilityStateSnapshot {
	for i := range values {
		if values[i].Owner == value.Owner && values[i].Handle == value.Handle {
			values[i] = value
			return values
		}
	}
	return append(values, value)
}
func removeAbility(values []AbilityStateSnapshot, owner EntityID, handle AbilityHandle) []AbilityStateSnapshot {
	result := values[:0]
	for _, value := range values {
		if value.Owner != owner || value.Handle != handle {
			result = append(result, value)
		}
	}
	return result
}
func upsertPersistent(values []PersistentStateSnapshot, value PersistentStateSnapshot) []PersistentStateSnapshot {
	for i := range values {
		if values[i].Handle == value.Handle && values[i].Binding == value.Binding {
			values[i] = value
			return values
		}
	}
	return append(values, value)
}
func removePersistent(values []PersistentStateSnapshot, handle StateHandle, binding StateScopeBinding) []PersistentStateSnapshot {
	result := values[:0]
	for _, value := range values {
		if value.Handle != handle || value.Binding != binding {
			result = append(result, value)
		}
	}
	return result
}

func sortRuntimeStateSnapshot(snapshot *RuntimeStateSnapshot) {
	sort.Slice(snapshot.Casts, func(i, j int) bool { return snapshot.Casts[i].ID < snapshot.Casts[j].ID })
	sort.Slice(snapshot.Cooldowns, func(i, j int) bool {
		if snapshot.Cooldowns[i].Caster != snapshot.Cooldowns[j].Caster {
			return snapshot.Cooldowns[i].Caster < snapshot.Cooldowns[j].Caster
		}
		return snapshot.Cooldowns[i].ProgramID < snapshot.Cooldowns[j].ProgramID
	})
	sort.Slice(snapshot.SkillResources, func(i, j int) bool {
		if snapshot.SkillResources[i].Caster != snapshot.SkillResources[j].Caster {
			return snapshot.SkillResources[i].Caster < snapshot.SkillResources[j].Caster
		}
		return snapshot.SkillResources[i].ProgramID < snapshot.SkillResources[j].ProgramID
	})
	sort.Slice(snapshot.Abilities, func(i, j int) bool {
		if snapshot.Abilities[i].Owner != snapshot.Abilities[j].Owner {
			return snapshot.Abilities[i].Owner < snapshot.Abilities[j].Owner
		}
		return snapshot.Abilities[i].Handle < snapshot.Abilities[j].Handle
	})
	sort.Slice(snapshot.Processes, func(i, j int) bool { return snapshot.Processes[i].ID < snapshot.Processes[j].ID })
	sort.Slice(snapshot.ActivePolicies, func(i, j int) bool {
		if snapshot.ActivePolicies[i].Caster != snapshot.ActivePolicies[j].Caster {
			return snapshot.ActivePolicies[i].Caster < snapshot.ActivePolicies[j].Caster
		}
		return snapshot.ActivePolicies[i].ProgramID < snapshot.ActivePolicies[j].ProgramID
	})
}
