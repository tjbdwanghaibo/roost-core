package skill

import (
	"fmt"
	"sort"
)

func (host *MemoryHost) applyStatusLocked(command StatusCommand) (EffectResult, error) {
	entity, ok := host.entities[command.Target]
	if !ok || !entity.Alive {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	policy, ok := host.statusPolicy(command.Status)
	if !ok {
		return EffectResult{}, fmt.Errorf("skill: unknown status handle %d", command.Status)
	}
	context := command.Event
	context.Owner, context.Target, context.EffectIndex = command.SourceOwner, command.Target, command.Meta.EffectIndex
	previousStacks := host.statusStacksLocked(command.Target, command.Status, 0)
	if policy.Category == "control" {
		if _, hook, immune := host.firstCombatHookStatusLocked(command.Target, "control_immunity"); immune {
			context.Result = hook
			host.consumeCombatHookStatusLocked(command.Target, hook)
			host.revision++
			host.appendContextEventLocked("combat_hook_"+hook, command.Target, 0, context)
			host.appendContextEventLocked("status_immune", command.Target, 0, context)
			return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Immune: true, PreviousStacks: previousStacks, CurrentStacks: previousStacks, CombatHooks: []string{hook}}}}, nil
		}
	}
	for _, immunity := range policy.ImmunityTags {
		if host.entityHasGameplayTagLocked(command.Target, immunity) {
			context.Result = combatResultImmune
			host.revision++
			host.appendContextEventLocked("status_immune", command.Target, 0, context)
			return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Immune: true, PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
		}
	}
	duration := command.DurationTicks
	if duration <= 0 {
		return EffectResult{}, fmt.Errorf("skill: status duration must be positive")
	}
	if policy.TenacityPolicy == "scale_duration" && entity.TenacityBP > 0 {
		factor := maxInt64(0, 10000-entity.TenacityBP)
		duration = Tick(maxInt64(1, scaleBasisPoints(int64(duration), factor)))
	}
	if policy.MaximumDurationTicks > 0 && duration > policy.MaximumDurationTicks {
		duration = policy.MaximumDurationTicks
	}
	due := saturatingTickAdd(host.tick, duration)
	stacks := command.Stacks
	if stacks <= 0 {
		stacks = 1
	}
	maximum := policy.MaxStacks
	if command.MaxStacks > 0 && (maximum <= 0 || command.MaxStacks < maximum) {
		maximum = command.MaxStacks
	}
	if maximum <= 0 {
		maximum = 1
	}

	match := -1
	for index, instance := range host.statuses {
		if instance.target == command.Target && instance.status == command.Status && instance.sourceOwner == command.SourceOwner && instance.sourceSkill == command.SourceSkill && instance.sourceCast == command.SourceCast && instance.effect == command.Meta.EffectIndex {
			match = index
			break
		}
	}
	if policy.RefreshPolicy == "replace" {
		host.removeStatusInstancesLocked(command.Target, command.Status, 0)
		match = -1
	}
	if match >= 0 {
		instance := host.statuses[match]
		instance.stacks = minInt(maximum, saturatingAdd(instance.stacks, stacks))
		instance.dueTick = due
		host.statuses[match] = instance
		stacks = instance.stacks
	} else {
		host.nextInstanceSequence++
		stacks = minInt(maximum, stacks)
		host.statuses = append(host.statuses, statusInstance{target: command.Target, status: command.Status, sourceOwner: command.SourceOwner, sourceSkill: command.SourceSkill, sourceCast: command.SourceCast, effect: command.Meta.EffectIndex, sequence: host.nextInstanceSequence, appliedTick: host.tick, stacks: stacks, dueTick: due})
	}
	if entity.Statuses == nil {
		entity.Statuses = make(map[StatusHandle]bool)
	}
	entity.Statuses[command.Status] = true
	host.entities[command.Target] = entity
	context.Result = "status_applied"
	host.revision++
	host.appendContextEventLocked("status_applied", command.Target, 0, context)
	currentStacks := host.statusStacksLocked(command.Target, command.Status, 0)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Applied: true, PreviousStacks: previousStacks, CurrentStacks: currentStacks, DueTick: due}}}, nil
}

