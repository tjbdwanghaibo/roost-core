package skill

type modifyStatusInstanceOperation struct {
	operationHeader
	effectContinuations
	effectIndex     EffectIndex
	status          programValue
	operation       string
	value           programValue
	hasValue        bool
	target          programValue
	hasTarget       bool
	ownershipPolicy string
}

func (operation modifyStatusInstanceOperation) isProgramOperation() {}
func (operation modifyStatusInstanceOperation) header() operationHeader {
	return operation.operationHeader
}
