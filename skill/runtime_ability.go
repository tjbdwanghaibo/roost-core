package skill

import (
	"math"
	"sort"
)

type abilityKey struct {
	owner  EntityID
	handle AbilityHandle
}

type abilityState struct {
	owner          EntityID
	handle         AbilityHandle
	slot           int
	tags           []GameplayTagHandle
	program        *Program
	cooldownTotal  Tick
	ammoStock      int64
	ammoMax        int64
	castActive     int
	lastCommitTick Tick
	lastFinishTick Tick
	overlays       map[uint64]Tick
}

type AbilityRegistration struct {
	Owner     EntityID
	Handle    AbilityHandle
	Slot      int
	Tags      []GameplayTagHandle
	Program   *Program
	AmmoStock int64
	AmmoMax   int64
}

type AbilityChangeResult struct {
	ResultOutcome
	Before RuntimeValue
	After  RuntimeValue
}

type AbilityChangeEvent struct {
	Owner     EntityID
	Ability   AbilityHandle
	Property  string
	Before    RuntimeValue
	After     RuntimeValue
	Operation string
}

func (runtime *Runtime) executeAbilityStateMutation(cast *castInstance, operation abilityStateOperation) (AbilityChangeResult, error) {
	property, found := abilityPropertyByHandle(cast.program, operation.property)
	if !found || property.name != operation.propertyName {
		return AbilityChangeResult{}, ErrProgramInvariant
	}
	owner, err := runtime.evalEntity(cast, operation.owner)
	if err != nil {
		return AbilityChangeResult{}, err
	}
	abilityValue, err := runtime.evalValue(cast, operation.ability)
	if err != nil {
		return AbilityChangeResult{}, err
	}
	ability, ok := abilityValue.Ability()
	if !ok {
		return AbilityChangeResult{}, ErrRuntimeTypeMismatch
	}
	if ability.Owner != owner || !runtime.abilityOwnerAllowed(cast.program, cast.caster, owner) {
		return AbilityChangeResult{ResultOutcome: failedResultOutcome(ExpectedFailurePermissionDenied)}, nil
	}
	value, err := runtime.evalValue(cast, operation.value)
	if err != nil {
		return AbilityChangeResult{}, err
	}
	result, err := runtime.modifyAbilityStateLocked(owner, ability.Handle, operation.propertyName, operation.operation, value, operation.durationTicks, runtime.effectEventContext(cast, operation.effectIndex))
	if err == ErrCastInputInvalid {
		return AbilityChangeResult{ResultOutcome: failedResultOutcome(ExpectedFailureReferenceExpired)}, nil
	}
	if err == ErrCastInputRejected {
		return AbilityChangeResult{ResultOutcome: failedResultOutcome(ExpectedFailurePolicyRejected)}, nil
	}
	return result, err
}

