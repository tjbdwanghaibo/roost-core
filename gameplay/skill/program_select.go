package skill

type consumerMode uint8

const (
	consumerOne consumerMode = iota + 1
	consumerEach
)

type selectorProgram struct {
	index         SelectorIndex
	element       selectionElementType
	shape         string
	limit         int
	consumerMode  consumerMode
	consumerRoot  OperationIndex
	emptyRoot     OperationIndex
	hasConsumer   bool
	hasEmpty      bool
	from          programValue
	shapePlan     shapeProgram
	filters       []filterProgram
	order         selectOrderProgram
	consumerLocal LocalIndex
	randomSite    RandomSiteIndex
	hasRandomSite bool
}

type shapeProgram struct {
	kind             string
	values           []programValue
	collision        []CollisionLayerHandle
	maxTargets       int
	allowRepeat      bool
	hopIntervalTicks Tick
}

type filterProgram struct {
	kind      string
	relation  string
	status    StatusHandle
	attribute AttributeHandle
	operation string
	value     programValue
	tag       GameplayTagHandle
	slot      int
	text      string
	template  UnitTemplateHandle
	cast      CastID
	tick      Tick
	collision []CollisionLayerHandle
}

type selectOrderProgram struct {
	by, direction string
}

type SelectionView struct {
	Index          SelectorIndex
	ElementKind    string
	Shape          string
	ShapeKind      string
	Limit          int
	ConsumerMode   string
	ConsumerRoot   OperationIndex
	EmptyRoot      OperationIndex
	OrderBy        string
	OrderDirection string
}

type EffectResultView struct {
	EffectIndex  EffectIndex
	ResultType   string
	FieldHandles []string
	SuccessRoot  OperationIndex
	FailureRoot  OperationIndex
	HasSuccess   bool
	HasFailure   bool
}
