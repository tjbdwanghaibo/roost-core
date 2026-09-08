package skill

type spawnEffectIR struct {
	source             sourceRef
	template           string
	position           valueIR
	count              int
	durationTicks      Tick
	attributeOverrides []spawnAttributeOverrideIR
	parameterBindings  []spawnParameterBindingIR
}

type spawnAttributeOverrideIR struct {
	attribute string
	value     valueIR
}

type spawnParameterBindingIR struct {
	name  string
	value valueIR
}

func (*spawnEffectIR) isEffectIR()            {}
func (e *spawnEffectIR) sourceRef() sourceRef { return e.source }
func (e *spawnEffectIR) walkValues(v valueVisitor) {
	walkValue(e.position, v)
	for _, override := range e.attributeOverrides {
		walkValue(override.value, v)
	}
	for _, binding := range e.parameterBindings {
		walkValue(binding.value, v)
	}
}

type entityCommandEffectIR struct {
	source                 sourceRef
	target                 valueIR
	command                string
	position, targetEntity valueIR
	behavior               string
}

func (*entityCommandEffectIR) isEffectIR()            {}
func (e *entityCommandEffectIR) sourceRef() sourceRef { return e.source }
func (e *entityCommandEffectIR) walkValues(v valueVisitor) {
	walkValue(e.target, v)
	if e.position != nil {
		walkValue(e.position, v)
	}
	if e.targetEntity != nil {
		walkValue(e.targetEntity, v)
	}
}

type ownedSourceSkillFilterIR struct {
	sourcedIR
	skill string
}
type ownedSourceCastFilterIR struct {
	sourcedIR
	cast CastID
}
type ownedSpawnTickFilterIR struct {
	sourcedIR
	kind string
	tick Tick
}
type ownedUnitTemplateFilterIR struct {
	sourcedIR
	template string
}
type ownedEntityTagFilterIR struct {
	sourcedIR
	tag string
}

func (*ownedSourceSkillFilterIR) isFilterIR()              {}
func (*ownedSourceSkillFilterIR) walkValues(valueVisitor)  {}
func (*ownedUnitTemplateFilterIR) isFilterIR()             {}
func (*ownedUnitTemplateFilterIR) walkValues(valueVisitor) {}
func (*ownedEntityTagFilterIR) isFilterIR()                {}
func (*ownedEntityTagFilterIR) walkValues(valueVisitor)    {}
func (*ownedSourceCastFilterIR) isFilterIR()               {}
func (*ownedSourceCastFilterIR) walkValues(valueVisitor)   {}
func (*ownedSpawnTickFilterIR) isFilterIR()                {}
func (*ownedSpawnTickFilterIR) walkValues(valueVisitor)    {}
