package skill

// globalCooldownProgramID is the reserved cooldown-map program id carrying a
// caster's global cooldown. It is not a valid skill identifier, so it can
// never collide with a real program, and it rides the ordinary cooldown
// machinery: state sync, mutations and checkpoints all see it as a plain
// cooldown entry.
const globalCooldownProgramID = "$gcd"

// casterWindowBusyLocked reports whether the caster currently owns a cast
// that is inside its exclusive window (windup, commit, or recovery). It scans
// the cast table instead of maintaining a counter: window stages change at
// many sites, casts are bounded, and a scan cannot drift out of sync.
func (runtime *Runtime) casterWindowBusyLocked(caster EntityID) bool {
	for _, cast := range runtime.casts {
		if cast.caster != caster || cast.status == CastFinished || cast.status == CastFailed {
			continue
		}
		switch cast.windowStage {
		case CastWindowPreparing, CastWindowCommitted, CastWindowRecovering:
			return true
		}
	}
	return false
}

func (runtime *Runtime) prepareCast(cast *castInstance) error {
	key := cooldownKey{Caster: cast.cooldownOwner, Skill: cast.program.id}
	if due := runtime.cooldowns[key]; due > runtime.currentTick {
		return ErrCooldownActive
	}
	cast.startTick = runtime.currentTick
	if cast.program.cast.mode == castModeCharge {
		cast.windowStage = CastWindowExecuting
		cast.pendingRootEvent = "enter"
		runtime.emitCastLifecycleEvent(cast, "cast_charging")
		return runtime.executeCast(cast)
	}
	return runtime.prepareCastWindow(cast, "enter")
}

func (runtime *Runtime) prepareCastWindow(cast *castInstance, event string) error {
	cast.windowStartTick = runtime.currentTick
	cast.pendingRootEvent = event
	cast.windowStage = CastWindowPreparing
	if cast.program.cast.mode == castModeAmmo {
		state := runtime.ammoState(cast)
		cast.stock, cast.maxStock = state.stock, state.maxStock
	}
	runtime.emitCastLifecycleEvent(cast, "cast_preparing")
	if !cast.program.cast.refundBeforeCommit {
		if err := runtime.payCosts(cast); err != nil {
			return err
		}
		cast.costsPaid = true
	}
	windupTicks, windupErr := runtime.castWindupTicks(cast)
	if windupErr != nil {
		return windupErr
	}
	commitDue := cast.windowStartTick + cast.program.cast.commitTick
	executeDue := cast.windowStartTick + windupTicks
	if commitDue == runtime.currentTick {
		if err := runtime.commitCast(cast); err != nil {
			return err
		}
	} else {
		frame := runtime.retainLocalFrame(cast.locals)
		if err := runtime.schedule(cast, commitDue, &castCommitTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame}); err != nil {
			return err
		}
	}
	if executeDue == runtime.currentTick {
		return runtime.beginCastExecution(cast)
	}
	frame := runtime.retainLocalFrame(cast.locals)
	return runtime.schedule(cast, executeDue, &castExecuteTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame})
}

func (runtime *Runtime) commitCast(cast *castInstance) error {
	if cast.windowStage == CastWindowCancelled || cast.committed {
		return nil
	}
	if cast.program.cast.mode == castModeAmmo && runtime.ammoState(cast).stock <= 0 {
		return ErrCastInputRejected
	}
	if !cast.costsPaid {
		if err := runtime.payCosts(cast); err != nil {
			return err
		}
		cast.costsPaid = true
	}
	if cast.program.cast.mode == castModeAmmo {
		state := runtime.ammoState(cast)
		state.stock--
		cast.stock, cast.maxStock = state.stock, state.maxStock
		runtime.ensureAmmoRecharge(cast, state)
	}
	cast.committed = true
	runtime.markAbilityCommitted(cast)
	runtime.emitCastPresentation(cast)
	cast.windowStage = CastWindowCommitted
	if gcd := cast.program.globalCooldownTicks; gcd > 0 {
		key := cooldownKey{Caster: cast.caster, Skill: globalCooldownProgramID}
		if due := runtime.currentTick + gcd; due > runtime.cooldowns[key] {
			runtime.cooldowns[key] = due
			runtime.touchCooldownLocked(key)
		}
	}
	if cast.program.cast.mode == castModeTap || cast.program.cast.mode == castModeAmmo || cast.program.cast.mode == castModeCharge {
		runtime.startCooldown(cast)
	}
	runtime.emitCastLifecycleEvent(cast, "cast_committed")
	return nil
}

