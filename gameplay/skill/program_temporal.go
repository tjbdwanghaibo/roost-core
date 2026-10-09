package skill

type captureSnapshotOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	profile     TemporalProfileHandle
}

type restoreSnapshotOperation struct {
	operationHeader
	effectContinuations
	effectIndex EffectIndex
	target      programValue
	snapshot    programValue
	onBlocked   string
}

func (captureSnapshotOperation) isProgramOperation() {}
func (operation captureSnapshotOperation) header() operationHeader {
	return operation.operationHeader
}

func (restoreSnapshotOperation) isProgramOperation() {}
func (operation restoreSnapshotOperation) header() operationHeader {
	return operation.operationHeader
}
