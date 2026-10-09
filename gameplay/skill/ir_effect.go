package skill

type effectIR interface {
	isEffectIR()
	sourceRef() sourceRef
	walkValues(valueVisitor)
}

type visualIR struct {
	category, theme string
	elements        []string
}

type damageEffectIR struct {
	source              sourceRef
	target, amount      valueIR
	damageType, element string
	combatTags          []string
	canCritical         bool
	visual              *visualIR
}

func (*damageEffectIR) isEffectIR()                 {}
func (e *damageEffectIR) sourceRef() sourceRef      { return e.source }
func (e *damageEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v); walkValue(e.amount, v) }

type healEffectIR struct {
	source         sourceRef
	target, amount valueIR
	visual         *visualIR
}

func (*healEffectIR) isEffectIR()                 {}
func (e *healEffectIR) sourceRef() sourceRef      { return e.source }
func (e *healEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v); walkValue(e.amount, v) }

type shieldEffectIR struct {
	source         sourceRef
	target, amount valueIR
	durationTicks  Tick
	visual         *visualIR
}

func (*shieldEffectIR) isEffectIR()                 {}
func (e *shieldEffectIR) sourceRef() sourceRef      { return e.source }
func (e *shieldEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v); walkValue(e.amount, v) }

type addStatusEffectIR struct {
	source        sourceRef
	target        valueIR
	status        string
	durationTicks Tick
	stacks        int
	maxStacks     *int
	visual        *visualIR
}

func (*addStatusEffectIR) isEffectIR()                 {}
func (e *addStatusEffectIR) sourceRef() sourceRef      { return e.source }
func (e *addStatusEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v) }

type removeStatusEffectIR struct {
	source sourceRef
	target valueIR
	status string
	visual *visualIR
}

func (*removeStatusEffectIR) isEffectIR()                 {}
func (e *removeStatusEffectIR) sourceRef() sourceRef      { return e.source }
func (e *removeStatusEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v) }

type attributeModifierEffectIR struct {
	source               sourceRef
	target               valueIR
	attribute, operation string
	value                valueIR
	durationTicks        Tick
	stackPolicy          string
	maxStacks            int
	visual               *visualIR
}

func (*attributeModifierEffectIR) isEffectIR()            {}
func (e *attributeModifierEffectIR) sourceRef() sourceRef { return e.source }
func (e *attributeModifierEffectIR) walkValues(v valueVisitor) {
	walkValue(e.target, v)
	walkValue(e.value, v)
}

type resourceEffectIR struct {
	source              sourceRef
	target, amount      valueIR
	resource, operation string
	visual              *visualIR
}

func (*resourceEffectIR) isEffectIR()                 {}
func (e *resourceEffectIR) sourceRef() sourceRef      { return e.source }
func (e *resourceEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v); walkValue(e.amount, v) }

type setMemoryEffectIR struct {
	source sourceRef
	name   string
	value  valueIR
}

func (*setMemoryEffectIR) isEffectIR()                 {}
func (e *setMemoryEffectIR) sourceRef() sourceRef      { return e.source }
func (e *setMemoryEffectIR) walkValues(v valueVisitor) { walkValue(e.value, v) }

type addMemoryEffectIR struct {
	source sourceRef
	name   string
	value  valueIR
}

func (*addMemoryEffectIR) isEffectIR()                 {}
func (e *addMemoryEffectIR) sourceRef() sourceRef      { return e.source }
func (e *addMemoryEffectIR) walkValues(v valueVisitor) { walkValue(e.value, v) }

type clearMemoryEffectIR struct {
	source sourceRef
	name   string
}

func (*clearMemoryEffectIR) isEffectIR()             {}
func (e *clearMemoryEffectIR) sourceRef() sourceRef  { return e.source }
func (*clearMemoryEffectIR) walkValues(valueVisitor) {}

type teleportEffectIR struct {
	source              sourceRef
	target, destination valueIR
	onBlocked           string
	visual              *visualIR
}

func (*teleportEffectIR) isEffectIR()            {}
func (e *teleportEffectIR) sourceRef() sourceRef { return e.source }
func (e *teleportEffectIR) walkValues(v valueVisitor) {
	walkValue(e.target, v)
	walkValue(e.destination, v)
}

type knockbackEffectIR struct {
	source                 sourceRef
	target, from, distance valueIR
	visual                 *visualIR
}

func (*knockbackEffectIR) isEffectIR()            {}
func (e *knockbackEffectIR) sourceRef() sourceRef { return e.source }
func (e *knockbackEffectIR) walkValues(v valueVisitor) {
	walkValue(e.target, v)
	walkValue(e.from, v)
	walkValue(e.distance, v)
}

type pullEffectIR struct {
	source                   sourceRef
	target, toward, distance valueIR
	visual                   *visualIR
}

func (*pullEffectIR) isEffectIR()            {}
func (e *pullEffectIR) sourceRef() sourceRef { return e.source }
func (e *pullEffectIR) walkValues(v valueVisitor) {
	walkValue(e.target, v)
	walkValue(e.toward, v)
	walkValue(e.distance, v)
}

type stopMovementEffectIR struct {
	source sourceRef
	target valueIR
	visual *visualIR
}

func (*stopMovementEffectIR) isEffectIR()                 {}
func (e *stopMovementEffectIR) sourceRef() sourceRef      { return e.source }
func (e *stopMovementEffectIR) walkValues(v valueVisitor) { walkValue(e.target, v) }

type modifyStateEffectIR struct {
	source        sourceRef
	state         string
	owner         valueIR
	subject       valueIR
	teamOf        valueIR
	operation     string
	value         valueIR
	durationTicks Tick
	expiryPolicy  string
}

func (*modifyStateEffectIR) isEffectIR()                 {}
func (effect *modifyStateEffectIR) sourceRef() sourceRef { return effect.source }
func (effect *modifyStateEffectIR) walkValues(visitor valueVisitor) {
	walkValue(effect.owner, visitor)
	walkValue(effect.subject, visitor)
	walkValue(effect.teamOf, visitor)
	walkValue(effect.value, visitor)
}
