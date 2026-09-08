package skill

type sourceRef struct{ Path string }

type sourcedIR struct{ source sourceRef }

func (s sourcedIR) sourceRef() sourceRef { return s.source }

type sourceMap struct{ refs []sourceRef }

func (m *sourceMap) add(path string) sourceRef {
	ref := sourceRef{Path: path}
	m.refs = append(m.refs, ref)
	return ref
}

type localSymbol struct {
	Name    string
	ScopeID int
}

type skillIR struct {
	source              sourceRef
	schema              string
	id                  string
	name                string
	description         string
	presentation        *skillPresentationIR
	gameplayTags        []string
	activation          activationIR
	input               inputIR
	cooldownTicks       Tick
	globalCooldownTicks Tick
	costs               []costIR
	memory              map[string]memoryDeclarationIR
	persistentState     map[string]stateDeclarationIR
	initialPhase        string
	phases              []phaseIR
}

type skillPresentationIR struct {
	iconKeywords []string
	cast         *visualIR
}

type costIR struct {
	source   sourceRef
	resource string
	amount   valueIR
}

type memoryDeclarationIR struct {
	source       sourceRef
	name         string
	declaredType string
	defaultValue valueIR
}

type phaseIR struct {
	source       sourceRef
	id           string
	timeoutTicks Tick
	events       phaseEventsIR
}

type phaseEventsIR struct {
	enter            flowIR
	recast           flowIR
	cancel           flowIR
	directionChanged flowIR
	targetChanged    flowIR
	timeout          flowIR
	release          flowIR
	pulse            flowIR
}

type castMode string

const (
	castModeTap    castMode = "tap"
	castModeToggle castMode = "toggle"
	castModeCharge castMode = "charge"
	castModeAmmo   castMode = "ammo"
	castModeHold   castMode = "hold"
)

type activationIR struct {
	source        sourceRef
	kind          string
	policy        castPolicyIR
	castWindow    castWindowIR
	cooldownScope string
	eventFilter   eventFilterIR
	procPolicy    procPolicyIR
}

type castWindowIR struct {
	windupTicks           Tick
	hasWindupExpression   bool
	windupExpression      valueIR
	windupTicksMin        Tick
	windupTicksMax        Tick
	commitTick            Tick
	recoveryTicks         Tick
	hasRecoveryExpression bool
	recoveryExpression    valueIR
	recoveryTicksMin      Tick
	recoveryTicksMax      Tick
	movement              string
	turning               string
	interruptTags         []string
	refundBeforeCommit    bool
	concurrent            bool
}

type eventFilterIR struct{ requiredTags, excludedTags, elements, damageTypes, results []string }
type procPolicyIR struct {
	maxDepth                           int
	allowSelfTrigger, oncePerRootEvent bool
}

type castPolicyIR struct {
	mode               castMode
	pulseIntervalTicks Tick
	maxDurationTicks   Tick
	maxChargeTicks     Tick
	minChargeBP        int64
	autoRelease        bool
	maxStock           int64
	rechargeTicks      Tick
	initialStock       int64
	sustainCosts       []costIR
}

type valueVisitor func(valueIR)
type effectVisitor func(effectIR)

type valueIR interface {
	isValueIR()
	sourceRef() sourceRef
	valueType() valueType
}

type nullValueIR struct{ source sourceRef }

func (*nullValueIR) isValueIR()             {}
func (v *nullValueIR) sourceRef() sourceRef { return v.source }
func (*nullValueIR) valueType() valueType   { return valueType{Base: valueKindNull, Optional: true} }

type intValueIR struct {
	source   sourceRef
	value    int64
	quantity quantityKind
}

func (*intValueIR) isValueIR()             {}
func (v *intValueIR) sourceRef() sourceRef { return v.source }
func (v *intValueIR) valueType() valueType {
	return valueType{Base: valueKindInt, Quantity: v.quantity}
}

type boolValueIR struct {
	source sourceRef
	value  bool
}

func (*boolValueIR) isValueIR()             {}
func (v *boolValueIR) sourceRef() sourceRef { return v.source }
func (*boolValueIR) valueType() valueType   { return valueType{Base: valueKindBool} }

type stringValueIR struct {
	source sourceRef
	value  string
}

func (*stringValueIR) isValueIR()             {}
func (v *stringValueIR) sourceRef() sourceRef { return v.source }
func (*stringValueIR) valueType() valueType   { return valueType{Base: valueKindString} }

type referenceValueIR struct {
	source       sourceRef
	reference    string
	resolvedType valueType
	resultField  ResultFieldHandle
}

func (*referenceValueIR) isValueIR()             {}
func (v *referenceValueIR) sourceRef() sourceRef { return v.source }
func (v *referenceValueIR) valueType() valueType { return v.resolvedType }

type expressionValueIR struct {
	source       sourceRef
	op           string
	args         []valueIR
	resolvedType valueType
}

func (*expressionValueIR) isValueIR()             {}
func (v *expressionValueIR) sourceRef() sourceRef { return v.source }
func (v *expressionValueIR) valueType() valueType { return v.resolvedType }

type attributeReadValueIR struct {
	source              sourceRef
	entity              valueIR
	attribute, snapshot string
	resolvedType        valueType
}

func (*attributeReadValueIR) isValueIR()             {}
func (v *attributeReadValueIR) sourceRef() sourceRef { return v.source }
func (v *attributeReadValueIR) valueType() valueType { return v.resolvedType }
