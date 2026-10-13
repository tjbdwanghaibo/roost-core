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
	// GlobalCooldownTicks 为正时，施法提交时让施法者进入公共冷却；冷却结束前任何技能的根施法都以
	// ErrGlobalCooldownActive 被拒绝。它作为保留 program id "$gcd" 下的冷却条目同步给客户端。
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
