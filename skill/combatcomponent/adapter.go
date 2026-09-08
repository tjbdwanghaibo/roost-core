package combatcomponent

import (
	"context"
	"fmt"
	"math"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-skill/skill"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

// Resolver locates the combat component for a skill entity id. The entity
// must already be locked by the caller (skill.Host methods run under the
// Runtime lock inside a nest handler, which holds the entity locks).
type Resolver interface {
	CombatComponent(id skill.EntityID) (*CombatComponent, bool)
}

// RevisionSource owns the world revision and the host event stream.
type RevisionSource interface {
	CurrentRevision() skill.WorldRevision
	// CommitEffect advances the revision once and appends the events in
	// order, returning the receipt for the mutation.
	CommitEffect(events []EffectEvent) skill.CommitReceipt
}

// EffectEvent is one host event an effect produced.
type EffectEvent struct {
	Kind    string
	Entity  skill.EntityID
	Context skill.EventContext
}

// HostAdapter implements the combat effect surface of skill.Host over
// combat components: damage, heal, and shield commands, attribute and
// resource reads, and cost payment. Embed it in a business Host and delegate
// the commands it recognizes; world queries (Select), motion, processes, and
// spawning stay with the business host. It follows the MemoryHost event
// vocabulary (damage_resolved, combat_hook_*, shield_absorbed, ...) so proc
// filters behave identically on both hosts.
type HostAdapter struct {
	Resolver Resolver
	Revision RevisionSource
	// Committer is required only when the adapter is invoked outside an
	// existing Nest transaction; that path uses RunDetachedTransaction.
	Committer nest.TransactionCommitter
	// Catalog supplies attribute quantities for reads and the critical tag.
	Catalog skill.GameplayCatalog
	// SpellTag marks damage commands carrying it as spell damage; zero
	// disables spell-shield interception.
	SpellTag skill.GameplayTagHandle
	// CriticalTag is appended to damage event tags on critical hits.
	CriticalTag skill.GameplayTagHandle
	// Hooks supplies combat-hook interceptors for one damage instance; nil
	// disables status-driven hooks.
	Hooks func(source, target skill.EntityID) combat.Hooks
	// ResourceAttribute maps a resource to the attribute channel that backs
	// it; required for PayCosts, resource reads, and resource commands.
	ResourceAttribute func(resource string, handle skill.ResourceHandle) (combat.AttributeID, bool)
	// Status, when set, extends Apply to the status-domain commands
	// (StatusCommand, RemoveStatusCommand, DispelStatusCommand,
	// AttributeModifierCommand) through a StatusBridge.
	Status *StatusBridge
}

// Apply handles a combat effect command. handled=false means the payload is
// not a combat command and the business host must process it.
func (adapter *HostAdapter) Apply(command skill.EffectCommand) (skill.EffectResult, bool, error) {
	if nest.CurrentRollbackTx() == nil && adapter.handlesMutation(command) {
		value, err := nest.RunDetachedTransaction(context.Background(), adapter.Committer, "combat_host_apply", func() (any, error) {
			result, handled, applyErr := adapter.Apply(command)
			return hostApplyResult{result: result, handled: handled}, applyErr
		})
		if value == nil {
			return skill.EffectResult{}, true, err
		}
		result := value.(hostApplyResult)
		return result.result, result.handled, err
	}
	switch payload := command.Payload.(type) {
	case skill.DamageCommand:
		result, err := adapter.applyDamage(payload)
		return result, true, err
	case skill.HealCommand:
		result, err := adapter.applyHeal(payload)
		return result, true, err
	case skill.ShieldCommand:
		result, err := adapter.applyShield(payload)
		return result, true, err
	case skill.ResourceCommand:
		result, err := adapter.applyResource(payload)
		return result, true, err
	}
	if adapter.Status != nil {
		return adapter.Status.Apply(command)
	}
	return skill.EffectResult{}, false, nil
}

type hostApplyResult struct {
	result  skill.EffectResult
	handled bool
}

func (adapter *HostAdapter) handlesMutation(command skill.EffectCommand) bool {
	switch command.Payload.(type) {
	case skill.DamageCommand, skill.HealCommand, skill.ShieldCommand, skill.ResourceCommand:
		return true
	case skill.StatusCommand, skill.RemoveStatusCommand, skill.DispelStatusCommand, skill.AttributeModifierCommand, skill.ModifyStatusInstanceCommand:
		return adapter.Status != nil
	default:
		return false
	}
}

// applyResource lands a resource gain/spend/set on the mapped attribute
// base, with the MemoryHost's semantics: spend/sub fails atomically on
// insufficient funds, negative results are rejected, and a no-op change
// commits nothing.
func (adapter *HostAdapter) applyResource(command skill.ResourceCommand) (skill.EffectResult, error) {
	component, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: entity %d has no combat component", command.Target)
	}
	attribute, mapped := adapter.ResourceAttribute("", command.Resource)
	if !mapped {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: resource handle %d has no attribute mapping", command.Resource)
	}
	before := component.AttributeBase(attribute)
	after := before
	switch command.Operation {
	case "set":
		after = command.Amount
	case "add":
		after = saturatingAttributeAdd(before, command.Amount)
	case "spend", "sub":
		if command.Amount < 0 || before < command.Amount {
			return skill.EffectResult{}, skill.ErrInsufficientResource
		}
		after = before - command.Amount
	default:
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: unsupported resource operation %q", command.Operation)
	}
	if after < 0 {
		return skill.EffectResult{}, skill.ErrInsufficientResource
	}
	if after == before {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Value: skill.ResourceRuntimeValue(after)}, nil
	}
	component.SetAttributeBase(attribute, after)
	context := skill.EventContext{Target: command.Target, Result: "resource_changed"}
	receipt := adapter.Revision.CommitEffect([]EffectEvent{{Kind: "resource_changed", Entity: command.Target, Context: context}})
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Value: skill.ResourceRuntimeValue(after)}, nil
}

func saturatingAttributeAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func (adapter *HostAdapter) applyDamage(command skill.DamageCommand) (skill.EffectResult, error) {
	target, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok || !target.Combatant().Alive {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.DamageEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	var source *CombatComponent
	if command.Source != 0 {
		source, ok = adapter.Resolver.CombatComponent(command.Source)
		if !ok {
			return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.DamageEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
		}
	}
	var hooks combat.Hooks
	if adapter.Hooks != nil {
		hooks = adapter.Hooks(command.Source, command.Target)
	}
	input := combat.DamageInput{
		Amount: command.Amount, Type: combat.DamageType(command.DamageType), Element: combat.Element(command.Element),
		CanCritical: command.CanCritical,
		SpellTagged: adapter.SpellTag != 0 && containsTagHandle(command.Tags, adapter.SpellTag),
	}
	outcome, _ := target.ApplyDamage(source, input, hooks)
	result := skill.DamageResult{
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
	tags := append([]skill.GameplayTagHandle(nil), command.Tags...)
	if result.Critical && adapter.CriticalTag != 0 {
		tags = append(tags, adapter.CriticalTag)
	}
	context = context.WithGameplayTags(tags)
	events := []EffectEvent{{Kind: "damage_resolved", Entity: command.Target, Context: context}}
	for _, hook := range result.CombatHooks {
		hookContext := context
		hookContext.Result = hook
		events = append(events, EffectEvent{Kind: "combat_hook_" + hook, Entity: command.Target, Context: hookContext})
	}
	if result.Absorbed > 0 {
		events = append(events, EffectEvent{Kind: "shield_absorbed", Entity: command.Target, Context: context})
		if target.Combatant().Shield == 0 {
			events = append(events, EffectEvent{Kind: "shield_broken", Entity: command.Target, Context: context})
		}
	}
	receipt := adapter.Revision.CommitEffect(events)
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Payload: skill.DamageEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: result}}, nil
}

func (adapter *HostAdapter) applyHeal(command skill.HealCommand) (skill.EffectResult, error) {
	target, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.HealEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	outcome, alive := target.Heal(command.Amount)
	if !alive {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.HealEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	if outcome.Effective == 0 {
		return skill.EffectResult{
			Commit:  skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()},
			Payload: skill.HealEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.HealResult{Attempted: outcome.Attempted}},
		}, nil
	}
	context := command.Event
	context.Source, context.Target, context.Result = command.Source, command.Target, "healed"
	receipt := adapter.Revision.CommitEffect([]EffectEvent{{Kind: "heal_resolved", Entity: command.Target, Context: context}})
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Payload: skill.HealEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.HealResult{Attempted: outcome.Attempted, Effective: outcome.Effective}}}, nil
}

