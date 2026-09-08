package skill

type numericTrackIR struct {
	source    sourceRef
	property  string
	operation string
	value     valueIR
	overTicks Tick
}

func (track numericTrackIR) walkValues(visitor valueVisitor) { walkValue(track.value, visitor) }

type modifyProcessEffectIR struct {
	source    sourceRef
	process   valueIR
	property  string
	operation string
	value     valueIR
	overTicks Tick
}

func (*modifyProcessEffectIR) isEffectIR()                 {}
func (effect *modifyProcessEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *modifyProcessEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.process, visitor)
	walkValue(effect.value, visitor)
}
