package skill

type abilityStateReadValueIR struct {
	source       sourceRef
	owner        valueIR
	ability      valueIR
	property     string
	snapshot     string
	resolvedType valueType
}

func (*abilityStateReadValueIR) isValueIR()                 {}
func (value *abilityStateReadValueIR) sourceRef() sourceRef { return value.source }
func (value *abilityStateReadValueIR) valueType() valueType { return value.resolvedType }

type modifyAbilityStateEffectIR struct {
	source        sourceRef
	owner         valueIR
	ability       valueIR
	property      string
	operation     string
	value         valueIR
	durationTicks Tick
}

type abilityTagFilterIR struct {
	sourcedIR
	tag string
}

func (*abilityTagFilterIR) isFilterIR()             {}
func (*abilityTagFilterIR) walkValues(valueVisitor) {}

type abilitySlotFilterIR struct {
	sourcedIR
	slot int
}

func (*abilitySlotFilterIR) isFilterIR()             {}
func (*abilitySlotFilterIR) walkValues(valueVisitor) {}

func (*modifyAbilityStateEffectIR) isEffectIR()                 {}
func (effect *modifyAbilityStateEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *modifyAbilityStateEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.owner, visitor)
	walkValue(effect.ability, visitor)
	walkValue(effect.value, visitor)
}