func (adapter *HostAdapter) applyShield(command skill.ShieldCommand) (skill.EffectResult, error) {
	target, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.ShieldEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	added, alive := target.AddShield(command.Amount)
	if !alive {
		return skill.EffectResult{Commit: skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, Payload: skill.ShieldEffectResult{ResultOutcome: skill.ResultOutcome{FailureReason: skill.ExpectedFailureInvalidTarget}}}, nil
	}
	if added == 0 {
		return skill.EffectResult{
			Commit:  skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()},
			Payload: skill.ShieldEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.ShieldResult{}},
		}, nil
	}
	context := command.Event
	context.Source, context.Target, context.Result = command.Source, command.Target, "shielded"
	receipt := adapter.Revision.CommitEffect([]EffectEvent{{Kind: "shield_resolved", Entity: command.Target, Context: context}})
	receipt.Changed = true
	return skill.EffectResult{Commit: receipt, Payload: skill.ShieldEffectResult{ResultOutcome: skill.ResultOutcome{Succeeded: true}, Result: skill.ShieldResult{Added: added}}}, nil
}

// Read answers attribute and resource reads from component state.
// handled=false means the payload is not a combat read.
func (adapter *HostAdapter) Read(request skill.ReadRequest) (skill.ReadResult, bool, error) {
	switch payload := request.Payload.(type) {
	case skill.AttributeRead:
		component, ok := adapter.Resolver.CombatComponent(payload.Entity)
		if !ok {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: entity %d has no combat component", payload.Entity)
		}
		value := component.AttributeCurrent(combat.AttributeID(payload.Attribute))
		return skill.ReadResult{Meta: skill.QueryResultMeta{Revision: adapter.Revision.CurrentRevision()}, Value: skill.AttributeRuntimeValue(adapter.Catalog, payload.Attribute, value)}, true, nil
	case skill.ResourceRead:
		component, ok := adapter.Resolver.CombatComponent(payload.Entity)
		if !ok {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: entity %d has no combat component", payload.Entity)
		}
		attribute, mapped := adapter.ResourceAttribute(payload.Resource, 0)
		if !mapped {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: resource %q has no attribute mapping", payload.Resource)
		}
		return skill.ReadResult{Meta: skill.QueryResultMeta{Revision: adapter.Revision.CurrentRevision()}, Value: skill.ResourceRuntimeValue(component.AttributeCurrent(attribute))}, true, nil
	}
	return skill.ReadResult{}, false, nil
}

// PayCosts atomically validates and deducts every entry against the mapped
// attribute bases: either every entry is paid or nothing changes.
func (adapter *HostAdapter) PayCosts(payment skill.CostPayment) (skill.CommitReceipt, error) {
	if nest.CurrentRollbackTx() == nil {
		value, err := nest.RunDetachedTransaction(context.Background(), adapter.Committer, "combat_host_pay_costs", func() (any, error) {
			return adapter.PayCosts(payment)
		})
		if value == nil {
			return skill.CommitReceipt{}, err
		}
		return value.(skill.CommitReceipt), err
	}
	component, ok := adapter.Resolver.CombatComponent(payment.Entity)
	if !ok {
		return skill.CommitReceipt{}, fmt.Errorf("combatcomponent: entity %d has no combat component", payment.Entity)
	}
	totals := make(map[combat.AttributeID]int64, len(payment.Entries))
	order := make([]combat.AttributeID, 0, len(payment.Entries))
	for _, entry := range payment.Entries {
		if entry.Amount < 0 {
			return skill.CommitReceipt{}, fmt.Errorf("combatcomponent: negative cost")
		}
		attribute, mapped := adapter.ResourceAttribute(entry.Resource, entry.Handle)
		if !mapped {
			return skill.CommitReceipt{}, fmt.Errorf("combatcomponent: resource %q has no attribute mapping", entry.Resource)
		}
		if _, seen := totals[attribute]; !seen {
			order = append(order, attribute)
		}
		if totals[attribute] > math.MaxInt64-entry.Amount {
			return skill.CommitReceipt{}, fmt.Errorf("combatcomponent: cost total overflows")
		}
		totals[attribute] += entry.Amount
	}
	for _, attribute := range order {
		if component.AttributeBase(attribute) < totals[attribute] {
			return skill.CommitReceipt{}, skill.ErrInsufficientResource
		}
	}
	changed := false
	for _, attribute := range order {
		if amount := totals[attribute]; amount > 0 {
			component.SetAttributeBase(attribute, component.AttributeBase(attribute)-amount)
			changed = true
		}
	}
	if !changed {
		return skill.CommitReceipt{Revision: adapter.Revision.CurrentRevision()}, nil
	}
	receipt := adapter.Revision.CommitEffect([]EffectEvent{{Kind: "costs_paid", Entity: payment.Entity}})
	receipt.Changed = true
	return receipt, nil
}

func containsTagHandle(tags []skill.GameplayTagHandle, want skill.GameplayTagHandle) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
