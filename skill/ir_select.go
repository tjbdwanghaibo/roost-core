package skill

type selectionElementType uint8

const (
	selectionEntity selectionElementType = iota + 1
	selectionPosition
	selectionHit
	selectionPath
	selectionAbility
	selectionStatusInstance
)

type selectIR struct {
	source      sourceRef
	from        valueIR
	elementType selectionElementType
	shape       shapeIR
	filters     []filterIR
	order       *selectOrderIR
	limit       int
}

func (s *selectIR) walkValues(visitor valueVisitor) {
	walkValue(s.from, visitor)
	s.shape.walkValues(visitor)
	for _, filter := range s.filters {
		filter.walkValues(visitor)
	}
}

type selectOrderIR struct{ by, direction string }

type selectConsumeIR interface {
	isSelectConsumeIR()
	sourceRef() sourceRef
	walkValues(valueVisitor)
}
type selectOneConsumeIR struct {
	sourcedIR
	local localSymbol
	then  flowIR
}

func (*selectOneConsumeIR) isSelectConsumeIR() {}
func (c *selectOneConsumeIR) walkValues(visitor valueVisitor) {
	walkOptionalFlowValues(c.then, visitor)
}

type selectEachConsumeIR struct {
	sourcedIR
	local localSymbol
	body  flowIR
}

func (*selectEachConsumeIR) isSelectConsumeIR() {}
func (c *selectEachConsumeIR) walkValues(visitor valueVisitor) {
	walkOptionalFlowValues(c.body, visitor)
}

type shapeIR interface {
	isShapeIR()
	sourceRef() sourceRef
	walkValues(valueVisitor)
}
type singleShapeIR struct{ sourcedIR }

func (*singleShapeIR) isShapeIR()              {}
func (*singleShapeIR) walkValues(valueVisitor) {}

type circleShapeIR struct {
	sourcedIR
	radius valueIR
}

func (*circleShapeIR) isShapeIR()                  {}
func (s *circleShapeIR) walkValues(v valueVisitor) { walkValue(s.radius, v) }

type ringShapeIR struct {
	sourcedIR
	innerRadius, outerRadius valueIR
}

func (*ringShapeIR) isShapeIR() {}
func (s *ringShapeIR) walkValues(v valueVisitor) {
	walkValue(s.innerRadius, v)
	walkValue(s.outerRadius, v)
}

type coneShapeIR struct {
	sourcedIR
	rangeValue, angleDeg, direction valueIR
}

func (*coneShapeIR) isShapeIR() {}
func (s *coneShapeIR) walkValues(v valueVisitor) {
	walkValue(s.rangeValue, v)
	walkValue(s.angleDeg, v)
	walkValue(s.direction, v)
}

type lineShapeIR struct {
	sourcedIR
	length, width, direction valueIR
}

func (*lineShapeIR) isShapeIR() {}
func (s *lineShapeIR) walkValues(v valueVisitor) {
	walkValue(s.length, v)
	walkValue(s.width, v)
	walkValue(s.direction, v)
}

type rectangleShapeIR struct {
	sourcedIR
	length, width, direction valueIR
}

func (*rectangleShapeIR) isShapeIR() {}
func (s *rectangleShapeIR) walkValues(v valueVisitor) {
	walkValue(s.length, v)
	walkValue(s.width, v)
	walkValue(s.direction, v)
}

type raycastShapeIR struct {
	sourcedIR
	length, direction valueIR
	collision         []string
}

func (*raycastShapeIR) isShapeIR() {}
func (s *raycastShapeIR) walkValues(v valueVisitor) {
	walkValue(s.length, v)
	walkValue(s.direction, v)
}

type chainShapeIR struct {
	sourcedIR
	hopRange         valueIR
	maxTargets       int
	allowRepeat      bool
	hopIntervalTicks Tick
}

func (*chainShapeIR) isShapeIR()                  {}
func (s *chainShapeIR) walkValues(v valueVisitor) { walkValue(s.hopRange, v) }

type pathShapeIR struct {
	sourcedIR
	points valueIR
}

func (*pathShapeIR) isShapeIR()                  {}
func (s *pathShapeIR) walkValues(v valueVisitor) { walkValue(s.points, v) }

type nearestValidShapeIR struct {
	sourcedIR
	searchRadius valueIR
	collision    []string
}

func (*nearestValidShapeIR) isShapeIR()                  {}
func (s *nearestValidShapeIR) walkValues(v valueVisitor) { walkValue(s.searchRadius, v) }

type abilitySetShapeIR struct{ sourcedIR }

func (*abilitySetShapeIR) isShapeIR()              {}
func (*abilitySetShapeIR) walkValues(valueVisitor) {}

type statusSetShapeIR struct{ sourcedIR }

func (*statusSetShapeIR) isShapeIR()              {}
func (*statusSetShapeIR) walkValues(valueVisitor) {}

type ownedEntitiesShapeIR struct{ sourcedIR }

func (*ownedEntitiesShapeIR) isShapeIR()              {}
func (*ownedEntitiesShapeIR) walkValues(valueVisitor) {}

type filterIR interface {
	isFilterIR()
	sourceRef() sourceRef
	walkValues(valueVisitor)
}
type flagFilterIR struct {
	sourcedIR
	kind string
}

func (*flagFilterIR) isFilterIR()             {}
func (*flagFilterIR) walkValues(valueVisitor) {}

type relationFilterIR struct {
	sourcedIR
	relation string
}

func (*relationFilterIR) isFilterIR()             {}
func (*relationFilterIR) walkValues(valueVisitor) {}

type statusFilterIR struct {
	sourcedIR
	kind, status string
}

func (*statusFilterIR) isFilterIR()             {}
func (*statusFilterIR) walkValues(valueVisitor) {}

type attributeCompareFilterIR struct {
	sourcedIR
	attribute, op string
	value         valueIR
}

func (*attributeCompareFilterIR) isFilterIR()                 {}
func (f *attributeCompareFilterIR) walkValues(v valueVisitor) { walkValue(f.value, v) }

type gameplayTagFilterIR struct {
	sourcedIR
	kind, tag string
}

func (*gameplayTagFilterIR) isFilterIR()             {}
func (*gameplayTagFilterIR) walkValues(valueVisitor) {}

type lineOfSightFilterIR struct {
	sourcedIR
	collision []string
}

func (*lineOfSightFilterIR) isFilterIR()             {}
func (*lineOfSightFilterIR) walkValues(valueVisitor) {}