func (runtime *Runtime) RegisterAbility(registration AbilityRegistration) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if registration.Owner == 0 || registration.Handle == 0 || registration.Slot < 0 || registration.Program == nil || runtime.host == nil || registration.Program.compilerSemanticsRevision != runtime.options.SupportedCompilerSemanticsRevision || !authorityMatches(registration.Program.authority, runtime.host.AuthorityIdentity()) {
		return ErrCastInputInvalid
	}
	programKey := skillStateKey{Caster: registration.Owner, Skill: registration.Program.id}
	if runtime.abilityByProgram[programKey] != 0 {
		return ErrCastInputInvalid
	}
	if _, exists := runtime.abilities[abilityKey{owner: registration.Owner, handle: registration.Handle}]; exists {
		return ErrCastInputInvalid
	}
	if len(runtime.abilities) >= runtime.options.MaxAbilities {
		return ErrRuntimeCapacityExceeded
	}
	if registration.Program.cast.mode == castModeAmmo {
		if registration.AmmoMax != registration.Program.cast.maxStock || registration.AmmoStock < 0 || registration.AmmoStock > registration.AmmoMax {
			return ErrCastInputInvalid
		}
	} else if registration.AmmoStock != 0 || registration.AmmoMax != 0 {
		return ErrCastInputInvalid
	}
	tags := registration.Tags
	for _, tag := range tags {
		if !containsGameplayTag(registration.Program.abilityControl.selectableTags, tag) {
			return ErrCastInputInvalid
		}
	}
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	state := &abilityState{owner: registration.Owner, handle: registration.Handle, slot: registration.Slot, tags: normalizeGameplayTagHandles(tags), program: registration.Program, ammoStock: registration.AmmoStock, ammoMax: registration.AmmoMax, overlays: make(map[uint64]Tick)}
	runtime.abilities[abilityKey{owner: state.owner, handle: state.handle}] = state
	runtime.touchAbilityLocked(abilityKey{owner: state.owner, handle: state.handle})
	runtime.abilityByProgram[skillStateKey{Caster: state.owner, Skill: state.program.id}] = state.handle
	if state.program.cast.mode == castModeAmmo {
		runtime.skillStates[skillStateKey{Caster: state.owner, Skill: state.program.id}] = &skillState{stock: state.ammoStock, maxStock: state.ammoMax, rechargeTicks: state.program.cast.rechargeTicks}
		runtime.touchSkillResourceLocked(skillStateKey{Caster: state.owner, Skill: state.program.id})
	}
	if state.handle > runtime.nextAbilityHandle {
		runtime.nextAbilityHandle = state.handle
	}
	return nil
}

func (runtime *Runtime) abilityOwnerAllowed(program *Program, viewer, owner EntityID) bool {
	relation := ""
	if viewer == owner {
		relation = "self"
	} else if provider, ok := runtime.host.(AbilityRelationProvider); ok {
		var found bool
		relation, found = provider.AbilityOwnerRelation(viewer, owner)
		if !found {
			return false
		}
	}
	for _, allowed := range program.abilityControl.ownerRelations {
		if allowed == relation {
			return true
		}
	}
	return false
}

func (runtime *Runtime) ensureAbilityLocked(owner EntityID, program *Program) (*abilityState, error) {
	programKey := skillStateKey{Caster: owner, Skill: program.id}
	if handle := runtime.abilityByProgram[programKey]; handle != 0 {
		state := runtime.abilities[abilityKey{owner: owner, handle: handle}]
		if state == nil || len(state.overlays) > 0 {
			return nil, ErrCastInputRejected
		}
		return state, nil
	}
	if len(runtime.abilities) >= runtime.options.MaxAbilities {
		return nil, ErrRuntimeCapacityExceeded
	}
	runtime.nextAbilityHandle++
	state := &abilityState{owner: owner, handle: runtime.nextAbilityHandle, slot: runtime.nextAbilitySlot(owner), tags: selectableProgramAbilityTags(program), program: program, overlays: make(map[uint64]Tick)}
	if program.cast.mode == castModeAmmo {
		if ammo := runtime.skillStates[skillStateKey{Caster: owner, Skill: program.id}]; ammo != nil {
			state.ammoStock, state.ammoMax = ammo.stock, ammo.maxStock
		} else {
			state.ammoStock, state.ammoMax = program.cast.initialStock, program.cast.maxStock
		}
	}
	runtime.abilities[abilityKey{owner: owner, handle: state.handle}] = state
	runtime.touchAbilityLocked(abilityKey{owner: owner, handle: state.handle})
	runtime.abilityByProgram[programKey] = state.handle
	return state, nil
}

func selectableProgramAbilityTags(program *Program) []GameplayTagHandle {
	result := make([]GameplayTagHandle, 0, len(program.gameplayTags))
	for _, tag := range program.gameplayTags {
		if containsGameplayTag(program.abilityControl.selectableTags, tag) {
			result = append(result, tag)
		}
	}
	return result
}

func (runtime *Runtime) nextAbilitySlot(owner EntityID) int {
	maximum := -1
	for key, state := range runtime.abilities {
		if key.owner == owner && state.slot > maximum {
			maximum = state.slot
		}
	}
	return maximum + 1
}

