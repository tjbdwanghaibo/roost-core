package skill

import "github.com/tjbdwanghaibo/roost-skill/combat"

// MemoryHost delegates all combat arithmetic to the combat package so the
// reference host and production hosts run the same twelve-stage pipeline.
// This file keeps only host bookkeeping: entity lookup, status-backed hooks,
// event emission, and revision commits.

// memoryCombatHooks adapts MemoryHost's status storage to combat.Hooks. Each
// Peek remembers which entity owns the peeked hook, because ConsumeHook is
// keyed by hook name only and always follows the matching Peek.
type memoryCombatHooks struct {
	host       *MemoryHost
	source     EntityID
	target     EntityID
	peekedFrom map[string]EntityID
}

func (hooks *memoryCombatHooks) peek(entity EntityID, names ...string) (string, bool) {
	_, hook, ok := hooks.host.firstCombatHookStatusLocked(entity, names...)
	if ok {
		if hooks.peekedFrom == nil {
			hooks.peekedFrom = map[string]EntityID{}
		}
		hooks.peekedFrom[hook] = entity
	}
	return hook, ok
}

func (hooks *memoryCombatHooks) PeekSpellShield() (string, bool) {
	return hooks.peek(hooks.target, "spell_shield")
}

func (hooks *memoryCombatHooks) PeekCriticalOverride() (string, bool) {
	return hooks.peek(hooks.source, "critical_override")
}

func (hooks *memoryCombatHooks) PeekDeathPrevention() (string, bool) {
	return hooks.peek(hooks.target, "death_prevention", "execute_immunity")
}

func (hooks *memoryCombatHooks) ConsumeHook(hook string) {
	hooks.host.consumeCombatHookStatusLocked(hooks.peekedFrom[hook], hook)
}

func (hooks *memoryCombatHooks) OnShieldAbsorbed(absorbed int64) {
	remaining := absorbed
	statusRemove := map[int]bool{}
	for index := range hooks.host.statuses {
		instance := &hooks.host.statuses[index]
		policy, _ := hooks.host.statusPolicy(instance.status)
		if instance.target != hooks.target || policy.Category != "shield" || remaining <= 0 {
			continue
		}
		drained := minInt64(instance.shield, remaining)
		instance.shield -= drained
		remaining -= drained
		if instance.shield == 0 {
			statusRemove[index] = true
		}
	}
	if len(statusRemove) > 0 {
		hooks.host.filterStatusesLocked(statusRemove)
	}
}

func combatantFromMemoryEntity(entity MemoryEntity, element ElementHandle) combat.Combatant {
	combatant := combat.Combatant{
		Alive: entity.Alive, Health: entity.Health, MaxHealth: entity.MaxHealth, Shield: entity.Shield,
		Armor: entity.Armor, MagicResistance: entity.MagicResistance, Penetration: entity.Penetration,
		DamageDealtBP: entity.DamageDealtBP, DamageTakenBP: entity.DamageTakenBP,
		CriticalMultiplierBP: entity.CriticalMultiplierBP, VampBP: entity.VampBP,
		DamageCap: entity.DamageCap, MinimumDamage: entity.MinimumDamage,
		Dodge: entity.Dodge, Parry: entity.Parry, Block: entity.Block,
		DamageImmune: entity.DamageImmune, SpellShield: entity.SpellShield, ForceCritical: entity.ForceCritical,
	}
	if multiplier := entity.ElementMultipliers[element]; multiplier != 0 {
		combatant.ElementMultipliersBP = map[combat.Element]int64{combat.Element(element): multiplier}
	}
	return combatant
}

