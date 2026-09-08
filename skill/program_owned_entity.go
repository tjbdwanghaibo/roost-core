package skill

type spawnOperation struct {
	operationHeader
	effectContinuations
	effectIndex        EffectIndex
	template           UnitTemplateHandle
	position           programValue
	count              int
	durationTicks      Tick
	attributeOverrides []spawnAttributeOverrideProgram
	parameterBindings  []spawnParameterBindingProgram
}

type spawnAttributeOverrideProgram struct {
	attribute AttributeHandle
	value     programValue
}

type spawnParameterBindingProgram struct {
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

func (operation spawnOperation) isProgramOperation()             {}
func (operation spawnOperation) header() operationHeader         { return operation.operationHeader }
func (operation entityCommandOperation) isProgramOperation()     {}
func (operation entityCommandOperation) header() operationHeader { return operation.operationHeader }
