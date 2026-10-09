package skill

type numericTrackIR struct {
	source    sourceRef
	property  string
	operation string
	value     valueIR
	overTicks Tick
}

func (track numericTrackIR) walkValues(visitor valueVisitor) { walkValue(track.value, visitor) }

type modifySpawnEffectIR struct {
	source    sourceRef
	spawn     valueIR
	property  string
	operation string
	value     valueIR
	overTicks Tick
}

func (*modifySpawnEffectIR) isEffectIR()                 {}
func (effect *modifySpawnEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *modifySpawnEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.spawn, visitor)
	walkValue(effect.value, visitor)
}
