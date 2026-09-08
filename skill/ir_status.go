package skill

type statusInstanceFilterIR struct {
	sourcedIR
	kind, text, operation string
	status                string
	value                 valueIR
}

func (*statusInstanceFilterIR) isFilterIR() {}
func (filter *statusInstanceFilterIR) walkValues(visitor valueVisitor) {
	if filter.value != nil {
		walkValue(filter.value, visitor)
	}
}

type modifyStatusInstanceEffectIR struct {
	source          sourceRef
	status          valueIR
	operation       string
	value           valueIR
	target          valueIR
	ownershipPolicy string
}

func (*modifyStatusInstanceEffectIR) isEffectIR()                 {}
func (effect *modifyStatusInstanceEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *modifyStatusInstanceEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.status, visitor)
	if effect.value != nil {
		walkValue(effect.value, visitor)
	}
	if effect.target != nil {
		walkValue(effect.target, visitor)
	}
}
