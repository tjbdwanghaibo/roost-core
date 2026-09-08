package skill

type Tick int64

type Definition struct {
	Schema        string
	ID            string
	Name          string
	Description   string
	Presentation  *SkillPresentation
	GameplayTags  []string
	Activation    ActivationDefinition
	InputSchema   InputSchemaDefinition
	CooldownTicks Tick
	// GlobalCooldownTicks, when positive, puts the caster on a shared global
	// cooldown at cast commit; root activations of any skill are rejected
	// with ErrGlobalCooldownActive until it elapses. Synced to clients as a
	// cooldown entry under the reserved program id "$gcd".
	GlobalCooldownTicks Tick
	Costs               []Cost
	Memory              map[string]MemoryDeclaration
	PersistentState     map[string]PersistentStateDefinition
	InitialPhase        string
	Phases              []PhaseDefinition
}

type Cost struct {
	Resource string `json:"resource"`
	Amount   Value  `json:"-"`
}

type MemoryDeclaration struct {
	Type    string
	Default Value
}

type PhaseDefinition struct {
	ID           string
	TimeoutTicks Tick
	On           PhaseEventsDefinition
}

type PhaseEventsDefinition struct {
	Enter            FlowDefinition
	Recast           FlowDefinition
	Cancel           FlowDefinition
	DirectionChanged FlowDefinition
	TargetChanged    FlowDefinition
	Timeout          FlowDefinition
	Release          FlowDefinition
	Pulse            FlowDefinition
}
