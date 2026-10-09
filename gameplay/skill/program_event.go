package skill

type eventPlanProgram struct {
	filter FilterPlan
	proc   ProcPlan
}

type EventPlanView struct {
	RequiredTags     []GameplayTagHandle
	ExcludedTags     []GameplayTagHandle
	Elements         []ElementHandle
	DamageTypes      []DamageTypeHandle
	Results          []string
	MaxDepth         int
	MaxEventsPerRoot int
	AllowSelfTrigger bool
	OncePerRootEvent bool
}