func (runtime *Runtime) startCooldown(cast *castInstance) {
	if cast.cooldownStarted {
		return
	}
	cast.cooldownStarted = true
	runtime.cooldowns[cooldownKey{Caster: cast.cooldownOwner, Skill: cast.program.id}] = runtime.currentTick + cast.program.cooldownTicks
	runtime.touchCooldownLocked(cooldownKey{Caster: cast.cooldownOwner, Skill: cast.program.id})
	runtime.emitCastLifecycleEvent(cast, "cast_cooldown_started")
}

func (runtime *Runtime) beginCastExecution(cast *castInstance) error {
	if cast.windowStage == CastWindowCancelled {
		return nil
	}
	if !cast.committed {
		return ErrProgramInvariant
	}
	cast.windowStage = CastWindowExecuting
	cast.status = CastRunning
	runtime.emitCastLifecycleEvent(cast, "cast_executing")
	if cast.pendingRootEvent == "release" {
		return runtime.executeCastEvent(cast, "release")
	}
	return runtime.executeCast(cast)
}

func (runtime *Runtime) beginCastRecovery(cast *castInstance) error {
	if err := runtime.stopFinishingProcesses(cast); err != nil {
		_ = runtime.stopProcesses(cast, true)
		return err
	}
	cast.logicalFinished = true
	cast.windowStage = CastWindowRecovering
	if err := runtime.handoffEntityProcesses(cast); err != nil {
		_ = runtime.stopProcesses(cast, true)
		return err
	}
	if cast.pendingTasks > 0 {
		cast.status = CastSuspended
		return nil
	}
	runtime.emitCastLifecycleEvent(cast, "cast_recovering")
	recoveryTicks, recoveryErr := runtime.castRecoveryTicks(cast)
	if recoveryErr != nil {
		return recoveryErr
	}
	if recoveryTicks == 0 {
		return runtime.completeCastRecovery(cast)
	}
	frame := runtime.retainLocalFrame(cast.locals)
	return runtime.schedule(cast, runtime.currentTick+recoveryTicks, &castRecoveryTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame})
}

// castWindupTicks resolves the cast window's windup duration: the literal
// program value, or the windup expression sampled as the window is prepared.
// Expression results are clamped into the compile-time [min, max] bounds, so
// scheduled due ticks can never escape the declared worst case, and the
// commit_tick <= min invariant keeps commit before execute.
func (runtime *Runtime) castWindupTicks(cast *castInstance) (Tick, error) {
	window := cast.program.cast
	if !window.hasWindupExpression {
		return window.windupTicks, nil
	}
	return runtime.evalWindowTicks(cast, window.windupExpression, window.windupTicksMin, window.windupTicksMax)
}

// castRecoveryTicks resolves the recovery duration, sampled as recovery
// begins so the expression sees post-execution state.
func (runtime *Runtime) castRecoveryTicks(cast *castInstance) (Tick, error) {
	window := cast.program.cast
	if !window.hasRecoveryExpression {
		return window.recoveryTicks, nil
	}
	return runtime.evalWindowTicks(cast, window.recoveryExpression, window.recoveryTicksMin, window.recoveryTicksMax)
}

func (runtime *Runtime) evalWindowTicks(cast *castInstance, expression programValue, minimum, maximum Tick) (Tick, error) {
	value, err := runtime.evalValue(cast, expression)
	if err != nil {
		return 0, err
	}
	ticks, ok := value.Int()
	if !ok {
		return 0, ErrRuntimeTypeMismatch
	}
	result := Tick(ticks)
	if result < minimum {
		result = minimum
	}
	if result > maximum {
		result = maximum
	}
	return result, nil
}

func (runtime *Runtime) completeCastRecovery(cast *castInstance) error {
	if cast.windowStage == CastWindowCancelled {
		return nil
	}
	cast.windowStage = CastWindowComplete
	cast.status = CastFinished
	runtime.markAbilityCastFinished(cast)
	cast.policyActive = false
	delete(runtime.activePolicies, skillStateKey{Caster: cast.caster, Skill: cast.program.id})
	runtime.touchActivePolicyLocked(skillStateKey{Caster: cast.caster, Skill: cast.program.id})
	runtime.emitCastLifecycleEvent(cast, "cast_complete")
	return nil
}

func (runtime *Runtime) emitCastLifecycleEvent(cast *castInstance, kind string) {
	runtime.appendCastEvent(cast, RuntimeEvent{
		Tick: runtime.currentTick, Kind: kind, Entity: cast.caster,
		Context: EventContext{Tick: runtime.currentTick, Source: cast.caster, Owner: cast.caster, Target: cast.primaryTarget, SkillID: cast.program.id, CastID: cast.id},
	})
}