func (host *MemoryHost) entityHasGameplayTagLocked(target EntityID, tag GameplayTagHandle) bool {
	if host.entities[target].GameplayTags[tag] {
		return true
	}
	for _, instance := range host.statuses {
		if instance.target != target {
			continue
		}
		policy, ok := host.statusPolicy(instance.status)
		if !ok {
			continue
		}
		for _, candidate := range policy.GameplayTags {
			if candidate == tag {
				return true
			}
		}
	}
	return false
}

func (host *MemoryHost) removeStatusLocked(command RemoveStatusCommand) (EffectResult, error) {
	if _, ok := host.entities[command.Target]; !ok {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	previousStacks := host.statusStacksLocked(command.Target, command.Status, command.SourceOwner)
	removed := host.removeStatusInstancesLocked(command.Target, command.Status, command.SourceOwner)
	if removed == 0 {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
	}
	host.revision++
	context := command.Event
	context.Owner, context.Target, context.Result = command.SourceOwner, command.Target, "status_removed"
	host.appendContextEventLocked("status_removed", command.Target, 0, context)
	currentStacks := host.statusStacksLocked(command.Target, command.Status, command.SourceOwner)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Removed: true, PreviousStacks: previousStacks, CurrentStacks: currentStacks, RemovedStacks: previousStacks - currentStacks}}}, nil
}

func (host *MemoryHost) dispelStatusLocked(command DispelStatusCommand) (EffectResult, error) {
	if _, ok := host.entities[command.Target]; !ok {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	type candidate struct {
		index, priority int
		sequence        uint64
	}
	var candidates []candidate
	for index, instance := range host.statuses {
		policy, ok := host.statusPolicy(instance.status)
		if instance.target == command.Target && ok && policy.Dispellable && policy.DispelCategory == command.Category {
			candidates = append(candidates, candidate{index: index, priority: policy.DispelPriority, sequence: instance.sequence})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].sequence < candidates[j].sequence
	})
	count := command.Count
	if count <= 0 || count > len(candidates) {
		count = len(candidates)
	}
	previousStacks := 0
	for _, candidate := range candidates {
		previousStacks = saturatingAdd(previousStacks, host.statuses[candidate.index].stacks)
	}
	remove := make(map[int]bool, count)
	removedStacks := 0
	for _, candidate := range candidates[:count] {
		remove[candidate.index] = true
		removedStacks = saturatingAdd(removedStacks, host.statuses[candidate.index].stacks)
	}
	host.filterStatusesLocked(remove)
	if count == 0 {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
	}
	host.revision++
	context := command.Event
	context.Target, context.Result = command.Target, "status_dispelled"
	host.appendContextEventLocked("status_dispelled", command.Target, 0, context)
	currentStacks := previousStacks - removedStacks
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Removed: true, PreviousStacks: previousStacks, CurrentStacks: currentStacks, RemovedStacks: removedStacks}}}, nil
}

func (host *MemoryHost) applyAttributeModifierLocked(command AttributeModifierCommand) (EffectResult, error) {
	if _, ok := host.entities[command.Target]; !ok {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: AttributeModifierEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	if command.Operation != "add" && command.Operation != "mul_bp" {
		return EffectResult{}, fmt.Errorf("skill: unsupported modifier operation %q", command.Operation)
	}
	if command.DurationTicks <= 0 {
		return EffectResult{}, fmt.Errorf("skill: modifier duration must be positive")
	}
	host.nextInstanceSequence++
	due := saturatingTickAdd(host.tick, command.DurationTicks)
	host.modifiers = append(host.modifiers, attributeModifierInstance{target: command.Target, attribute: command.Attribute, sourceOwner: command.SourceOwner, sourceSkill: command.SourceSkill, sourceCast: command.SourceCast, effect: command.Meta.EffectIndex, sequence: host.nextInstanceSequence, operation: command.Operation, value: command.Value, dueTick: due})
	host.revision++
	context := command.Event
	context.Owner, context.Target, context.Result = command.SourceOwner, command.Target, "attribute_modifier_applied"
	host.appendContextEventLocked("attribute_modifier_applied", command.Target, 0, context)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: AttributeModifierEffectResult{ResultOutcome: successfulResultOutcome(), Result: AttributeModifierResult{Applied: true, DueTick: due}}}, nil
}

