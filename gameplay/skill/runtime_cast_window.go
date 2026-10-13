package skill

// globalCooldownProgramID 是冷却表里保留的 program id，承载施法者的公共冷却。它不是合法的技能标识，不会与真实
// Program 冲突；它走普通冷却的全部机制，状态同步、状态变更与 checkpoint 都把它当作一条普通冷却条目。
const globalCooldownProgramID = "$gcd"

// casterWindowBusyLocked 报告施法者是否有施法处在独占窗口内（前摇、提交或后摇）。它扫描施法表而不维护计数：
// 窗口阶段在很多处改变，施法数有上限，扫描不会与实际状态脱节。
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
		cast.pendingRootEvent = phaseEventEnter
		runtime.emitCastLifecycleEvent(cast, "cast_charging")
		return runtime.executeCast(cast)
	}
	return runtime.prepareCastWindow(cast, phaseEventEnter)
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
	if cast.pendingRootEvent == phaseEventRelease {
		return runtime.executeCastEvent(cast, phaseEventRelease)
	}
	return runtime.executeCast(cast)
}

func (runtime *Runtime) beginCastRecovery(cast *castInstance) error {
	cast.logicalFinished = true
	cast.windowStage = CastWindowRecovering
	if err := runtime.handoffEntitySpawns(cast); err != nil {
		_ = runtime.stopCastSpawns(cast)
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

// castWindupTicks 求施法窗口的前摇时长：Program 里的字面值，或准备窗口时对前摇表达式采样。表达式结果夹在编译期的
// [min, max] 内，排程的到期 tick 不会超出声明的最坏情况；commit_tick <= min 的不变量保证提交先于执行。
func (runtime *Runtime) castWindupTicks(cast *castInstance) (Tick, error) {
	window := cast.program.cast
	if !window.hasWindupExpression {
		return window.windupTicks, nil
	}
	return runtime.evalWindowTicks(cast, window.windupExpression, window.windupTicksMin, window.windupTicksMax)
}

// castRecoveryTicks 求后摇时长，在后摇开始时采样，表达式看到的是执行之后的状态。
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

// Cancel 取消一个尚未结束的施法：撤掉当前 phase 的任务，已提交的 toggle / hold 开始冷却，跑 phase 的 cancel 流程、
// 停止名下衍生物、释放策略槽位，施法以 cancelled 窗口阶段结束。施法不存在或已结束时返回 ErrCastInputRejected；
// 取消途中出错时施法进入 failed 终态并返回该错误。
func (runtime *Runtime) Cancel(id CastID) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[id]
	if cast == nil || castEnded(cast) {
		return ErrCastInputRejected
	}
	cast.releaseReason = "cancelled"
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	if cast.committed && (cast.program.cast.mode == castModeToggle || cast.program.cast.mode == castModeHold) {
		runtime.startCooldown(cast)
	}
	// 走到这里 cast 已经被改动（任务撤了、token 推进了），出错必须进失败终态，不能停在半终止。
	if operation, found := phaseRootOperation(cast.program, cast.program.phases[cast.currentPhase], phaseEventCancel); found {
		if _, err := runtime.executeOperation(cast, operation); err != nil {
			return runtime.failCastLocked(cast, err)
		}
	}
	if err := runtime.stopCastSpawns(cast); err != nil {
		return runtime.failCastLocked(cast, err)
	}
	runtime.releasePolicySlot(cast)
	cast.windowStage = CastWindowCancelled
	cast.status = CastFinished
	runtime.markAbilityCastFinished(cast)
	runtime.emitCastLifecycleEvent(cast, "cast_cancelled")
	return nil
}

// releasePolicySlot 清掉持有策略的施法在 activePolicies 里的条目，被取消或打断的 toggle / hold / charge 既不挡住
// 之后的施放，也不把施法钉在内存里（castEvictableLocked 不回收策略生效中的施法）。只清自己持有的槽位：同一技能
// 更新的施法可能已经占了它。
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

// Interrupt 用打断标签打断施法：只有标签在 Program 的 interrupt 标签里时才生效，否则返回 ErrCastInputRejected。
// 与 Cancel 的区别是不跑 phase 的 cancel 流程，生命周期事件为 cast_interrupted。
func (runtime *Runtime) Interrupt(id CastID, tag GameplayTagHandle) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	runtime.beginStateMutationLocked()
	defer runtime.commitStateMutationsLocked()
	cast := runtime.casts[id]
	if cast == nil || !containsGameplayTag(cast.program.cast.interruptTags, tag) || castEnded(cast) {
		return ErrCastInputRejected
	}
	cast.releaseReason = "cancelled"
	runtime.cancelPhaseTasks(cast, cast.phaseToken)
	cast.phaseToken++
	if cast.committed && (cast.program.cast.mode == castModeToggle || cast.program.cast.mode == castModeHold) {
		runtime.startCooldown(cast)
	}
	if err := runtime.stopCastSpawns(cast); err != nil {
		return runtime.failCastLocked(cast, err)
	}
	runtime.releasePolicySlot(cast)
	cast.windowStage = CastWindowCancelled
	cast.status = CastFinished
	runtime.markAbilityCastFinished(cast)
	runtime.emitCastLifecycleEvent(cast, "cast_interrupted")
	return nil
}

// Release 送入一次松开输入，原因记为 input_release：charge 蓄力不足最小值时取消，否则进入释放窗口；toggle / hold
// 结束策略。其他施放模式或施法已结束时返回 ErrCastInputRejected。
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

// castEnded 报告 cast 是否已不接受 Cancel / Interrupt / Release：逻辑结束、进入恢复期、窗口已完成或已取消，
// 或已是 failed / finished 终态。只看窗口阶段会把排程失败的 cast（阶段仍是 executing）当成活的，
// 再对它执行 cancel / release 回调。
func castEnded(cast *castInstance) bool {
	if cast.status == CastFailed || cast.status == CastFinished || cast.logicalFinished {
		return true
	}
	switch cast.windowStage {
	case CastWindowRecovering, CastWindowComplete, CastWindowCancelled:
		return true
	}
	return false
}

// releaseCast 处理 Release、toggle 二次激活与排程的自动释放 / 资源耗尽释放。前置检查之后的任何错误都让 cast
// 进入失败终态：charge 已撤掉 auto release 任务、阶段回到 preparing，原样返回会永久占住施法者窗口。
func (runtime *Runtime) releaseCast(cast *castInstance, reason string) error {
	if castEnded(cast) {
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
		if err := runtime.prepareCastWindow(cast, phaseEventRelease); err != nil {
			return runtime.failCastLocked(cast, err)
		}
		return nil
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
	if err := runtime.executeCastEvent(cast, phaseEventRelease); err != nil {
		return runtime.failCastLocked(cast, err)
	}
	return nil
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
	// 无条件记录写入点：调用方会原地修改返回的状态；只读调用多记一次，差分结果为空。
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
	// 让技能实例在快照里可见的弹药缓存与权威库存同步，并记为写入点；否则轮询技能状态的客户端要到下一次提交才看到回复。
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

// CooldownUntil 返回施法者这个技能冷却结束的 tick；没有冷却时返回 0。
func (runtime *Runtime) CooldownUntil(program *Program, caster EntityID) Tick {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if program == nil {
		return 0
	}
	return runtime.cooldowns[cooldownKey{Caster: caster, Skill: program.id}]
}

// SkillStock 返回弹药模式技能的当前库存与上限；技能不是弹药模式时第三个返回值为 false。
// 还没有库存记录时返回 Program 声明的初始库存。
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