func (runtime *Runtime) Cancel(id CastID) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[id]
	if cast == nil {
		return ErrCastInputRejected
	}
	if cast.logicalFinished || cast.windowStage == CastWindowRecovering || cast.windowStage == CastWindowComplete || cast.windowStage == CastWindowCancelled {
		return ErrCastInputRejected
	}
	cast.releaseReason = "cancelled"
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	if cast.committed && (cast.program.cast.mode == castModeToggle || cast.program.cast.mode == castModeHold) {
		runtime.startCooldown(cast)
	}
	if operation, found := phaseRootOperation(cast.program, cast.program.phases[cast.currentPhase], "cancel"); found {
		if _, err := runtime.executeOperation(cast, operation); err != nil {
			return err
		}
	}
	if err := runtime.stopProcesses(cast, true); err != nil {
		return err
	}
	runtime.releasePolicySlot(cast)
	cast.windowStage = CastWindowCancelled
	cast.status = CastFinished
	runtime.markAbilityCastFinished(cast)
	runtime.emitCastLifecycleEvent(cast, "cast_cancelled")
	return nil
}

// releasePolicySlot clears a policy-holding cast's activePolicies entry so a
// cancelled or interrupted toggle/hold/charge neither blocks future
// activations nor pins the cast in memory (castEvictableLocked refuses
// policy-active casts). Guarded by ownership: a newer cast of the same skill
// may already hold the slot.
func (runtime *Runtime) releasePolicySlot(cast *castInstance) {
	if !cast.policyActive {
		return
	}
	cast.policyActive = false
	policyKey := skillStateKey{Caster: cast.caster, Skill: cast.program.id}
	if runtime.activePolicies[policyKey] == cast.id {
		delete(runtime.activePolicies, policyKey)
		runtime.touchActivePolicyLocked(policyKey)
	}
}

func (runtime *Runtime) Interrupt(id CastID, tag GameplayTagHandle) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[id]
	if cast == nil || !containsGameplayTag(cast.program.cast.interruptTags, tag) {
		return ErrCastInputRejected
	}
	if cast.logicalFinished || cast.windowStage == CastWindowRecovering || cast.windowStage == CastWindowComplete || cast.windowStage == CastWindowCancelled {
		return ErrCastInputRejected
	}
	cast.releaseReason = "cancelled"
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	if cast.committed && (cast.program.cast.mode == castModeToggle || cast.program.cast.mode == castModeHold) {
		runtime.startCooldown(cast)
	}
	if err := runtime.stopProcesses(cast, true); err != nil {
		return err
	}
	runtime.releasePolicySlot(cast)
	cast.windowStage = CastWindowCancelled
	cast.status = CastFinished
	runtime.markAbilityCastFinished(cast)
	runtime.emitCastLifecycleEvent(cast, "cast_interrupted")
	return nil
}

func (runtime *Runtime) Release(id CastID) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[id]
	if cast == nil {
		return ErrCastInputRejected
	}
	return runtime.releaseCast(cast, "input_release")
}

func (runtime *Runtime) releaseCast(cast *castInstance, reason string) error {
	if cast.logicalFinished || cast.windowStage == CastWindowRecovering || cast.windowStage == CastWindowComplete || cast.windowStage == CastWindowCancelled {
		return ErrCastInputRejected
	}
	if cast.program.cast.mode == castModeCharge {
		chargeBP := runtime.castChargeBP(cast)
		if chargeBP < cast.program.cast.minChargeBP {
			cast.releaseReason = "cancelled"
			cast.policyActive = false
			delete(runtime.activePolicies, skillStateKey{Caster: cast.caster, Skill: cast.program.id})
			runtime.touchActivePolicyLocked(skillStateKey{Caster: cast.caster, Skill: cast.program.id})
			runtime.cancelPhaseTasks(cast, cast.phaseToken)
			cast.windowStage = CastWindowCancelled
			cast.status = CastFinished
			runtime.markAbilityCastFinished(cast)
			runtime.emitCastLifecycleEvent(cast, "cast_cancelled")
			return nil
		}
		cast.releaseReason = reason
		runtime.cancelPhaseTasks(cast, cast.phaseToken)
		cast.phaseToken++
		cast.status = CastRunning
		return runtime.prepareCastWindow(cast, "release")
	}
	if cast.program.cast.mode != castModeToggle && cast.program.cast.mode != castModeHold {
		return ErrCastInputRejected
	}
	cast.releaseReason = reason
	cast.policyActive = false
	delete(runtime.activePolicies, skillStateKey{Caster: cast.caster, Skill: cast.program.id})
	runtime.touchActivePolicyLocked(skillStateKey{Caster: cast.caster, Skill: cast.program.id})
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	runtime.startCooldown(cast)
	return runtime.executeCastEvent(cast, "release")
}