func (runtime *Runtime) markAbilityCastStarted(cast *castInstance) {
	if state := runtime.abilities[abilityKey{owner: cast.caster, handle: cast.ability}]; state != nil {
		state.castActive++
		runtime.touchAbilityLocked(abilityKey{owner: cast.caster, handle: cast.ability})
	}
}

func (runtime *Runtime) markAbilityCommitted(cast *castInstance) {
	if state := runtime.abilities[abilityKey{owner: cast.caster, handle: cast.ability}]; state != nil {
		runtime.touchAbilityLocked(abilityKey{owner: cast.caster, handle: cast.ability})
		state.cooldownTotal = cast.program.cooldownTicks
		state.lastCommitTick = runtime.currentTick
		if cast.program.cast.mode == castModeAmmo {
			ammo := runtime.ammoState(cast)
			state.ammoStock, state.ammoMax = ammo.stock, ammo.maxStock
		}
	}
}

func (runtime *Runtime) markAbilityCastFinished(cast *castInstance) {
	if cast.abilityFinished {
		return
	}
	cast.abilityFinished = true
	runtime.trackCompletedCastLocked(cast)
	if state := runtime.abilities[abilityKey{owner: cast.caster, handle: cast.ability}]; state != nil {
		if state.castActive > 0 {
			state.castActive--
		}
		state.lastFinishTick = runtime.currentTick
		runtime.touchAbilityLocked(abilityKey{owner: cast.caster, handle: cast.ability})
	}
}

func (runtime *Runtime) ReadAbilityState(owner EntityID, ability AbilityHandle, property string) (RuntimeValue, error) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return runtime.readAbilityStateLocked(owner, ability, property)
}

func (runtime *Runtime) readAbilityStateLocked(owner EntityID, ability AbilityHandle, property string) (RuntimeValue, error) {
	state := runtime.abilities[abilityKey{owner: owner, handle: ability}]
	if state == nil {
		return RuntimeValue{}, ErrCastInputInvalid
	}
	if _, found := abilityPropertyByName(state.program, property); !found {
		return RuntimeValue{}, ErrCastInputRejected
	}
	switch property {
	case "cooldown_remaining_ticks":
		due := runtime.cooldowns[cooldownKey{Caster: owner, Skill: state.program.id}]
		remaining := due - runtime.currentTick
		if remaining < 0 {
			remaining = 0
		}
		return IntRuntimeValue(int64(remaining), quantityTicks), nil
	case "cooldown_total_ticks":
		return IntRuntimeValue(int64(state.cooldownTotal), quantityTicks), nil
	case "ammo_stock":
		// Pure read: the authoritative stock lives in skillStates. Writing it
		// back into the ability cache here would mutate snapshot-visible
		// state on a read path without a write point, silently diverging the
		// incremental mutation baseline (the cache itself is synchronized at
		// recharge and commit time).
		if state.program.cast.mode == castModeAmmo {
			if ammo := runtime.skillStates[skillStateKey{Caster: owner, Skill: state.program.id}]; ammo != nil {
				return IntRuntimeValue(ammo.stock, quantityCount), nil
			}
		}
		return IntRuntimeValue(state.ammoStock, quantityCount), nil
	case "ammo_max_stock":
		if state.program.cast.mode == castModeAmmo {
			if ammo := runtime.skillStates[skillStateKey{Caster: owner, Skill: state.program.id}]; ammo != nil {
				return IntRuntimeValue(ammo.maxStock, quantityCount), nil
			}
		}
		return IntRuntimeValue(state.ammoMax, quantityCount), nil
	case "enabled":
		return BoolRuntimeValue(len(state.overlays) == 0), nil
	case "cast_active":
		return BoolRuntimeValue(state.castActive > 0), nil
	case "last_commit_tick":
		return IntRuntimeValue(int64(state.lastCommitTick), quantityTicks), nil
	case "last_finish_tick":
		return IntRuntimeValue(int64(state.lastFinishTick), quantityTicks), nil
	default:
		return RuntimeValue{}, ErrProgramInvariant
	}
}

