package skill

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

type DamageResult struct {
	Attempted, Mitigated, Absorbed, HealthDamage int64
	Critical, Blocked, Dodged, Immune, Killed    bool
	Parried                                      bool
	CombatHooks                                  []string
}

type DamageEffectResult struct {
	ResultOutcome
	Result DamageResult
}

func (DamageEffectResult) isEffectResultPayload() {}

type HealCommand struct {
	Source, Target EntityID
	Amount         int64
	Meta           CommandMeta
	Event          EventContext
}

func (HealCommand) isEffectCommandPayload() {}

type HealResult struct{ Attempted, Effective int64 }
type HealEffectResult struct {
	ResultOutcome
	Result HealResult
}

func (HealEffectResult) isEffectResultPayload() {}

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

type ShieldResult struct{ Added int64 }
type ShieldEffectResult struct {
	ResultOutcome
	Result ShieldResult
}

func (ShieldEffectResult) isEffectResultPayload() {}

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

type RemoveStatusCommand struct {
	SourceOwner EntityID
	Target      EntityID
	Status      StatusHandle
	Meta        CommandMeta
	Event       EventContext
}

func (RemoveStatusCommand) isEffectCommandPayload() {}

type DispelStatusCommand struct {
	Target   EntityID
	Category string
	Count    int
	Meta     CommandMeta
	Event    EventContext
}

func (DispelStatusCommand) isEffectCommandPayload() {}

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

type StatusEffectResult struct {
	ResultOutcome
	Result StatusResult
}

func (StatusEffectResult) isEffectResultPayload() {}

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

type AttributeModifierResult struct {
	Applied bool
	DueTick Tick
}

type AttributeModifierEffectResult struct {
	ResultOutcome
	Result AttributeModifierResult
}

func (AttributeModifierEffectResult) isEffectResultPayload() {}
