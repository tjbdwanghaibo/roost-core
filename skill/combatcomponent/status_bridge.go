package combatcomponent

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-skill/skill"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

// AttributeModifierBuffID is the reserved buff definition id carrying
// standalone AttributeModifierCommand grants. Modifier commands apply as
// independent instances under this id (each with its own duration, matching
// the MemoryHost's per-command modifier semantics), tagged
// "attribute_modifier" so dispels can address or avoid them.
const AttributeModifierBuffID combat.BuffID = 0xFFFFFFFF

// AttributeModifierTag classifies bridge-created modifier instances.
const AttributeModifierTag combat.Tag = "attribute_modifier"

// StatusBridge lands the skill status and attribute-modifier effect
// commands on combat containers: StatusCommand becomes a BuffContainer
// application driven by the status catalog entry, Remove/Dispel become
// container removals, and AttributeModifierCommand becomes an independent
// timed modifier grant. It follows the MemoryHost event vocabulary
// (status_applied, status_immune, status_removed, status_dispelled,
// attribute_modifier_applied).
//
// One deliberate semantic difference from the MemoryHost: mul_bp modifiers
// aggregate additively in basis-point deltas (combat.AttributeSet's rule,
// order-independent and exactly reversible for transaction rollback), while
// the MemoryHost chains them multiplicatively. Two +20% modifiers yield
// +40% here and +44% there; pick one host semantics per game and keep it.
type StatusBridge struct {
	Resolver Resolver
	Revision RevisionSource
	// Committer backs lower-isolation calls made outside a Nest handler.
	Committer nest.TransactionCommitter
	// Catalog supplies status policies (stacking, dispel category, immunity
	// tags, attribute modifiers, tenacity and duration caps).
	Catalog skill.GameplayCatalog
	// CurrentTick supplies the authoritative tick durations anchor to.
	CurrentTick func() skill.Tick
	// HasGameplayTag reports whether the target carries a gameplay tag —
	// immunity tags are world facts the component does not own. nil disables
	// immunity-tag checks.
	HasGameplayTag func(target skill.EntityID, tag skill.GameplayTagHandle) bool
}

// Apply handles a status-domain effect command. handled=false means the
// payload is not a status command and the caller must process it.
func (bridge *StatusBridge) Apply(command skill.EffectCommand) (skill.EffectResult, bool, error) {
	if nest.CurrentRollbackTx() == nil && bridge.handlesMutation(command) {
		value, err := nest.RunDetachedTransaction(context.Background(), bridge.Committer, "combat_status_apply", func() (any, error) {
			result, handled, applyErr := bridge.Apply(command)
			return hostApplyResult{result: result, handled: handled}, applyErr
		})
		if value == nil {
			return skill.EffectResult{}, true, err
		}
		result := value.(hostApplyResult)
		return result.result, result.handled, err
	}
	switch payload := command.Payload.(type) {
	case skill.StatusCommand:
		result, err := bridge.applyStatus(payload)
		return result, true, err
	case skill.RemoveStatusCommand:
		result, err := bridge.removeStatus(payload)
		return result, true, err
	case skill.DispelStatusCommand:
		result, err := bridge.dispelStatus(payload)
		return result, true, err
	case skill.AttributeModifierCommand:
		result, err := bridge.applyAttributeModifier(payload)
		return result, true, err
	case skill.ModifyStatusInstanceCommand:
		result, err := bridge.modifyStatusInstance(payload)
		return result, true, err
	}
	return skill.EffectResult{}, false, nil
}

func (*StatusBridge) handlesMutation(command skill.EffectCommand) bool {
	switch command.Payload.(type) {
	case skill.StatusCommand, skill.RemoveStatusCommand, skill.DispelStatusCommand, skill.AttributeModifierCommand, skill.ModifyStatusInstanceCommand:
		return true
	default:
		return false
	}
}

