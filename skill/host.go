package skill

// Host is the world-authority surface the Runtime drives. Implementations
// must honor the following concurrency contract:
//
//   - Every Host method is invoked while the Runtime holds its internal lock:
//     calls are strictly serialized, and implementations never need their own
//     synchronization against concurrent Runtime callbacks.
//   - Host methods must not re-enter the Runtime (Start, Advance, Input,
//     Cancel, StateDeltas, ...). The Runtime lock is not reentrant; a
//     re-entrant call deadlocks. World reactions to skill effects belong in
//     Events, which the Runtime polls at deterministic points.
//   - Host methods must not block on channels, locks held by other
//     goroutines that call the Runtime, or I/O with unbounded latency. A
//     blocked Host call stalls every caster on this Runtime.
//   - Results must be deterministic functions of world state at the request
//     revision. Wall-clock time, map iteration order, and goroutine timing
//     must never influence a result; replay and checkpoint recovery re-issue
//     the same calls and must observe identical answers.
type Host interface {
	AuthorityProvider
	StateStore
	Advance(tick Tick) (WorldRevision, error)
	CurrentRevision() WorldRevision
	Read(request ReadRequest) (ReadResult, error)
	Select(request SelectRequest) (SelectResult, error)
	PayCosts(payment CostPayment) (CommitReceipt, error)
	Apply(command EffectCommand) (EffectResult, error)
	StepProcess(command ProcessStepCommand, state ProcessHostState) (ProcessStepResult, error)
	StopProcess(command ProcessStopCommand, state ProcessHostState) (CommitReceipt, error)
	Events(after EventCursor) []RuntimeEvent
}

// HostEventCompactor is an optional single-consumer optimization. A Host must
// implement it only when the Runtime is the exclusive consumer of Events.
//
// Retention contract: a compacting host must keep every event emitted since
// the last successful Checkpoint. RestoreRuntime rewinds the event cursor to
// the checkpoint's value and replays forward from there; events compacted
// past that cursor are unrecoverable.
type HostEventCompactor interface {
	CompactEventsThrough(EventCursor)
}

// InputPositionResolver supplies authoritative blocked-position facts without
// moving deterministic range and path arithmetic out of the Runtime.
type InputPositionResolver interface {
	ResolveInputPosition(InputPositionRequest) (Position, bool)
}

type InputPositionRequest struct {
	Caster   EntityID
	Position Position
	Policy   string
}

// AbilityRelationProvider supplies the authoritative relation between the
// activating owner and an ability owner without exposing ability state to the
// world query interface.
type AbilityRelationProvider interface {
	AbilityOwnerRelation(viewer, owner EntityID) (string, bool)
}

type ReadRequest struct {
	Meta    QueryMeta
	Payload ReadPayload
}

type ReadPayload interface{ isReadPayload() }

type ResourceRead struct {
	Entity   EntityID
	Resource string
}

func (ResourceRead) isReadPayload() {}

type PositionRead struct{ Entity EntityID }

func (PositionRead) isReadPayload() {}

type AttributeRead struct {
	Entity    EntityID
	Attribute AttributeHandle
}

func (AttributeRead) isReadPayload() {}

type ReadResult struct {
	Meta  QueryResultMeta
	Value RuntimeValue
}

type SelectRequest struct {
	Meta        QueryMeta
	Caster      EntityID
	ElementKind string
	Shape       SelectShape
	Filters     []SelectFilter
	Order       SelectOrder
	Limit       int
}

type SelectResult struct {
	Meta      QueryResultMeta
	Selection Selection
}

type SelectShape interface{ isSelectShape() }

type SingleSelectShape struct{ Entity EntityID }
type CircleSelectShape struct {
	Center Position
	Radius int64
}
type RingSelectShape struct {
	Center                   Position
	InnerRadius, OuterRadius int64
}
type ConeSelectShape struct {
	Origin    Position
	Direction Direction
	Range     int64
	AngleMDeg int64
}
type LineSelectShape struct {
	Origin    Position
	Direction Direction
	Length    int64
	Width     int64
}
type RectangleSelectShape struct {
	Origin    Position
	Direction Direction
	Length    int64
	Width     int64
}
type RaycastSelectShape struct {
	Origin    Position
	Direction Direction
	Length    int64
}
type ChainSelectShape struct {
	Origin     Position
	HopRange   int64
	MaxTargets int
}
type PathSelectShape struct {
	Points []Position
	Width  int64
}
type NearestValidSelectShape struct {
	Origin       Position
	SearchRadius int64
}

func (SingleSelectShape) isSelectShape()       {}
func (CircleSelectShape) isSelectShape()       {}
func (RingSelectShape) isSelectShape()         {}
func (ConeSelectShape) isSelectShape()         {}
func (LineSelectShape) isSelectShape()         {}
func (RectangleSelectShape) isSelectShape()    {}
func (RaycastSelectShape) isSelectShape()      {}
func (ChainSelectShape) isSelectShape()        {}
func (PathSelectShape) isSelectShape()         {}
func (NearestValidSelectShape) isSelectShape() {}

type SelectFilter interface{ isSelectFilter() }

type AliveSelectFilter struct{}
type NotCasterSelectFilter struct{}
type RelationSelectFilter struct{ Relation string }
type StatusSelectFilter struct {
	Status StatusHandle
	Has    bool
}
type AttributeSelectFilter struct {
	Attribute AttributeHandle
	Operation string
	Value     int64
}
type VisibleSelectFilter struct{}
type TargetableSelectFilter struct{}
type LineOfSightSelectFilter struct{ Layers []CollisionLayerHandle }
type GameplayTagSelectFilter struct {
	Tag GameplayTagHandle
	Has bool
}

func (AliveSelectFilter) isSelectFilter()       {}
func (NotCasterSelectFilter) isSelectFilter()   {}
func (RelationSelectFilter) isSelectFilter()    {}
func (StatusSelectFilter) isSelectFilter()      {}
func (AttributeSelectFilter) isSelectFilter()   {}
func (VisibleSelectFilter) isSelectFilter()     {}
func (TargetableSelectFilter) isSelectFilter()  {}
func (LineOfSightSelectFilter) isSelectFilter() {}
func (GameplayTagSelectFilter) isSelectFilter() {}

type SelectOrderBy string
type SelectDirection string

const (
	SelectOrderEntityID             SelectOrderBy   = "entity_id"
	SelectOrderDistance             SelectOrderBy   = "distance"
	SelectOrderRandom               SelectOrderBy   = "random"
	SelectOrderSpawnTick            SelectOrderBy   = "spawn_tick"
	SelectOrderSpawnSequence        SelectOrderBy   = "spawn_sequence"
	SelectOrderDistanceToOwner      SelectOrderBy   = "distance_to_owner"
	SelectOrderRemainingLifetime    SelectOrderBy   = "remaining_lifetime"
	SelectOrderStatusDispelPriority SelectOrderBy   = "status_dispel_priority"
	SelectOrderRemainingDuration    SelectOrderBy   = "remaining_duration"
	SelectOrderStackCount           SelectOrderBy   = "stack_count"
	SelectOrderAppliedTick          SelectOrderBy   = "applied_tick"
	SelectOrderStatusInstanceID     SelectOrderBy   = "status_instance_id"
	SelectAscending                 SelectDirection = "asc"
	SelectDescending                SelectDirection = "desc"
)

type SelectOrder struct {
	By        SelectOrderBy
	Direction SelectDirection
}
