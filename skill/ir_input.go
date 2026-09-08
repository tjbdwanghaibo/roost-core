package skill

type inputKind string

const (
	inputNone              inputKind = "none"
	inputDirection         inputKind = "direction"
	inputPosition          inputKind = "position"
	inputEntity            inputKind = "entity"
	inputDirectionPosition inputKind = "direction_position"
	inputEntityPosition    inputKind = "entity_position"
	inputTwoPoint          inputKind = "two_point"
	inputDrag              inputKind = "drag"
	inputPath              inputKind = "path"
)

type inputIR struct {
	source               sourceRef
	kind                 inputKind
	maximumRange         *int64
	minimumLength        int64
	maximumLength        int64
	maximumPoints        int
	maximumTotalLength   int64
	minimumSegmentLength int64
	clampPolicy          string
	simplificationPolicy string
}
