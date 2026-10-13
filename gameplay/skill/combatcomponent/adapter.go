package combatcomponent

import (
	"context"
	"fmt"
	"math"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

// Resolver 按 skill 实体 ID 找到战斗组件。实体必须已由调用方加锁（skill.Host 的方法在 nest handler 内、Runtime
// 的锁下运行，handler 持有实体锁）。
// 来源（含吸血）、目标和 buff 迁移目的实体都须在 Nest 声明目标或 Cast 动态纳入的范围内；
// Resolver 只查已持锁对象，不自动上锁，也不替未 CaptureEntities 的实体建立 state 回滚快照。
type Resolver interface {
	CombatComponent(id skill.EntityID) (*CombatComponent, bool)
}

// RevisionSource 持有世界 revision 与 Host 事件流。
type RevisionSource interface {
	CurrentRevision() skill.WorldRevision
	// CommitEffect 把 revision 推进一次并按顺序追加事件，返回这次修改的回执。
	CommitEffect(events []EffectEvent) skill.CommitReceipt
}

// EffectEvent 是一次效果产生的一条 Host 事件。
type EffectEvent struct {
	Kind    string
	Entity  skill.EntityID
	Context skill.EventContext
}

// HostAdapter 在战斗组件之上实现 skill.Host 的战斗效果部分：伤害、治疗与护盾命令，属性与资源读取，以及费用支付。
// 把它嵌进业务 Host，由业务 Host 把它认得的命令委托过来；世界查询（Select）、运动、衍生物与召唤留在业务 Host。
// 它沿用 MemoryHost 的事件词表（damage_resolved、combat_hook_*、shield_absorbed 等），proc 过滤在两种 Host 上
// 行为一致。
type HostAdapter struct {
	Resolver Resolver
	Revision RevisionSource
	// Committer 只在不处于已有 Nest 事务里调用适配器时才需要；那条路径用 RunDetachedTransaction。
	Committer nest.TransactionCommitter
	// Catalog 提供读取用的属性量纲与暴击标签。
	Catalog skill.GameplayCatalog
	// SpellTag：带这个标签的伤害命令算作法术伤害；零值关闭法术护盾拦截。
	SpellTag skill.GameplayTagHandle
	// CriticalTag 在暴击时追加到伤害事件的标签里。
	CriticalTag skill.GameplayTagHandle
	// Hooks 为一次伤害实例提供战斗钩子拦截器；nil 关闭由状态驱动的钩子。
	Hooks func(source, target skill.EntityID) combat.Hooks
	// ResourceAttribute 把资源映射到承载它的属性通道；PayCosts、资源读取与资源命令都需要它。
	ResourceAttribute func(resource string, handle skill.ResourceHandle) (combat.AttributeID, bool)
	// Status 设置时，Apply 经 StatusBridge 额外处理状态域命令（StatusCommand、RemoveStatusCommand、
	// DispelStatusCommand、AttributeModifierCommand）。
	Status *StatusBridge
}

// Apply 处理一条战斗效果命令。handled=false 表示载荷不是战斗命令，业务 Host 必须自己处理。
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

// applyResource 把资源的增加 / 花费 / 设置落到映射属性的 base 上，语义与 MemoryHost 相同：余额不足时 spend / sub
// 原子失败，结果为负时拒绝，没有变化时什么也不提交。
func (adapter *HostAdapter) applyResource(command skill.ResourceCommand) (skill.EffectResult, error) {
	component, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok {
		return skill.EffectResult{}, fmt.Errorf("combatcomponent: entity %d has no combat component", command.Target)
	}
	attribute, mapped := adapter.resourceAttribute("", command.Resource)
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
	if policy := adapter.Catalog.Combat.FormulaPolicy; policy != "" && policy != "twelve_stage_v1" {
		return skill.EffectResult{}, skill.ErrCombatPolicyUnsupported
	}
	if types := adapter.Catalog.Combat.DamageTypes; len(types) > 0 {
		valid := false
		for _, handle := range types {
			valid = valid || handle == command.DamageType
		}
		if !valid {
			return skill.EffectResult{}, skill.ErrCombatHandleInvalid
		}
	}
	if entries := adapter.Catalog.Elements.Entries; len(entries) > 0 {
		valid := false
		for _, entry := range entries {
			valid = valid || entry.Handle == command.Element
		}
		if !valid {
			return skill.EffectResult{}, skill.ErrCombatHandleInvalid
		}
	}

	target, ok := adapter.Resolver.CombatComponent(command.Target)
	if !ok || !target.Combatant().Alive || target.Combatant().Health < 0 {
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

// HostCapabilities 声明这个适配器实现的那部分 skill Host 能力表：Catalog 里可读的属性、ResourceAttribute 映射到的
// Catalog 资源、四种资源操作，以及（有 StatusBridge 时）add / mul_bp 属性修正。衍生物、运动与召唤属于业务 Host，
// 由业务 Host 声明自己的部分，再用 skill.MergeHostCapabilities 合并两者。
func (adapter *HostAdapter) HostCapabilities() skill.HostCapabilityTable {
	table := skill.CatalogHostCapabilities(adapter.Catalog)
	mapped := make([]string, 0, len(table.Resources))
	for _, entry := range adapter.Catalog.Resources.Entries {
		if adapter.ResourceAttribute == nil {
			break
		}
		if _, ok := adapter.ResourceAttribute(entry.Key, entry.Handle); ok {
			mapped = append(mapped, entry.Key)
		}
	}
	table.Resources = mapped
	if adapter.ResourceAttribute != nil {
		table.ResourceOperations = []string{"set", "add", "spend", "sub"}
	}
	if adapter.Status != nil {
		table.ModifierOperations = []string{"add", "mul_bp"}
	}
	return table
}

// readableAttribute 报告 Catalog 是否声明该句柄可读。能力表之外的读取直接失败，而不是返回未知战斗属性通道里的零值。
func (adapter *HostAdapter) readableAttribute(handle skill.AttributeHandle) bool {
	for _, entry := range adapter.Catalog.Attributes.Entries {
		if entry.Handle == handle {
			return entry.Readable
		}
	}
	return false
}

// Read 从组件状态回答属性与资源读取。
// 资源读返回可支付的 base 池；属性读返回包含修饰的 Current，两者不能混用。
// handled=false 表示载荷不是战斗读取。
func (adapter *HostAdapter) Read(request skill.ReadRequest) (skill.ReadResult, bool, error) {
	switch payload := request.Payload.(type) {
	case skill.AttributeRead:
		component, ok := adapter.Resolver.CombatComponent(payload.Entity)
		if !ok {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: entity %d has no combat component", payload.Entity)
		}
		if !adapter.readableAttribute(payload.Attribute) {
			return skill.ReadResult{}, true, fmt.Errorf("%w: combatcomponent: attribute handle %d is not readable in the catalog", skill.ErrHostCapabilityMissing, payload.Attribute)
		}
		value := component.AttributeCurrent(combat.AttributeID(payload.Attribute))
		return skill.ReadResult{Meta: skill.QueryResultMeta{Revision: adapter.Revision.CurrentRevision()}, Value: skill.AttributeRuntimeValue(adapter.Catalog, payload.Attribute, value)}, true, nil
	case skill.ResourceRead:
		component, ok := adapter.Resolver.CombatComponent(payload.Entity)
		if !ok {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: entity %d has no combat component", payload.Entity)
		}
		attribute, mapped := adapter.resourceAttribute(payload.Resource, 0)
		if !mapped {
			return skill.ReadResult{}, true, fmt.Errorf("combatcomponent: resource %q has no attribute mapping", payload.Resource)
		}
		return skill.ReadResult{Meta: skill.QueryResultMeta{Revision: adapter.Revision.CurrentRevision()}, Value: skill.ResourceRuntimeValue(component.AttributeBase(attribute))}, true, nil
	}
	return skill.ReadResult{}, false, nil
}

// PayCosts 原子地按映射属性的 base 校验并扣除每一项：要么全部支付，要么什么都不变。
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
		attribute, mapped := adapter.resourceAttribute(entry.Resource, entry.Handle)
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

// 未提供资源映射就不支持资源操作，声明与执行使用同一个边界。
func (adapter *HostAdapter) resourceAttribute(name string, handle skill.ResourceHandle) (combat.AttributeID, bool) {
	if adapter.ResourceAttribute == nil {
		return 0, false
	}
	return adapter.ResourceAttribute(name, handle)
}
