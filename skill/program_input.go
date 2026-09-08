package skill

type inputSlotProgram struct {
	name string
	typ  valueType
}

type InputPort string

const (
	InputPortDirectionChanged InputPort = "direction_changed"
	InputPortTargetChanged    InputPort = "target_changed"
)

type inputProgram struct {
	kind                 inputKind
	slots                []inputSlotProgram
	maximumRange         int64
	hasMaximumRange      bool
	minimumLength        int64
	maximumLength        int64
	maximumPathPoints    int
	maximumPathLength    int64
	minimumSegmentLength int64
	clampPolicy          string
	simplificationPolicy string
	updatePorts          []InputPort
}

type InputSlotView struct {
	Name     string
	Kind     string
	Optional bool
	Quantity string
}

type InputProgramView struct {
	Kind                 string
	Slots                []InputSlotView
	MaximumRange         int64
	HasMaximumRange      bool
	MinimumLength        int64
	MaximumLength        int64
	MaximumPathPoints    int
	MaximumPathLength    int64
	MinimumSegmentLength int64
	ClampPolicy          string
	SimplificationPolicy string
	UpdatePorts          []InputPort
}

type InputLayoutView = InputProgramView