// modifyStatusInstance lands instance-handle operations (steal, transfer,
// stack and duration edits) on the buff container. Instance addressing
// contract: the opaque StatusInstanceRef id IS the combat.BuffInstanceID —
// hosts that surface buff instances through Select must hand out the
// container's instance ids as the opaque ids. Authorization and operation
// gating mirror the MemoryHost matrix (SourceOwnership, Dispellable,
// Copyable/Transferable/Stealable, DurationOperations, MaximumDurationTicks).
func (bridge *StatusBridge) modifyStatusInstance(command skill.ModifyStatusInstanceCommand) (skill.EffectResult, error) {
	component, ok := bridge.Resolver.CombatComponent(command.Status.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureReferenceExpired}}}, nil
	}
	id := combat.BuffInstanceID(command.Status.ID.OpaqueID())
	var instance combat.BuffInstance
	found := false
	for _, candidate := range component.ActiveBuffs() {
		if candidate.Instance == id {
			instance, found = candidate, true
			break
		}
	}
	if !found {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureReferenceExpired}}}, nil
	}
	policy, ok := bridge.statusPolicy(skill.StatusHandle(instance.Spec.ID))
	if !ok {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: buff %d has no status policy", instance.Spec.ID)
	}
	target := command.Status.Target
	authorized := policy.SourceOwnership == "owner" && command.Owner == target ||
		policy.SourceOwnership != "owner" && int64(command.Owner) == instance.Source
	if command.Operation == "remove" && command.Owner == target {
		authorized = true
	}
	if command.Operation == "transfer_to" && policy.Stealable {
		authorized = true
	}
	if !authorized {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailurePermissionDenied}}}, nil
	}
	now := int64(bridge.CurrentTick())
	clampDue := func(due int64) int64 {
		if policy.MaximumDurationTicks > 0 && due-now > int64(policy.MaximumDurationTicks) {
			return now + int64(policy.MaximumDurationTicks)
		}
		return due
	}
	durationAllowed := func(operation string) bool {
		if len(policy.DurationOperations) == 0 {
			return true
		}
		for _, allowed := range policy.DurationOperations {
			if allowed == operation {
				return true
			}
		}
		return false
	}
	beforeStacks, beforeDue := int(instance.Stacks), skill.Tick(instance.DueTick)
	remaining := instance.DueTick - now
	if remaining < 0 {
		remaining = 0
	}
	policyFailure := func() (skill.EffectResult, error) {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailurePolicyRejected}}}, nil
	}
	created := skill.StatusInstanceRef{}
	removed := false
	switch command.Operation {
	case "remove":
		if !policy.Dispellable {
			return policyFailure()
		}
		component.RemoveBuff(id)
		removed = true
	case "add_stacks":
		instance, removed = bridge.setStacks(component, id, instance.Stacks+command.Value)
	case "set_stacks":
		instance, removed = bridge.setStacks(component, id, command.Value)
	case "add_duration":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := remaining + command.Value
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(now+next))
	case "set_duration":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := command.Value
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(now+next))
	case "mul_duration_bp":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := combat.ScaleBasisPoints(remaining, command.Value)
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(now+next))
	case "refresh":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		total := instance.DueTick - instance.AppliedTick
		if total < 1 {
			total = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(now+total))
	case "copy_to", "transfer_to":
		allowed := command.Operation == "copy_to" && policy.Copyable || command.Operation == "transfer_to" && policy.Transferable
		destination, alive := bridge.Resolver.CombatComponent(command.Target)
		if command.Target == 0 || !alive || !destination.Combatant().Alive {
			return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
		}
		if !allowed || !validOwnershipPolicy(command.OwnershipPolicy) {
			return policyFailure()
		}
		if bridge.HasGameplayTag != nil {
			for _, immunity := range policy.ImmunityTags {
				if bridge.HasGameplayTag(command.Target, immunity) {
					return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Immune: true, PreviousStacks: beforeStacks, CurrentStacks: beforeStacks, DueTick: beforeDue, Status: command.Status}}}, nil
				}
			}
		}
		moved := instance
		moved.AppliedTick = now
		switch command.OwnershipPolicy {
		case "original_owner":
			moved.Source = int64(target)
		case "new_owner":
			moved.Source = int64(command.Target)
		case "new_source":
			moved.Source = int64(command.Owner)
		}
		createdID := destination.AdoptBuff(moved)
		created = skill.StatusInstanceRef{ID: skill.NewStatusInstanceID(uint64(createdID)), Target: command.Target}
		if command.Operation == "transfer_to" {
			component.RemoveBuff(id)
			removed = true
		}
	default:
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: unsupported status instance operation %q", command.Operation)
	}
	context := command.Event
	context.Owner, context.Target, context.Result = command.Owner, target, "status_instance_"+command.Operation
	receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_instance_" + command.Operation, Entity: target, Context: context}})
	receipt.Changed = true
	currentStacks := int(instance.Stacks)
	dueTick := skill.Tick(instance.DueTick)
	if removed {
		currentStacks = 0
	}
	removedStacks := beforeStacks - currentStacks
	if removedStacks < 0 {
		removedStacks = 0
	}
	return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Applied: true, Removed: removed, PreviousStacks: beforeStacks, CurrentStacks: currentStacks, RemovedStacks: removedStacks, DueTick: dueTick, PreviousDueTick: beforeDue, Status: command.Status, Created: created}}}, nil
}

