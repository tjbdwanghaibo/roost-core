package skill

type Program struct {
	id                        string
	name                      string
	description               string
	compilerSemanticsRevision string
	authority                 AuthorityIdentity
	identity                  programIdentity
	activationKind            string
	cooldownScope             string
	cooldownTicks             Tick
	globalCooldownTicks       Tick
	initialPhase              PhaseIndex
	cast                      castWindowProgram
	costs                     []costProgram
	gameplayTags              []GameplayTagHandle
	input                     inputProgram
	memory                    []memorySlotProgram
	states                    []stateSlotProgram
	abilityProperties         []abilityPropertyProgram
	abilityControl            abilityControlProgram
	locals                    []localSlotProgram
	phases                    []phaseProgram
	roots                     []rootProgram
	operations                []operation
	selectors                 []selectorProgram
	processTemplates          []processTemplateProgram
	processProperties         []processPropertyProgram
	quantities                []quantityProgram
	randomSites               []randomSiteProgram
	visuals                   []visualProgram
	castVisual                VisualIndex
	hasCastVisual             bool
	visualCatalogRevision     string
	visualCatalogDigest       string
	eventPlans                []eventPlanProgram
	snapshots                 []attributeSnapshotProgram
	limits                    ComputedLimits
}

func (program *Program) AuthorityIdentity() AuthorityIdentity {
	if program == nil {
		return AuthorityIdentity{}
	}
	return program.authority
}

func (program *Program) CompilerSemanticsRevision() string {
	if program == nil {
		return ""
	}
	return program.compilerSemanticsRevision
}

type costProgram struct {
	resource ResourceHandle
	amount   programValue
}

type castWindowProgram struct {
	windupTicks           Tick
	hasWindupExpression   bool
	windupExpression      programValue
	windupTicksMin        Tick
	windupTicksMax        Tick
	commitTick            Tick
	recoveryTicks         Tick
	hasRecoveryExpression bool
	recoveryExpression    programValue
	recoveryTicksMin      Tick
	recoveryTicksMax      Tick
	movement              string
	turning               string
	interruptTags         []GameplayTagHandle
	refundBeforeCommit    bool
	concurrent            bool
	mode                  castMode
	pulseIntervalTicks    Tick
	maxDurationTicks      Tick
	maxChargeTicks        Tick
	minChargeBP           int64
	autoRelease           bool
	maxStock              int64
	rechargeTicks         Tick
	initialStock          int64
	sustainCosts          []costProgram
}

type attributeSnapshotProgram struct {
	slot      int
	entity    programValue
	attribute AttributeHandle
	point     snapshotPoint
}

type localSlotProgram struct {
	index LocalIndex
	name  string
	typ   valueType
}

type memorySlotProgram struct {
	index        MemoryIndex
	name         string
	typ          valueType
	defaultValue programValue
}

type phaseProgram struct {
	index        PhaseIndex
	id           string
	timeoutTicks Tick
	roots        []RootIndex
}

type rootProgram struct {
	index        RootIndex
	phase        PhaseIndex
	event        string
	operation    OperationIndex
	hasOperation bool
}

type ProgramView struct {
	ID                        string
	Name                      string
	Description               string
	CompilerSemanticsRevision string
	Authority                 AuthorityIdentity
	Identity                  ProgramIdentityView
	CooldownTicks             Tick
	GlobalCooldownTicks       Tick
	Cast                      CastWindowView
	Costs                     []CostView
	GameplayTags              []GameplayTagHandle
	Input                     InputProgramView
	Memory                    []MemorySlotView
	PersistentState           []PersistentStateView
	Locals                    []LocalSlotView
	Phases                    []PhaseView
	Roots                     []RootView
	Operations                []OperationView
	Selectors                 []SelectionView
	RandomSites               []RandomSiteView
	Limits                    MetricSnapshot
}

type CastWindowView struct {
	WindupTicks           Tick
	HasWindupExpression   bool
	WindupTicksMin        Tick
	WindupTicksMax        Tick
	CommitTick            Tick
	RecoveryTicks         Tick
	HasRecoveryExpression bool
	RecoveryTicksMin      Tick
	RecoveryTicksMax      Tick
	Concurrent            bool
	Movement              string
	Turning               string
	InterruptTags         []GameplayTagHandle
	RefundBeforeCommit    bool
	Mode                  string
	PulseIntervalTicks    Tick
	MaxDurationTicks      Tick
	MaxChargeTicks        Tick
	MinChargeBP           int64
	AutoRelease           bool
	MaxStock              int64
	RechargeTicks         Tick
	InitialStock          int64
	SustainCostCount      int
}

type CostView struct {
	Resource ResourceHandle
	Kind     string
	Quantity string
}

type MemorySlotView struct {
	Index    MemoryIndex
	Name     string
	Kind     string
	Optional bool
	Quantity string
}

type LocalSlotView struct {
	Index    LocalIndex
	Kind     string
	Optional bool
	Quantity string
}

type PhaseView struct {
	Index        PhaseIndex
	ID           string
	TimeoutTicks Tick
	Roots        []RootIndex
}

type RootView struct {
	Index        RootIndex
	Phase        PhaseIndex
	Event        string
	Operation    OperationIndex
	HasOperation bool
}

type OperationView struct {
	Index    OperationIndex
	Kind     string
	Children []OperationIndex
}

type MetricSnapshot struct {
	ComputedLimits
}

type CombatSemanticView struct {
	DamageEffects int
	Damage        []DamageSemanticView
}

type DamageSemanticView struct {
	EffectIndex EffectIndex
	DamageType  DamageTypeHandle
	Element     ElementHandle
	CombatTags  []GameplayTagHandle
	CanCritical bool
}

type StateLayoutView struct {
	Memory          []MemorySlotView
	PersistentState []PersistentStateView
	Locals          []LocalSlotView
}

type AbilityControlView struct{ Operations int }
type OwnedEntityView struct{ Operations int }
type StatusOperationView struct{ Operations int }
type TemporalProfileView struct{ Profiles int }
