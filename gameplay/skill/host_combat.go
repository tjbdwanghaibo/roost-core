package skill

// DamageCommand 是一次伤害实例。
type DamageCommand struct {
	Source      EntityID
	Owner       EntityID
	Target      EntityID
	Amount      int64
	DamageType  DamageTypeHandle
	Element     ElementHandle
	Tags        []GameplayTagHandle
	CanCritical bool
	Meta        CommandMeta
	Event       EventContext
}

func (DamageCommand) isEffectCommandPayload() {}

// DamageResult 是伤害管线的结算结果。
type DamageResult struct {
	Attempted, Mitigated, Absorbed, HealthDamage int64
	Critical, Blocked, Dodged, Immune, Killed    bool
	Parried                                      bool
	CombatHooks                                  []string
}

// DamageEffectResult 是伤害效果的结果载荷。
type DamageEffectResult struct {
	ResultOutcome
	Result DamageResult
}

func (DamageEffectResult) isEffectResultPayload() {}

// HealCommand 是一次治疗。
type HealCommand struct {
	Source, Target EntityID
	Amount         int64
	Meta           CommandMeta
	Event          EventContext
}

func (HealCommand) isEffectCommandPayload() {}

// HealResult 是治疗的尝试量与实际恢复量；HealEffectResult 是它的结果载荷。
type HealResult struct{ Attempted, Effective int64 }
type HealEffectResult struct {
	ResultOutcome
	Result HealResult
}

func (HealEffectResult) isEffectResultPayload() {}

// ShieldCommand 给目标加护盾。
type ShieldCommand struct {
	Source, Target EntityID
	Amount         int64
	SourceSkill    string
	SourceCast     CastID
	DurationTicks  Tick
	Meta           CommandMeta
	Event          EventContext
}

func (ShieldCommand) isEffectCommandPayload() {}

// ShieldResult 是实际增加的护盾值；ShieldEffectResult 是它的结果载荷。
type ShieldResult struct{ Added int64 }
type ShieldEffectResult struct {
	ResultOutcome
	Result ShieldResult
}

func (ShieldEffectResult) isEffectResultPayload() {}

// StatusCommand 给目标施加状态（buff）。
type StatusCommand struct {
	SourceOwner   EntityID
	SourceSkill   string
	SourceCast    CastID
	Target        EntityID
	Status        StatusHandle
	DurationTicks Tick
	Stacks        int
	MaxStacks     int
	Meta          CommandMeta
	Event         EventContext
}

func (StatusCommand) isEffectCommandPayload() {}

// RemoveStatusCommand 移除目标的某个状态。
type RemoveStatusCommand struct {
	SourceOwner EntityID
	Target      EntityID
	Status      StatusHandle
	Meta        CommandMeta
	Event       EventContext
}

func (RemoveStatusCommand) isEffectCommandPayload() {}

// DispelStatusCommand 驱散目标身上某个类别的至多 Count 个状态。
type DispelStatusCommand struct {
	Target   EntityID
	Category string
	Count    int
	Meta     CommandMeta
	Event    EventContext
}

func (DispelStatusCommand) isEffectCommandPayload() {}

// StatusResult 是状态操作的结果：是否施加 / 移除 / 免疫、层数与到期变化。
type StatusResult struct {
	Applied, Removed, Immune bool
	PreviousStacks           int
	CurrentStacks            int
	RemovedStacks            int
	DueTick                  Tick
	PreviousDueTick          Tick
	Status, Created          StatusInstanceRef
	CombatHooks              []string
}

// StatusEffectResult 是状态效果的结果载荷。
type StatusEffectResult struct {
	ResultOutcome
	Result StatusResult
}

func (StatusEffectResult) isEffectResultPayload() {}

// AttributeModifierCommand 给目标施加一个限时属性修正（add / mul_bp）。
type AttributeModifierCommand struct {
	SourceOwner   EntityID
	SourceSkill   string
	SourceCast    CastID
	Target        EntityID
	Attribute     AttributeHandle
	Operation     string
	Value         int64
	DurationTicks Tick
	Meta          CommandMeta
	Event         EventContext
}

func (AttributeModifierCommand) isEffectCommandPayload() {}

// AttributeModifierResult 是属性修正的结果；AttributeModifierEffectResult 是它的结果载荷。
type AttributeModifierResult struct {
	Applied bool
	DueTick Tick
}

type AttributeModifierEffectResult struct {
	ResultOutcome
	Result AttributeModifierResult
}

func (AttributeModifierEffectResult) isEffectResultPayload() {}