func (host *MemoryHost) applyDamageLocked(command DamageCommand) (EffectResult, error) {
	if len(fixedCombatPipeline) != len(combat.PipelineStages) {
		panic("skill: combat pipeline invariant violated")
	}
	if policy := host.gameplay.Combat.FormulaPolicy; policy != "" && policy != "twelve_stage_v1" {
		return EffectResult{}, ErrCombatPolicyUnsupported
	}
	if len(host.gameplay.Combat.DamageTypes) > 0 && !containsDamageTypeHandle(host.gameplay.Combat.DamageTypes, command.DamageType) {
		return EffectResult{}, ErrCombatHandleInvalid
	}
	if len(host.gameplay.Elements.Entries) > 0 && !containsElementHandle(host.gameplay.Elements.Entries, command.Element) {
		return EffectResult{}, ErrCombatHandleInvalid
	}
	target, ok := host.entities[command.Target]
	if !ok || !target.Alive {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: DamageEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	source := MemoryEntity{}
	if command.Source != 0 {
		var sourceExists bool
		source, sourceExists = host.entities[command.Source]
		if !sourceExists {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: DamageEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
		}
	}

	sourceCombatant := combatantFromMemoryEntity(source, command.Element)
	targetCombatant := combatantFromMemoryEntity(target, command.Element)
	hooks := &memoryCombatHooks{host: host, source: command.Source, target: command.Target}
	input := combat.DamageInput{
		Amount: command.Amount, Type: combat.DamageType(command.DamageType), Element: combat.Element(command.Element),
		CanCritical: command.CanCritical,
		SpellTagged: host.spellTag != 0 && containsGameplayTag(command.Tags, host.spellTag),
	}
	var sourcePointer *combat.Combatant
	if command.Source != 0 {
		sourcePointer = &sourceCombatant
	}
	outcome, _ := combat.ResolveDamage(sourcePointer, &targetCombatant, input, hooks)

	result := DamageResult{
		Attempted: outcome.Attempted, Mitigated: outcome.Mitigated, Absorbed: outcome.Absorbed,
		HealthDamage: outcome.HealthDamage,
		Dodged:       outcome.Dodged, Parried: outcome.Parried, Blocked: outcome.Blocked,
		Critical: outcome.Critical, Immune: outcome.Immune, Killed: outcome.Killed,
		CombatHooks: outcome.CombatHooks,
	}
	context := command.Event
	context.Source, context.Owner, context.Target = command.Source, command.Owner, command.Target
	context.EffectIndex, context.DamageType, context.Element = command.Meta.EffectIndex, command.DamageType, command.Element
	context.Result = outcome.Result

	// Write source before target so a self-vamp keeps the historical
	// resolution order (the target write wins on overlap).
	if command.Source != 0 && outcome.VampHeal > 0 {
		source.Health = sourceCombatant.Health
		host.entities[command.Source] = source
	}
	if !outcome.Immune || target.SpellShield {
		target.Health, target.Shield = targetCombatant.Health, targetCombatant.Shield
		target.Alive, target.SpellShield = targetCombatant.Alive, targetCombatant.SpellShield
		host.entities[command.Target] = target
	}
	return host.commitDamageLocked(command, result, context), nil
}

func containsDamageTypeHandle(values []DamageTypeHandle, want DamageTypeHandle) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsElementHandle(values []ElementCatalogEntry, want ElementHandle) bool {
	for _, value := range values {
		if value.Handle == want {
			return true
		}
	}
	return false
}

func (host *MemoryHost) commitDamageLocked(command DamageCommand, result DamageResult, context EventContext) EffectResult {
	tags := append([]GameplayTagHandle(nil), command.Tags...)
	if result.Critical && host.criticalTag != 0 {
		tags = append(tags, host.criticalTag)
	}
	context = context.WithGameplayTags(tags)
	host.revision++
	host.appendContextEventLocked("damage_resolved", command.Target, 0, context)
	for _, hook := range result.CombatHooks {
		hookContext := context
		hookContext.Result = hook
		host.appendContextEventLocked("combat_hook_"+hook, command.Target, 0, hookContext)
	}
	if result.Absorbed > 0 {
		host.appendContextEventLocked("shield_absorbed", command.Target, 0, context)
		if host.entities[command.Target].Shield == 0 {
			host.appendContextEventLocked("shield_broken", command.Target, 0, context)
		}
	}
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: DamageEffectResult{ResultOutcome: successfulResultOutcome(), Result: result}}
}

