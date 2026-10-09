package skill

type stateDeclarationIR struct {
	source               sourceRef
	name                 string
	declaredType         string
	scope                StateScope
	defaultValue         valueIR
	minimum              *int64
	maximum              *int64
	enumValues           []string
	durationTicks        Tick
	maximumDurationTicks Tick
	onWrite              string
	clearOn              []string
}

type stateReadValueIR struct {
	source       sourceRef
	state        string
	owner        valueIR
	subject      valueIR
	teamOf       valueIR
	snapshot     string
	resolvedType valueType
}

func (*stateReadValueIR) isValueIR()                 {}
func (value *stateReadValueIR) sourceRef() sourceRef { return value.source }
func (value *stateReadValueIR) valueType() valueType { return value.resolvedType }
