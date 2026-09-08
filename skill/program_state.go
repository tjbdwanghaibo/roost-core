package skill

type stateSlotProgram struct {
	slot                 StateSlot
	name                 string
	typ                  valueType
	scope                StateScope
	defaultValue         programValue
	minimum              int64
	maximum              int64
	enumValues           []string
	durationTicks        Tick
	maximumDurationTicks Tick
	onWrite              string
	clearOn              []string
}

type PersistentStateView struct {
	Slot                 StateSlot
	Name                 string
	Type                 string
	Scope                StateScope
	Minimum              int64
	Maximum              int64
	EnumValues           []string
	DurationTicks        Tick
	MaximumDurationTicks Tick
	OnWrite              string
	ClearOn              []string
}

type stateReferenceProgram struct {
	shared               SharedStateHandle
	slot                 StateSlot
	typ                  valueType
	scope                StateScope
	defaultValue         programValue
	minimum              int64
	maximum              int64
	durationTicks        Tick
	maximumDurationTicks Tick
	onWrite              string
	clearOn              []string
}

type stateBindingProgram struct {
	owner, subject, teamOf          programValue
	hasOwner, hasSubject, hasTeamOf bool
}

type stateReadProgramValue struct {
	state    stateReferenceProgram
	binding  stateBindingProgram
	snapshot string
	typ      valueType
}

func (stateReadProgramValue) isProgramValue() {}

type stateOperation struct {
	operationHeader
	effectContinuations
	effectIndex   EffectIndex
	state         stateReferenceProgram
	binding       stateBindingProgram
	operation     string
	value         programValue
	hasValue      bool
	durationTicks Tick
	expiryPolicy  string
}

func (operation stateOperation) isProgramOperation()     {}
func (operation stateOperation) header() operationHeader { return operation.operationHeader }