func (host *MemoryHost) firstCombatHookStatusLocked(target EntityID, hooks ...string) (int, string, bool) {
	wanted := make(map[string]bool, len(hooks))
	for _, hook := range hooks {
		wanted[hook] = true
	}
	selectedIndex := -1
	selectedSequence := ^uint64(0)
	selectedHook := ""
	for index, instance := range host.statuses {
		if instance.target != target || instance.sequence >= selectedSequence {
			continue
		}
		policy, ok := host.statusPolicy(instance.status)
		if !ok {
			continue
		}
		for _, hook := range policy.CombatHooks {
			if wanted[hook] {
				selectedIndex, selectedSequence, selectedHook = index, instance.sequence, hook
				break
			}
		}
	}
	return selectedIndex, selectedHook, selectedIndex >= 0
}

func (host *MemoryHost) consumeCombatHookStatusLocked(target EntityID, hook string) bool {
	index, _, ok := host.firstCombatHookStatusLocked(target, hook)
	if !ok {
		return false
	}
	policy, ok := host.statusPolicy(host.statuses[index].status)
	if !ok || policy.RemovalPolicy != "consume" {
		return false
	}
	host.filterStatusesLocked(map[int]bool{index: true})
	return true
}

func (host *MemoryHost) applyHealLocked(command HealCommand) (EffectResult, error) {
	target, ok := host.entities[command.Target]
	if !ok || !target.Alive {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: HealEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	targetCombatant := combatantFromMemoryEntity(target, 0)
	outcome, _ := combat.ResolveHeal(&targetCombatant, command.Amount)
	target.Health = targetCombatant.Health
	host.entities[command.Target] = target
	context := command.Event
	context.Source, context.Target, context.Result = command.Source, command.Target, "healed"
	host.revision++
	host.appendContextEventLocked("heal_resolved", command.Target, 0, context)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: HealEffectResult{ResultOutcome: successfulResultOutcome(), Result: HealResult{Attempted: outcome.Attempted, Effective: outcome.Effective}}}, nil
}

func (host *MemoryHost) applyShieldLocked(command ShieldCommand) (EffectResult, error) {
	target, ok := host.entities[command.Target]
	if !ok || !target.Alive {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: ShieldEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	targetCombatant := combatantFromMemoryEntity(target, 0)
	added, _ := combat.AddShield(&targetCombatant, command.Amount)
	target.Shield = targetCombatant.Shield
	shieldStatus := StatusHandle(0)
	for _, entry := range host.gameplay.Statuses.Entries {
		if entry.Category == "shield" {
			shieldStatus = entry.Handle
			break
		}
	}
	if shieldStatus == 0 || command.DurationTicks <= 0 {
		return EffectResult{}, ErrHostContractViolation
	}
	shieldPolicy, _ := host.statusPolicy(shieldStatus)
	duration := command.DurationTicks
	if shieldPolicy.MaximumDurationTicks > 0 && duration > shieldPolicy.MaximumDurationTicks {
		duration = shieldPolicy.MaximumDurationTicks
	}
	host.nextInstanceSequence++
	host.statuses = append(host.statuses, statusInstance{target: command.Target, status: shieldStatus, sourceOwner: command.Source, sourceSkill: command.SourceSkill, sourceCast: command.SourceCast, effect: command.Meta.EffectIndex, sequence: host.nextInstanceSequence, appliedTick: host.tick, stacks: 1, dueTick: saturatingTickAdd(host.tick, duration), shield: added})
	if target.Statuses == nil {
		target.Statuses = map[StatusHandle]bool{}
	}
	target.Statuses[shieldStatus] = true
	host.entities[command.Target] = target
	context := command.Event
	context.Source, context.Target, context.Result = command.Source, command.Target, "shielded"
	host.revision++
	host.appendContextEventLocked("shield_resolved", command.Target, 0, context)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: ShieldEffectResult{ResultOutcome: successfulResultOutcome(), Result: ShieldResult{Added: added}}}, nil
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func scaleBasisPoints(value, basisPoints int64) int64 {
	return combat.ScaleBasisPoints(value, basisPoints)
}
