package combatcomponent

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

// AttributeModifierBuffID 是承载独立 AttributeModifierCommand 授予的保留 buff 定义 ID。修正命令以独立实例施加在
// 这个 ID 下（各自有持续时间，与 MemoryHost 每条命令一个修正的语义一致），并带 "attribute_modifier" 标签，驱散时
// 可以针对或避开它们。
const AttributeModifierBuffID combat.BuffID = 0xFFFFFFFF

// AttributeModifierTag 给桥接创建的修正实例分类。
const AttributeModifierTag combat.Tag = "attribute_modifier"

// StatusBridge 把 skill 的状态与属性修正效果命令落到战斗容器上：StatusCommand 变成按状态 catalog 条目驱动的
// BuffContainer 施加，Remove / Dispel 变成容器移除，AttributeModifierCommand 变成独立的限时修正授予。它沿用
// MemoryHost 的事件词表（status_applied、status_immune、status_removed、status_dispelled、
// attribute_modifier_applied）。
//
// 与 MemoryHost 有一处刻意的语义差异：mul_bp 修正在这里按基点增量相加（combat.AttributeSet 的规则，与顺序无关，
// 事务回滚时精确可逆），MemoryHost 则相乘。两个 +20% 修正在这里得 +40%，在 MemoryHost 得 +44%；每个游戏选定一种
// Host 语义并保持一致。
type StatusBridge struct {
	Resolver Resolver
	Revision RevisionSource
	// Committer 承接在 Nest handler 之外发起的低隔离调用。
	Committer nest.TransactionCommitter
	// Catalog 提供状态策略（叠层、驱散类别、免疫标签、属性修正、韧性与时长上限）。
	Catalog skill.GameplayCatalog
	// CurrentTick 提供权威 tick，时长以它为起点。
	CurrentTick func() skill.Tick
	// HasGameplayTag 报告目标是否带某个玩法标签——免疫标签是组件不拥有的世界事实。nil 关闭免疫标签检查。
	HasGameplayTag func(target skill.EntityID, tag skill.GameplayTagHandle) bool
}

// Apply 处理一条状态域效果命令。handled=false 表示载荷不是状态命令，调用方必须自己处理。
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

// modifyStatusInstance 把实例句柄操作（偷取、转移、层数与时长修改）落到 buff 容器上。实例寻址契约：不透明的
// StatusInstanceRef ID 就是 combat.BuffInstanceID——经 Select 暴露 buff 实例的 Host 必须把容器的实例 ID 作为
// 不透明 ID 交出去。授权与操作门槛与 MemoryHost 的矩阵一致（SourceOwnership、Dispellable、
// Copyable / Transferable / Stealable、DurationOperations、MaximumDurationTicks）。
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
		instance, removed = bridge.setStacks(component, id, saturatingAttributeAdd(instance.Stacks, command.Value))
	case "set_stacks":
		instance, removed = bridge.setStacks(component, id, command.Value)
	case "add_duration":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := saturatingAttributeAdd(remaining, command.Value)
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(saturatingAttributeAdd(now, next)))
	case "set_duration":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := command.Value
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(saturatingAttributeAdd(now, next)))
	case "mul_duration_bp":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		next := combat.ScaleBasisPoints(remaining, command.Value)
		if next < 1 {
			next = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(saturatingAttributeAdd(now, next)))
	case "refresh":
		if !durationAllowed(command.Operation) {
			return policyFailure()
		}
		if instance.DueTick == 0 {
			break
		} // 永久实例刷新仍然永久。
		total := instance.DueTick - instance.AppliedTick
		if total < 1 {
			total = 1
		}
		instance, _ = component.SetBuffDueTick(id, clampDue(saturatingAttributeAdd(now, total)))
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

// buffSpec 把一条状态 catalog 条目加一次施加命令翻译成容器施加用的 combat.BuffSpec。
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
	default: // ""、"refresh" 与 "replace"（replace 先移除，见下）
		spec.StackPolicy = combat.BuffRefresh
	}
	for _, modifier := range policy.AttributeModifiers {
		translated := combat.Modifier{Attribute: combat.AttributeID(modifier.Attribute)}
		if modifier.Operation == "mul_bp" {
			// catalog 里的 mul_bp 是绝对比率（12000 = ×1.2）；AttributeSet 聚合的是加法增量（+2000）。
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