func (runtime *Runtime) ModifyAbilityState(owner EntityID, ability AbilityHandle, property, operation string, value RuntimeValue, duration Tick, event EventContext) (AbilityChangeResult, error) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	return runtime.modifyAbilityStateLocked(owner, ability, property, operation, value, duration, event)
}

func (runtime *Runtime) modifyAbilityStateLocked(owner EntityID, ability AbilityHandle, property, operation string, value RuntimeValue, duration Tick, event EventContext) (AbilityChangeResult, error) {
	state := runtime.abilities[abilityKey{owner: owner, handle: ability}]
	if state == nil {
		return AbilityChangeResult{}, ErrCastInputInvalid
	}
	policy, found := abilityPropertyByName(state.program, property)
	if !found || !policy.mutable || !abilityOperationAllowed(property, operation) {
		return AbilityChangeResult{}, ErrCastInputRejected
	}
	if !validAbilityMutationValue(property, operation, value) {
		return AbilityChangeResult{}, ErrRuntimeTypeMismatch
	}
	before, err := runtime.readAbilityStateLocked(owner, ability, property)
	if err != nil {
		return AbilityChangeResult{}, err
	}
	if property == "enabled" {
		enabled, ok := value.Bool()
		if !ok || enabled || duration <= 0 || duration > policy.maximumDuration {
			return AbilityChangeResult{}, ErrCastInputRejected
		}
		runtime.nextAbilityOverlay++
		overlayID := runtime.nextAbilityOverlay
		state.overlays[overlayID] = runtime.currentTick + duration
		runtime.touchAbilityLocked(abilityKey{owner: state.owner, handle: state.handle})
		if err := runtime.scheduleSystem(runtime.currentTick+duration, &abilityOverlayExpiryTask{Owner: owner, Ability: ability, OverlayID: overlayID, Context: cloneEventContext(event)}); err != nil {
			return AbilityChangeResult{}, err
		}
		after := BoolRuntimeValue(false)
		runtime.emitAbilityChange(state, property, operation, before, after, event)
		return AbilityChangeResult{ResultOutcome: successfulResultOutcome(), Before: before, After: after}, nil
	}
	current, currentOK := before.Int()
	operand, operandOK := value.Int()
	if !currentOK || !operandOK {
		return AbilityChangeResult{}, ErrRuntimeTypeMismatch
	}
	next, err := applyAbilityNumericOperation(current, operand, operation)
	if err != nil {
		return AbilityChangeResult{}, err
	}
	if policy.maximumMutation > 0 {
		minimumChange, maximumChange := current-policy.maximumMutation, current+policy.maximumMutation
		if next < minimumChange {
			next = minimumChange
		}
		if next > maximumChange {
			next = maximumChange
		}
	}
	if next < policy.minimum {
		next = policy.minimum
	}
	if next > policy.maximum {
		next = policy.maximum
	}
	if property == "ammo_stock" && next > state.ammoMax {
		next = state.ammoMax
	}
	if property == "cooldown_remaining_ticks" {
		runtime.cooldowns[cooldownKey{Caster: owner, Skill: state.program.id}] = runtime.currentTick + Tick(next)
		runtime.touchCooldownLocked(cooldownKey{Caster: owner, Skill: state.program.id})
	} else {
		state.ammoStock = next
		runtime.touchAbilityLocked(abilityKey{owner: state.owner, handle: state.handle})
		if state.program.cast.mode == castModeAmmo {
			ammo := runtime.skillStates[skillStateKey{Caster: owner, Skill: state.program.id}]
			if ammo == nil {
				ammo = &skillState{stock: next, maxStock: state.ammoMax, rechargeTicks: state.program.cast.rechargeTicks}
				runtime.skillStates[skillStateKey{Caster: owner, Skill: state.program.id}] = ammo
			}
			ammo.stock = next
			if next >= ammo.maxStock {
				ammo.rechargeScheduled = false
				ammo.rechargeGeneration++
			}
			runtime.touchSkillResourceLocked(skillStateKey{Caster: owner, Skill: state.program.id})
		}
	}
	after := IntRuntimeValue(next, abilityPropertyQuantity(property))
	runtime.emitAbilityChange(state, property, operation, before, after, event)
	return AbilityChangeResult{ResultOutcome: successfulResultOutcome(), Before: before, After: after}, nil
}

