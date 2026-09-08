package skill

import "math"

type EntityID uint64

// SnapshotToken is intentionally opaque. Only the runtime/host snapshot store
// can mint a non-zero token; skill definitions and callers can only carry it.
type SnapshotToken struct{ opaque uint64 }

// NewSnapshotToken lets an authoritative Host expose an opaque store-issued
// identifier without making the token's representation mutable.
func NewSnapshotToken(opaque uint64) (SnapshotToken, error) {
	if opaque == 0 {
		return SnapshotToken{}, ErrCastInputInvalid
	}
	return SnapshotToken{opaque: opaque}, nil
}

func (token SnapshotToken) OpaqueID() uint64 { return token.opaque }

type Position struct{ X, Y int64 }
type Direction struct{ X, Y int64 }
type Hit struct {
	Entity     EntityID
	Position   Position
	Distance   int64
	ColliderID uint64
}

type AbilityRef struct {
	Owner  EntityID
	Handle AbilityHandle
}

type RuntimeValue struct {
	present      bool
	typ          valueType
	integer      int64
	boolean      bool
	text         string
	entity       EntityID
	position     Position
	direction    Direction
	hit          Hit
	path         []Position
	ability      AbilityRef
	status       StatusInstanceRef
	entities     []EntityID
	strings      []string
	snapshot     SnapshotToken
	process      ProcessID
	effectResult runtimeEffectResultValue
}

func MissingRuntimeValue(typ valueType) RuntimeValue {
	typ.Optional = true
	return RuntimeValue{typ: typ}
}

func IntRuntimeValue(value int64, quantity quantityKind) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindInt, Quantity: quantity}, integer: value}
}

func BoolRuntimeValue(value bool) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindBool}, boolean: value}
}

func StringRuntimeValue(value string) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindString}, text: value}
}

func EntityRuntimeValue(value EntityID) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindEntity}, entity: value}
}

func PositionRuntimeValue(value Position) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindPosition}, position: value}
}

func DirectionRuntimeValue(value Direction) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindDirection}, direction: value}
}

func HitRuntimeValue(value Hit) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindHit}, hit: value}
}

func PathRuntimeValue(value []Position) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindPath}, path: append([]Position(nil), value...)}
}

func AbilityRuntimeValue(value AbilityRef) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindAbility}, ability: value}
}

func StatusInstanceRuntimeValue(value StatusInstanceRef) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindStatusInstance}, status: value}
}

func EntityListRuntimeValue(value []EntityID) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindEntityList}, entities: append([]EntityID(nil), value...)}
}

func StringListRuntimeValue(value []string) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindStringList}, strings: append([]string(nil), value...)}
}

func SnapshotTokenRuntimeValue(value SnapshotToken) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindSnapshotToken}, snapshot: value}
}

func ProcessRuntimeValue(value ProcessID) RuntimeValue {
	return RuntimeValue{present: true, typ: valueType{Base: valueKindProcess}, process: value}
}

func (value RuntimeValue) Present() bool   { return value.present }
func (value RuntimeValue) Type() valueType { return value.typ }
func (value RuntimeValue) Int() (int64, bool) {
	return value.integer, value.present && value.typ.Base == valueKindInt
}
func (value RuntimeValue) Bool() (bool, bool) {
	return value.boolean, value.present && value.typ.Base == valueKindBool
}
func (value RuntimeValue) String() (string, bool) {
	return value.text, value.present && value.typ.Base == valueKindString
}
func (value RuntimeValue) Entity() (EntityID, bool) {
	return value.entity, value.present && value.typ.Base == valueKindEntity
}
func (value RuntimeValue) Position() (Position, bool) {
	return value.position, value.present && value.typ.Base == valueKindPosition
}
func (value RuntimeValue) Direction() (Direction, bool) {
	return value.direction, value.present && value.typ.Base == valueKindDirection
}
func (value RuntimeValue) Hit() (Hit, bool) {
	return value.hit, value.present && value.typ.Base == valueKindHit
}
func (value RuntimeValue) Path() ([]Position, bool) {
	return append([]Position(nil), value.path...), value.present && value.typ.Base == valueKindPath
}
func (value RuntimeValue) Ability() (AbilityRef, bool) {
	return value.ability, value.present && value.typ.Base == valueKindAbility
}
func (value RuntimeValue) StatusInstance() (StatusInstanceRef, bool) {
	return value.status, value.present && value.typ.Base == valueKindStatusInstance
}
func (value RuntimeValue) Entities() ([]EntityID, bool) {
	return append([]EntityID(nil), value.entities...), value.present && value.typ.Base == valueKindEntityList
}
func (value RuntimeValue) Strings() ([]string, bool) {
	return append([]string(nil), value.strings...), value.present && value.typ.Base == valueKindStringList
}
func (value RuntimeValue) SnapshotToken() (SnapshotToken, bool) {
	return value.snapshot, value.present && value.typ.Base == valueKindSnapshotToken
}

