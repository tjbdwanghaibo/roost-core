package skill

type captureSnapshotEffectIR struct {
	source  sourceRef
	target  valueIR
	profile string
}

func (*captureSnapshotEffectIR) isEffectIR()                 {}
func (effect *captureSnapshotEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *captureSnapshotEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.target, visitor)
}

type restoreSnapshotEffectIR struct {
	source           sourceRef
	target, snapshot valueIR
	onBlocked        string
}

func (*restoreSnapshotEffectIR) isEffectIR()                 {}
func (effect *restoreSnapshotEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *restoreSnapshotEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.target, visitor)
	walkValue(effect.snapshot, visitor)
}
