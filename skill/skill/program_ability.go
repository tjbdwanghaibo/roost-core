package skill

type abilityControlProgram struct {
	selectableTags []GameplayTagHandle
	ownerRelations []string
}

type abilityPropertyProgram struct {
	handle          AbilityPropertyHandle
	name            string
	typ             valueKind
	mutable         bool
	minimum         int64
	maximum         int64
	maximumMutation int64
	maximumDuration Tick
}

type abilityStateReadProgramValue struct {
	owner    programValue
	ability  programValue
	property AbilityPropertyHandle
	name     string
	snapshot string
	typ      valueType
}

func (abilityStateReadProgramValue) isProgramValue() {}

type abilityStateOperation struct {
	operationHeader
	effectContinuations
	effectIndex   EffectIndex
	owner         programValue
	ability       programValue
	property      AbilityPropertyHandle
	propertyName  string
	operation     string
	value         programValue
	durationTicks Tick
}

func (operation abilityStateOperation) isProgramOperation()     {}
func (operation abilityStateOperation) header() operationHeader { return operation.operationHeader }
