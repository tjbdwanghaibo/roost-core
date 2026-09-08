package skill

type operation interface {
	isProgramOperation()
	header() operationHeader
}

type operationHeader struct {
	index      OperationIndex
	sourcePath string
}

type programValue interface{ isProgramValue() }

type nullProgramValue struct{ typ valueType }
type intProgramValue struct {
	value int64
	typ   valueType
}
type boolProgramValue struct{ value bool }
type stringProgramValue struct{ value string }

type referenceProgramKind uint8

const (
	referenceBuiltin referenceProgramKind = iota + 1
	referenceInput
	referenceMemory
	referenceLocal
)

type referenceProgramValue struct {
	kind        referenceProgramKind
	builtin     string
	index       uint16
	field       string
	resultField ResultFieldHandle
	typ         valueType
}

type expressionProgramValue struct {
	op   string
	args []programValue
	typ  valueType
}

type attributeReadProgramValue struct {
	entity       programValue
	attribute    AttributeHandle
	snapshot     snapshotPoint
	snapshotSlot int
	typ          valueType
}

func (nullProgramValue) isProgramValue()          {}
func (intProgramValue) isProgramValue()           {}
func (boolProgramValue) isProgramValue()          {}
func (stringProgramValue) isProgramValue()        {}
func (referenceProgramValue) isProgramValue()     {}
func (expressionProgramValue) isProgramValue()    {}
func (attributeReadProgramValue) isProgramValue() {}

type sequenceOperation struct {
	operationHeader
	children []OperationIndex
}
type parallelOperation struct {
	operationHeader
	branches []OperationIndex
}
type branchOperation struct {
	operationHeader
	condition     programValue
	thenOperation OperationIndex
	elseOperation OperationIndex
	hasElse       bool
}
type repeatOperation struct {
	operationHeader
	times         programValue
	intervalTicks Tick
	indexLocal    LocalIndex
	body          OperationIndex
}
type waitOperation struct {
	operationHeader
	ticks Tick
	then  OperationIndex
}
type queryOperation struct {
	operationHeader
	selector SelectorIndex
}
type gotoOperation struct {
	operationHeader
	phase PhaseIndex
}
type finishOperation struct {
	operationHeader
	reason string
}

type modifyProcessOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	process     programValue
	property    ProcessPropertyHandle
	operation   processNumericOperation
	value       programValue
	overTicks   Tick
}

func (operation modifyProcessOperation) isProgramOperation() {}
func (operation modifyProcessOperation) header() operationHeader {
	return operation.operationHeader
}

func (operation sequenceOperation) isProgramOperation() {}
func (operation sequenceOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation parallelOperation) isProgramOperation() {}
func (operation parallelOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation branchOperation) isProgramOperation() {}
func (operation branchOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation repeatOperation) isProgramOperation() {}
func (operation repeatOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation waitOperation) isProgramOperation() {}
func (operation waitOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation queryOperation) isProgramOperation() {}
func (operation queryOperation) header() operationHeader {
	return operation.operationHeader
}
func (operation gotoOperation) isProgramOperation()     {}
func (operation gotoOperation) header() operationHeader { return operation.operationHeader }
func (operation finishOperation) isProgramOperation()   {}
func (operation finishOperation) header() operationHeader {
	return operation.operationHeader
}

func operationKind(value operation) string {
	switch value.(type) {
	case sequenceOperation:
		return "sequence"
	case parallelOperation:
		return "parallel"
	case branchOperation:
		return "branch"
	case repeatOperation:
		return "repeat"
	case waitOperation:
		return "wait"
	case queryOperation:
		return "query"
	case captureSnapshotOperation:
		return "capture_snapshot"
	case restoreSnapshotOperation:
		return "restore_snapshot"
	case damageOperation:
		return "damage"
	case healOperation:
		return "heal"
	case shieldOperation:
		return "shield"
	case statusOperation:
		return "status"
	case modifyStatusInstanceOperation:
		return "modify_status_instance"
	case attributeModifierOperation:
		return "attribute_modifier"
	case resourceOperation:
		return "resource"
	case memoryOperation:
		return "memory"
	case stateOperation:
		return "state"
	case abilityStateOperation:
		return "ability_state"
	case spawnOperation:
		return "spawn"
	case entityCommandOperation:
		return "entity_command"
	case teleportOperation:
		return "teleport"
	case motionImpulseOperation:
		return "motion_impulse"
	case stopMovementOperation:
		return "stop_movement"
	case modifyProcessOperation:
		return "modify_process"
	case gotoOperation:
		return "goto"
	case finishOperation:
		return "finish"
	default:
		return "invalid"
	}
}