func (runtime *Runtime) executeCastEvent(cast *castInstance, event string) error {
	phase := cast.program.phases[cast.currentPhase]
	operation, found := phaseRootOperation(cast.program, phase, event)
	if !found {
		return ErrProgramInvariant
	}
	control, err := runtime.executeOperation(cast, operation)
	if err != nil {
		return err
	}
	return runtime.resolveControl(cast, control)
}

func (runtime *Runtime) castChargeBP(cast *castInstance) int64 {
	maximum := cast.program.cast.maxChargeTicks
	if maximum <= 0 {
		return 0
	}
	elapsed := runtime.currentTick - cast.startTick
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > maximum {
		elapsed = maximum
	}
	return int64(elapsed) * 10000 / int64(maximum)
}

func (runtime *Runtime) ammoState(cast *castInstance) *skillState {
	key := skillStateKey{Caster: cast.caster, Skill: cast.program.id}
	// Recorded unconditionally: callers mutate the returned state in place,
	// and a spurious record on a read-only call diffs to nothing.
	runtime.touchSkillResourceLocked(key)
	state := runtime.skillStates[key]
	if state == nil {
		state = &skillState{stock: cast.program.cast.initialStock, maxStock: cast.program.cast.maxStock, rechargeTicks: cast.program.cast.rechargeTicks}
		runtime.skillStates[key] = state
	}
	return state
}

func (runtime *Runtime) ensureAmmoRecharge(cast *castInstance, state *skillState) {
	if state.rechargeScheduled || state.stock >= state.maxStock {
		return
	}
	runtime.touchSkillResourceLocked(skillStateKey{Caster: cast.caster, Skill: cast.program.id})
	state.rechargeScheduled = true
	state.rechargeGeneration++
	state.rechargeDue = runtime.currentTick + state.rechargeTicks
	_ = runtime.scheduleSystem(state.rechargeDue, &ammoRechargeTask{Caster: cast.caster, Skill: cast.program.id, Generation: state.rechargeGeneration})
}

func (runtime *Runtime) executeAmmoRecharge(task *ammoRechargeTask) error {
	state := runtime.skillStates[skillStateKey{Caster: task.Caster, Skill: task.Skill}]
	if state == nil || !state.rechargeScheduled || task.Generation != state.rechargeGeneration || runtime.currentTick != state.rechargeDue {
		return nil
	}
	runtime.touchSkillResourceLocked(skillStateKey{Caster: task.Caster, Skill: task.Skill})
	if state.stock < state.maxStock {
		state.stock++
	}
	// Keep the ability's snapshot-visible ammo cache in step with the
	// authoritative stock, as a recorded write point: without this, clients
	// polling ability state would not see recharges until the next commit.
	if handle, ok := runtime.abilityByProgram[skillStateKey{Caster: task.Caster, Skill: task.Skill}]; ok {
		if ability := runtime.abilities[abilityKey{owner: task.Caster, handle: handle}]; ability != nil {
			ability.ammoStock, ability.ammoMax = state.stock, state.maxStock
			runtime.touchAbilityLocked(abilityKey{owner: task.Caster, handle: handle})
		}
	}
	if state.stock >= state.maxStock {
		state.rechargeScheduled = false
		return nil
	}
	state.rechargeDue += state.rechargeTicks
	return runtime.scheduleSystem(state.rechargeDue, &ammoRechargeTask{Caster: task.Caster, Skill: task.Skill, Generation: task.Generation})
}

func (runtime *Runtime) CooldownUntil(program *Program, caster EntityID) Tick {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if program == nil {
		return 0
	}
	return runtime.cooldowns[cooldownKey{Caster: caster, Skill: program.id}]
}

func (runtime *Runtime) SkillStock(program *Program, caster EntityID) (int64, int64, bool) {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if program == nil || program.cast.mode != castModeAmmo {
		return 0, 0, false
	}
	state := runtime.skillStates[skillStateKey{Caster: caster, Skill: program.id}]
	if state == nil {
		return program.cast.initialStock, program.cast.maxStock, true
	}
	return state.stock, state.maxStock, true
}