func validAbilityMutationValue(property, operation string, value RuntimeValue) bool {
	if property == "enabled" {
		_, ok := value.Bool()
		return ok
	}
	if _, ok := value.Int(); !ok {
		return false
	}
	want := abilityPropertyQuantity(property)
	if operation == "mul_bp" {
		want = quantityBasisPoints
	}
	return value.Type().Quantity == want
}

func applyAbilityNumericOperation(current, operand int64, operation string) (int64, error) {
	switch operation {
	case "set":
		return operand, nil
	case "add":
		if operand > 0 && current > math.MaxInt64-operand || operand < 0 && current < math.MinInt64-operand {
			return 0, ErrRuntimeArithmeticOverflow
		}
		return current + operand, nil
	case "mul_bp":
		product, ok := checkedInt64Mul(current, operand)
		if !ok {
			return 0, ErrRuntimeArithmeticOverflow
		}
		return product / 10000, nil
	case "min":
		if operand < current {
			return operand, nil
		}
		return current, nil
	case "max":
		if operand > current {
			return operand, nil
		}
		return current, nil
	default:
		return 0, ErrProgramInvariant
	}
}

func abilityPropertyByName(program *Program, name string) (abilityPropertyProgram, bool) {
	for _, property := range program.abilityProperties {
		if property.name == name {
			return property, true
		}
	}
	return abilityPropertyProgram{}, false
}

func abilityPropertyByHandle(program *Program, handle AbilityPropertyHandle) (abilityPropertyProgram, bool) {
	for _, property := range program.abilityProperties {
		if property.handle == handle {
			return property, true
		}
	}
	return abilityPropertyProgram{}, false
}

func (runtime *Runtime) emitAbilityChange(state *abilityState, property, operation string, before, after RuntimeValue, context EventContext) {
	kind := "ability_state_changed"
	switch property {
	case "cooldown_remaining_ticks":
		kind = "cooldown_changed"
	case "ammo_stock":
		kind = "ammo_changed"
	case "enabled":
		kind = "ability_enabled_changed"
	}
	event := RuntimeEvent{Tick: runtime.currentTick, Kind: kind, Entity: state.owner, Context: cloneEventContext(context), Ability: &AbilityChangeEvent{Owner: state.owner, Ability: state.handle, Property: property, Before: before, After: after, Operation: operation}}
	runtime.appendRuntimeEvent(event)
	if cast := runtime.casts[context.CastID]; cast != nil {
		runtime.appendCastEvent(cast, event)
	}
}

func (runtime *Runtime) expireAbilityOverlay(task *abilityOverlayExpiryTask) error {
	state := runtime.abilities[abilityKey{owner: task.Owner, handle: task.Ability}]
	if state == nil {
		return nil
	}
	if _, found := state.overlays[task.OverlayID]; !found {
		return nil
	}
	delete(state.overlays, task.OverlayID)
	runtime.touchAbilityLocked(abilityKey{owner: task.Owner, handle: task.Ability})
	if len(state.overlays) == 0 {
		context := cloneEventContext(task.Context)
		context.Tick = runtime.currentTick
		context.Owner = state.owner
		runtime.emitAbilityChange(state, "enabled", "expire", BoolRuntimeValue(false), BoolRuntimeValue(true), context)
	}
	return nil
}

func (runtime *Runtime) abilitySelection(owner EntityID) []*abilityState {
	result := make([]*abilityState, 0)
	for key, state := range runtime.abilities {
		if key.owner == owner {
			result = append(result, state)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].slot != result[right].slot {
			return result[left].slot < result[right].slot
		}
		return result[left].handle < result[right].handle
	})
	return result
}
