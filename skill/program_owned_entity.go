package skill

type summonOperation struct {
	operationHeader
	effectContinuations
	effectIndex        EffectIndex
	template           UnitTemplateHandle
	position           programValue
	count              int
	durationTicks      Tick
	attributeOverrides []summonAttributeOverrideProgram
	parameterBindings  []summonParameterBindingProgram
}

type summonAttributeOverrideProgram struct {
	attribute AttributeHandle
	value     programValue
}

type summonParameterBindingProgram struct {
	name  string
	value programValue
}

type entityCommandOperation struct {
	operationHeader
	effectContinuations
	effectIndex     EffectIndex
	target          programValue
	command         string
	position        programValue
	hasPosition     bool
	targetEntity    programValue
	hasTargetEntity bool
	behavior        string
}

func (operation summonOperation) isProgramOperation()            {}
func (operation summonOperation) header() operationHeader        { return operation.operationHeader }
func (operation entityCommandOperation) isProgramOperation()     {}
func (operation entityCommandOperation) header() operationHeader { return operation.operationHeader }