func (bridge *StatusBridge) setStacks(component *CombatComponent, id combat.BuffInstanceID, stacks int64) (combat.BuffInstance, bool) {
	if stacks < 0 {
		stacks = 0
	}
	instance, _ := component.SetBuffStacks(id, stacks)
	return instance, stacks == 0
}

func validOwnershipPolicy(policy string) bool {
	return policy == "original_owner" || policy == "new_owner" || policy == "new_source"
}

func (bridge *StatusBridge) statusPolicy(handle skill.StatusHandle) (skill.StatusCatalogEntry, bool) {
	for _, entry := range bridge.Catalog.Statuses.Entries {
		if entry.Handle == handle {
			return entry, true
		}
	}
	return skill.StatusCatalogEntry{}, false
}

// buffSpec translates a status catalog entry plus one application command
// into the combat.BuffSpec the container applies.
func buffSpec(policy skill.StatusCatalogEntry, command skill.StatusCommand) combat.BuffSpec {
	spec := combat.BuffSpec{
		ID:               combat.BuffID(policy.Handle),
		MaxStacks:        int64(policy.MaxStacks),
		DurationTicks:    int64(command.DurationTicks),
		TenacityAffected: policy.TenacityPolicy == "scale_duration",
		MaxDurationTicks: int64(policy.MaximumDurationTicks),
	}
	if command.MaxStacks > 0 && (spec.MaxStacks <= 0 || int64(command.MaxStacks) < spec.MaxStacks) {
		spec.MaxStacks = int64(command.MaxStacks)
	}
	if policy.DispelCategory != "" {
		spec.Tags = append(spec.Tags, combat.Tag(policy.DispelCategory))
	}
	if policy.Category != "" && policy.Category != policy.DispelCategory {
		spec.Tags = append(spec.Tags, combat.Tag(policy.Category))
	}
	switch policy.RefreshPolicy {
	case "ignore":
		spec.StackPolicy = combat.BuffIgnore
	case "extend":
		spec.StackPolicy = combat.BuffExtend
	default: // "", "refresh", and "replace" (replace removes first, below)
		spec.StackPolicy = combat.BuffRefresh
	}
	for _, modifier := range policy.AttributeModifiers {
		translated := combat.Modifier{Attribute: combat.AttributeID(modifier.Attribute)}
		if modifier.Operation == "mul_bp" {
			// Catalog mul_bp values are absolute rates (12000 = ×1.2); the
			// AttributeSet aggregates additive deltas (+2000).
			translated.RateBP = modifier.Value - combat.BasisPointScale
		} else {
			translated.Flat = modifier.Value
		}
		spec.Modifiers = append(spec.Modifiers, translated)
	}
	return spec
}

func (bridge *StatusBridge) buffStacks(component *CombatComponent, id combat.BuffID) int {
	stacks := 0
	for _, instance := range component.ActiveBuffs() {
		if instance.Spec.ID == id {
			stacks += int(instance.Stacks)
		}
	}
	return stacks
}

func (bridge *StatusBridge) applyStatus(command skill.StatusCommand) (skill.EffectResult, error) {
	component, ok := bridge.Resolver.CombatComponent(command.Target)
	if !ok || !component.Combatant().Alive {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	policy, ok := bridge.statusPolicy(command.Status)
	if !ok {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: unknown status handle %d", command.Status)
	}
	if command.DurationTicks <= 0 {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: status duration must be positive")
	}
	context := command.Event
	context.Owner, context.Target, context.EffectIndex = command.SourceOwner, command.Target, command.Meta.EffectIndex
	previousStacks := bridge.buffStacks(component, combat.BuffID(policy.Handle))
	if bridge.HasGameplayTag != nil {
		for _, immunity := range policy.ImmunityTags {
			if bridge.HasGameplayTag(command.Target, immunity) {
				context.Result = "immune"
				receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_immune", Entity: command.Target, Context: context}})
				receipt.Changed = true
				return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Immune: true, PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
			}
		}
	}
	spec := buffSpec(policy, command)
	if policy.RefreshPolicy == "replace" {
		for _, instance := range component.ActiveBuffs() {
			if instance.Spec.ID == spec.ID {
				component.RemoveBuff(instance.Instance)
			}
		}
	}
	tick := int64(bridge.CurrentTick())
	stacks := command.Stacks
	if stacks <= 0 {
		stacks = 1
	}
	dueTick := int64(0)
	blocked := false
	for applied := 0; applied < stacks; applied++ {
		_, outcome := component.ApplyBuff(spec, tick, int64(command.SourceOwner))
		if outcome == combat.BuffBlockedImmune {
			blocked = true
			break
		}
	}
	if blocked && previousStacks == bridge.buffStacks(component, spec.ID) {
		context.Result = "immune"
		receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_immune", Entity: command.Target, Context: context}})
		receipt.Changed = true
		return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Immune: true, PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
	}
	for _, instance := range component.ActiveBuffs() {
		if instance.Spec.ID == spec.ID && instance.DueTick > dueTick {
			dueTick = instance.DueTick
		}
	}
	context.Result = "status_applied"
	receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_applied", Entity: command.Target, Context: context}})
	receipt.Changed = true
	currentStacks := bridge.buffStacks(component, spec.ID)
	return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Applied: true, PreviousStacks: previousStacks, CurrentStacks: currentStacks, DueTick: skill.Tick(dueTick)}}}, nil
}