func (value RuntimeValue) Process() (ProcessID, bool) {
	return value.process, value.present && value.typ.Base == valueKindProcess
}

func (value RuntimeValue) effectResultField(handle ResultFieldHandle) (RuntimeValue, bool) {
	if !value.present || value.typ.Base != valueKindEffectResult || handle == 0 || int(handle) > len(value.effectResult.fields) {
		return RuntimeValue{}, false
	}
	field := value.effectResult.fields[int(handle)-1]
	return cloneRuntimeValue(field), true
}

func CheckedAddRuntimeValues(left, right RuntimeValue) (RuntimeValue, error) {
	if !left.present || !right.present {
		return RuntimeValue{}, ErrRuntimeValueMissing
	}
	if left.typ.Base != valueKindInt || right.typ.Base != valueKindInt {
		return RuntimeValue{}, ErrRuntimeTypeMismatch
	}
	if left.typ.Quantity != right.typ.Quantity {
		return RuntimeValue{}, ErrRuntimeQuantityMismatch
	}
	if (right.integer > 0 && left.integer > math.MaxInt64-right.integer) || (right.integer < 0 && left.integer < math.MinInt64-right.integer) {
		return RuntimeValue{}, ErrRuntimeArithmeticOverflow
	}
	return IntRuntimeValue(left.integer+right.integer, left.typ.Quantity), nil
}

func CheckedScaleBPRuntimeValue(value, basisPoints RuntimeValue) (RuntimeValue, error) {
	if !value.present || !basisPoints.present {
		return RuntimeValue{}, ErrRuntimeValueMissing
	}
	if value.typ.Base != valueKindInt || basisPoints.typ.Base != valueKindInt {
		return RuntimeValue{}, ErrRuntimeTypeMismatch
	}
	if basisPoints.typ.Quantity != quantityBasisPoints {
		return RuntimeValue{}, ErrRuntimeQuantityMismatch
	}
	product, ok := checkedInt64Mul(value.integer, basisPoints.integer)
	if !ok {
		return RuntimeValue{}, ErrRuntimeArithmeticOverflow
	}
	return IntRuntimeValue(product/10000, value.typ.Quantity), nil
}

func checkedInt64Mul(left, right int64) (int64, bool) {
	if left == 0 || right == 0 {
		return 0, true
	}
	if (left == math.MinInt64 && right == -1) || (right == math.MinInt64 && left == -1) {
		return 0, false
	}
	product := left * right
	return product, product/right == left
}

type selectionElement struct {
	entity   EntityID
	position Position
	hit      Hit
	ability  AbilityRef
	status   StatusInstanceRef
}

type Selection struct {
	elementType selectionElementType
	elements    []selectionElement
	meta        QueryResultMeta
	query       SelectionQueryMeta
}

type SelectionQueryMeta struct {
	Revision  WorldRevision
	Order     SelectOrderBy
	Direction SelectDirection
	Limit     int
}

func (selection Selection) ElementType() string       { return selectionElementName(selection.elementType) }
func (selection Selection) Meta() QueryResultMeta     { return selection.meta }
func (selection Selection) Query() SelectionQueryMeta { return selection.query }
func (selection Selection) EntityIDs() []EntityID {
	result := make([]EntityID, len(selection.elements))
	for index, element := range selection.elements {
		result[index] = element.entity
	}
	return result
}

func (selection Selection) Hits() []Hit {
	result := make([]Hit, len(selection.elements))
	for index, element := range selection.elements {
		result[index] = element.hit
	}
	return result
}

func (selection Selection) Positions() []Position {
	result := make([]Position, len(selection.elements))
	for index, element := range selection.elements {
		result[index] = element.position
	}
	return result
}

func (selection Selection) StatusInstances() []StatusInstanceRef {
	result := make([]StatusInstanceRef, len(selection.elements))
	for index, element := range selection.elements {
		result[index] = element.status
	}
	return result
}