func (host *MemoryHost) expireDueLocked() []RuntimeEvent {
	type expiration struct {
		due      Tick
		sequence uint64
		status   *statusInstance
		modifier *attributeModifierInstance
	}
	var due []expiration
	for index := range host.statuses {
		if host.statuses[index].dueTick <= host.tick {
			instance := host.statuses[index]
			due = append(due, expiration{due: instance.dueTick, sequence: instance.sequence, status: &instance})
		}
	}
	for index := range host.modifiers {
		if host.modifiers[index].dueTick <= host.tick {
			instance := host.modifiers[index]
			due = append(due, expiration{due: instance.dueTick, sequence: instance.sequence, modifier: &instance})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].due != due[j].due {
			return due[i].due < due[j].due
		}
		return due[i].sequence < due[j].sequence
	})
	statusRemove := map[int]bool{}
	for index, instance := range host.statuses {
		if instance.dueTick <= host.tick {
			statusRemove[index] = true
		}
	}
	host.filterStatusesLocked(statusRemove)
	modifierWrite := 0
	for _, instance := range host.modifiers {
		if instance.dueTick > host.tick {
			host.modifiers[modifierWrite] = instance
			modifierWrite++
		}
	}
	host.modifiers = host.modifiers[:modifierWrite]
	for _, expiration := range due {
		if expiration.status != nil {
			host.appendContextEventLocked("status_expired", expiration.status.target, 0, EventContext{Target: expiration.status.target, Result: "status_expired"})
		} else {
			host.appendContextEventLocked("attribute_modifier_expired", expiration.modifier.target, 0, EventContext{Target: expiration.modifier.target, Result: "attribute_modifier_expired"})
		}
	}
	return nil
}

func (host *MemoryHost) filterStatusesLocked(remove map[int]bool) {
	touched := map[EntityID]bool{}
	write := 0
	for index, instance := range host.statuses {
		if remove[index] {
			touched[instance.target] = true
			if policy, ok := host.statusPolicy(instance.status); ok && policy.Category == "shield" && instance.shield > 0 {
				entity := host.entities[instance.target]
				entity.Shield = maxInt64(0, entity.Shield-instance.shield)
				host.entities[instance.target] = entity
			}
			continue
		}
		host.statuses[write] = instance
		write++
	}
	host.statuses = host.statuses[:write]
	for target := range touched {
		entity := host.entities[target]
		for status := range entity.Statuses {
			entity.Statuses[status] = false
		}
		for _, instance := range host.statuses {
			if instance.target == target {
				entity.Statuses[instance.status] = true
			}
		}
		host.entities[target] = entity
	}
}

func (host *MemoryHost) removeStatusInstancesLocked(target EntityID, status StatusHandle, sourceOwner EntityID) int {
	remove := map[int]bool{}
	for index, instance := range host.statuses {
		if instance.target == target && instance.status == status && (sourceOwner == 0 || instance.sourceOwner == sourceOwner) {
			remove[index] = true
		}
	}
	host.filterStatusesLocked(remove)
	return len(remove)
}

func (host *MemoryHost) statusStacksLocked(target EntityID, status StatusHandle, sourceOwner EntityID) int {
	result := 0
	for _, instance := range host.statuses {
		if instance.target == target && instance.status == status && (sourceOwner == 0 || instance.sourceOwner == sourceOwner) {
			result = saturatingAdd(result, instance.stacks)
		}
	}
	return result
}

