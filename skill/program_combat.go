package skill

type damageOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	amount      programValue
	damageType  DamageTypeHandle
	element     ElementHandle
	combatTags  []GameplayTagHandle
	canCritical bool
}

type healOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	amount      programValue
}

type shieldOperation struct {
	operationHeader
	effectContinuations
	effectIndex   EffectIndex
	target        programValue
	amount        programValue
	durationTicks Tick
}

type statusOperation struct {
	operationHeader
	effectContinuations
	effectIndex   EffectIndex
	target        programValue
	status        StatusHandle
	remove        bool
	durationTicks Tick
	stacks        int
	maxStacks     int
}

type attributeModifierOperation struct {
	operationHeader
	effectContinuations
	effectIndex   EffectIndex
	target        programValue
	attribute     AttributeHandle
	operation     string
	value         programValue
	durationTicks Tick
	stackPolicy   string
	maxStacks     int
}

type resourceOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	amount      programValue
	resource    ResourceHandle
	operation   string
}

type memoryOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	memory      MemoryIndex
	operation   string
	value       programValue
}

type teleportOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	destination programValue
	onBlocked   string
}

type motionImpulseOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	kind        string
	target      programValue
	origin      programValue
	distance    programValue
}

type stopMovementOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
}

type effectContinuations struct {
	success, failure       OperationIndex
	hasSuccess, hasFailure bool
	result                 resultLayoutProgram
	resultLocal            LocalIndex
	hasResultLocal         bool
	processTemplate        ProcessTemplateIndex
	hasProcess             bool
	visual                 VisualIndex
	hasVisual              bool
}

func (operation damageOperation) isProgramOperation()            {}
func (operation damageOperation) header() operationHeader        { return operation.operationHeader }
func (operation healOperation) isProgramOperation()              {}
func (operation healOperation) header() operationHeader          { return operation.operationHeader }
func (operation shieldOperation) isProgramOperation()            {}
func (operation shieldOperation) header() operationHeader        { return operation.operationHeader }
func (operation statusOperation) isProgramOperation()            {}
func (operation statusOperation) header() operationHeader        { return operation.operationHeader }
func (operation attributeModifierOperation) isProgramOperation() {}
func (operation attributeModifierOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation resourceOperation) isProgramOperation()      {}
func (operation resourceOperation) header() operationHeader  { return operation.operationHeader }
func (operation memoryOperation) isProgramOperation()        {}
func (operation memoryOperation) header() operationHeader    { return operation.operationHeader }
func (operation teleportOperation) isProgramOperation()      {}
func (operation teleportOperation) header() operationHeader  { return operation.operationHeader }
func (operation motionImpulseOperation) isProgramOperation() {}
func (operation motionImpulseOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation stopMovementOperation) isProgramOperation() {}
func (operation stopMovementOperation) header() operationHeader {
	return operation.operationHeader
}