func (bridge *StatusBridge) removeStatus(command skill.RemoveStatusCommand) (skill.EffectResult, error) {
	component, ok := bridge.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	id := combat.BuffID(command.Status)
	previousStacks := bridge.buffStacks(component, id)
	removed := 0
	for _, instance := range component.ActiveBuffs() {
		if instance.Spec.ID != id {
			continue
		}
		if command.SourceOwner != 0 && instance.Source != int64(command.SourceOwner) {
			continue
		}
		if _, ok := component.RemoveBuff(instance.Instance); ok {
			removed++
		}
	}
	if removed == 0 {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
	}
	context := command.Event
	context.Owner, context.Target, context.Result = command.SourceOwner, command.Target, "status_removed"
	receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_removed", Entity: command.Target, Context: context}})
	receipt.Changed = true
	currentStacks := bridge.buffStacks(component, id)
	return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Removed: true, PreviousStacks: previousStacks, CurrentStacks: currentStacks, RemovedStacks: previousStacks - currentStacks}}}, nil
}

func (bridge *StatusBridge) dispelStatus(command skill.DispelStatusCommand) (skill.EffectResult, error) {
	component, ok := bridge.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	tag := combat.Tag(command.Category)
	previousStacks := 0
	for _, instance := range component.ActiveBuffs() {
		if containsBuffTag(instance.Spec.Tags, tag) {
			previousStacks += int(instance.Stacks)
		}
	}
	removed := component.DispelBuffs(tag, command.Count)
	if len(removed) == 0 {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{PreviousStacks: previousStacks, CurrentStacks: previousStacks}}}, nil
	}
	removedStacks := 0
	for _, instance := range removed {
		removedStacks += int(instance.Stacks)
	}
	context := command.Event
	context.Target, context.Result = command.Target, "status_dispelled"
	receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "status_dispelled", Entity: command.Target, Context: context}})
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Payload: skill.StatusEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.StatusResult{Removed: true, PreviousStacks: previousStacks, CurrentStacks: previousStacks - removedStacks, RemovedStacks: removedStacks}}}, nil
}

func (bridge *StatusBridge) applyAttributeModifier(command skill.AttributeModifierCommand) (skill.EffectResult, error) {
	component, ok := bridge.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: bridge.Revision.CurrentRevision()}, Payload: skill.AttributeModifierEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	if command.Operation != "add" && command.Operation != "mul_bp" {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: unsupported modifier operation %q", command.Operation)
	}
	if command.DurationTicks <= 0 {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: modifier duration must be positive")
	}
	modifier := combat.Modifier{Attribute: combat.AttributeID(command.Attribute)}
	if command.Operation == "mul_bp" {
		modifier.RateBP = command.Value - combat.BasisPointScale
	} else {
		modifier.Flat = command.Value
	}
	spec := combat.BuffSpec{
		ID: AttributeModifierBuffID, Tags: []combat.Tag{AttributeModifierTag},
		StackPolicy: combat.BuffIndependent, DurationTicks: int64(command.DurationTicks),
		Modifiers: []combat.Modifier{modifier},
	}
	tick := int64(bridge.CurrentTick())
	component.ApplyBuff(spec, tick, int64(command.SourceOwner))
	context := command.Event
	context.Owner, context.Target, context.Result = command.SourceOwner, command.Target, "attribute_modifier_applied"
	receipt := bridge.Revision.CommitEffect([]EffectEvent{{Kind: "attribute_modifier_applied", Entity: command.Target, Context: context}})
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Payload: skill.AttributeModifierEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.AttributeModifierResult{Applied: true, DueTick: skill.Tick(tick) + command.DurationTicks}}}, nil
}

func containsBuffTag(tags []combat.Tag, want combat.Tag) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