func (host *MemoryHost) modifyStatusInstanceLocked(command ModifyStatusInstanceCommand) (EffectResult, error) {
	index := -1
	for candidate := range host.statuses {
		instance := host.statuses[candidate]
		if instance.sequence == command.Status.ID.OpaqueID() && instance.target == command.Status.Target {
			index = candidate
			break
		}
	}
	if index < 0 {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureReferenceExpired)}}, nil
	}
	instance := host.statuses[index]
	policy, ok := host.statusPolicy(instance.status)
	if !ok {
		return EffectResult{}, ErrHostContractViolation
	}
	authorized := policy.SourceOwnership == "owner" && command.Owner == instance.target || policy.SourceOwnership != "owner" && command.Owner == instance.sourceOwner
	if command.Operation == "remove" && command.Owner == instance.target {
		authorized = true
	}
	if command.Operation == "transfer_to" && policy.Stealable {
		authorized = true
	}
	if !authorized {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePermissionDenied)}}, nil
	}
	beforeStacks, beforeDue := instance.stacks, instance.dueTick
	if policy.MaximumDurationTicks > 0 && instance.dueTick-host.tick > policy.MaximumDurationTicks {
		instance.dueTick = saturatingTickAdd(host.tick, policy.MaximumDurationTicks)
	}
	maximumStacks := policy.MaxStacks
	if maximumStacks <= 0 {
		maximumStacks = 1
	}
	created := StatusInstanceRef{}
	remove := false
	switch command.Operation {
	case "remove":
		if !policy.Dispellable {
			return host.statusPolicyFailureLocked(), nil
		}
		remove = true
	case "add_stacks":
		instance.stacks = minInt(maximumStacks, max(0, saturatingAdd(instance.stacks, int(command.Value))))
	case "set_stacks":
		instance.stacks = minInt(maximumStacks, max(0, int(command.Value)))
	case "add_duration":
		if len(policy.DurationOperations) > 0 && !containsString(policy.DurationOperations, command.Operation) {
			return host.statusPolicyFailureLocked(), nil
		}
		instance.dueTick = saturatingTickAdd(host.tick, Tick(maxInt64(1, saturatingInt64Add(int64(max(Tick(0), instance.dueTick-host.tick)), command.Value))))
	case "set_duration":
		if len(policy.DurationOperations) > 0 && !containsString(policy.DurationOperations, command.Operation) {
			return host.statusPolicyFailureLocked(), nil
		}
		instance.dueTick = saturatingTickAdd(host.tick, Tick(maxInt64(1, command.Value)))
	case "mul_duration_bp":
		if len(policy.DurationOperations) > 0 && !containsString(policy.DurationOperations, command.Operation) {
			return host.statusPolicyFailureLocked(), nil
		}
		instance.dueTick = saturatingTickAdd(host.tick, Tick(maxInt64(1, scaleBasisPoints(int64(max(Tick(0), instance.dueTick-host.tick)), command.Value))))
	case "refresh":
		if len(policy.DurationOperations) > 0 && !containsString(policy.DurationOperations, command.Operation) {
			return host.statusPolicyFailureLocked(), nil
		}
		instance.dueTick = saturatingTickAdd(host.tick, max(Tick(1), beforeDue-instance.appliedTick))
	case "copy_to", "transfer_to":
		allowed := command.Operation == "copy_to" && policy.Copyable || command.Operation == "transfer_to" && policy.Transferable
		if command.Target == 0 || !host.entities[command.Target].Alive {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
		}
		if !allowed || !validStatusOwnershipPolicy(command.OwnershipPolicy) {
			return host.statusPolicyFailureLocked(), nil
		}
		for _, immunity := range policy.ImmunityTags {
			if host.entityHasGameplayTagLocked(command.Target, immunity) {
				return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Immune: true, PreviousStacks: beforeStacks, CurrentStacks: beforeStacks, DueTick: beforeDue, Status: command.Status}}}, nil
			}
		}
		host.nextInstanceSequence++
		copy := instance
		copy.sequence, copy.target, copy.appliedTick = host.nextInstanceSequence, command.Target, host.tick
		switch command.OwnershipPolicy {
		case "original_owner":
			copy.sourceOwner = instance.target
		case "new_owner":
			copy.sourceOwner = command.Target
			copy.sourceSkill = command.SourceSkillID
		case "new_source":
			copy.sourceOwner = command.Owner
			copy.sourceSkill = command.SourceSkillID
		}
		host.statuses = append(host.statuses, copy)
		created = StatusInstanceRef{ID: StatusInstanceID{opaque: copy.sequence}, Target: copy.target}
		remove = command.Operation == "transfer_to"
	default:
		return EffectResult{}, ErrHostContractViolation
	}
	if instance.stacks == 0 {
		remove = true
	}
	if policy.MaximumDurationTicks > 0 && instance.dueTick-host.tick > policy.MaximumDurationTicks {
		instance.dueTick = saturatingTickAdd(host.tick, policy.MaximumDurationTicks)
	}
	if remove {
		host.filterStatusesLocked(map[int]bool{index: true})
	} else {
		host.statuses[index] = instance
	}
	if created.ID.OpaqueID() != 0 {
		entity := host.entities[created.Target]
		if entity.Statuses == nil {
			entity.Statuses = map[StatusHandle]bool{}
		}
		entity.Statuses[instance.status] = true
		if policy.Category == "shield" {
			entity.Shield = saturatingInt64Add(entity.Shield, instance.shield)
		}
		host.entities[created.Target] = entity
	}
	context := command.Event
	context.Owner, context.Target, context.Result = command.Owner, instance.target, "status_instance_"+command.Operation
	host.revision++
	host.appendContextEventLocked("status_instance_"+command.Operation, instance.target, 0, context)
	receipt := CommitReceipt{Revision: host.revision, Changed: true}
	currentStacks := instance.stacks
	if remove {
		currentStacks = 0
	}
	return EffectResult{Commit: receipt, Payload: StatusEffectResult{ResultOutcome: successfulResultOutcome(), Result: StatusResult{Applied: true, Removed: remove, PreviousStacks: beforeStacks, CurrentStacks: currentStacks, RemovedStacks: max(0, beforeStacks-currentStacks), DueTick: instance.dueTick, PreviousDueTick: beforeDue, Status: command.Status, Created: created}}}, nil
}

func (host *MemoryHost) statusPolicyFailureLocked() EffectResult {
	return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: StatusEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePolicyRejected)}}
}

func (host *MemoryHost) statusPolicy(handle StatusHandle) (StatusCatalogEntry, bool) {
	for _, entry := range host.gameplay.Statuses.Entries {
		if entry.Handle == handle {
			return entry, true
		}
	}
	return StatusCatalogEntry{}, false
}

func (host *MemoryHost) StatusStacksForTest(target EntityID, status StatusHandle) int {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	result := 0
	for _, instance := range host.statuses {
		if instance.target == target && instance.status == status {
			result = saturatingAdd(result, instance.stacks)
		}
	}
	return result
}

func (host *MemoryHost) EffectiveAttributeForTest(target EntityID, attribute AttributeHandle) int64 {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	return host.effectiveAttributeLocked(target, attribute)
}

func (host *MemoryHost) effectiveAttributeLocked(target EntityID, attribute AttributeHandle) int64 {
	value := host.entities[target].Attributes[attribute]
	type modifierValue struct {
		sequence uint64
		value    int64
	}
	var additions, multipliers []modifierValue
	for _, status := range host.statuses {
		if status.target != target {
			continue
		}
		policy, ok := host.statusPolicy(status.status)
		if !ok {
			continue
		}
		for _, modifier := range policy.AttributeModifiers {
			if modifier.Attribute != attribute {
				continue
			}
			if modifier.Operation == "add" {
				additions = append(additions, modifierValue{sequence: status.sequence, value: saturatingInt64Mul(modifier.Value, int64(status.stacks))})
			} else if modifier.Operation == "mul_bp" {
				for stack := 0; stack < status.stacks; stack++ {
					multipliers = append(multipliers, modifierValue{sequence: status.sequence, value: modifier.Value})
				}
			}
		}
	}
	for _, modifier := range host.modifiers {
		if modifier.target == target && modifier.attribute == attribute && modifier.operation == "add" {
			additions = append(additions, modifierValue{sequence: modifier.sequence, value: modifier.value})
		}
		if modifier.target == target && modifier.attribute == attribute && modifier.operation == "mul_bp" {
			multipliers = append(multipliers, modifierValue{sequence: modifier.sequence, value: modifier.value})
		}
	}
	sort.SliceStable(additions, func(i, j int) bool { return additions[i].sequence < additions[j].sequence })
	sort.SliceStable(multipliers, func(i, j int) bool { return multipliers[i].sequence < multipliers[j].sequence })
	for _, modifier := range additions {
		value = saturatingInt64Add(value, modifier.value)
	}
	rounding := "toward_zero"
	for _, entry := range host.gameplay.Attributes.Entries {
		if entry.Handle == attribute {
			rounding = entry.Rounding
			for _, modifier := range multipliers {
				value = scaleBasisPointsRounded(value, modifier.value, rounding)
			}
			value = maxInt64(entry.Minimum, minInt64(entry.Maximum, value))
			break
		}
	}
	if len(host.gameplay.Attributes.Entries) == 0 {
		for _, modifier := range multipliers {
			value = scaleBasisPointsRounded(value, modifier.value, rounding)
		}
	}
	return value
}

func scaleBasisPointsRounded(value, basisPoints int64, rounding string) int64 {
	product := saturatingInt64Mul(value, basisPoints)
	quotient, remainder := product/10000, product%10000
	if rounding == "half_away_from_zero" && absoluteDifference(remainder, 0) >= 5000 {
		if product < 0 {
			return saturatingInt64Add(quotient, -1)
		}
		return saturatingInt64Add(quotient, 1)
	}
	return quotient
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
