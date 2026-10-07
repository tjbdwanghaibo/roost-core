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
//   - StopSpawn must be idempotent: stopping a spawn that is already
//     stopped, or that the Host does not know, succeeds without a second side
//     effect (MemoryHost returns the current revision and emits no event).
//     Every stop entry (cast failure and in-cast stops, failed spawn
//     starts, tick-driven reaping, RemoveProgram, Shutdown) goes through one
//     stop function: when StopSpawn fails, the Runtime marks the spawn
//     stop_pending and retries the same stop on later ticks with backoff
//     (RuntimeOptions.SpawnStopRetryBackoff / SpawnStopRetryLimit), also
//     after Shutdown when the Runtime keeps advancing or is restored from a
//     checkpoint, and Shutdown / RemoveProgram stop it again on request; a
//     stop the Host actually performed but reported as failed is therefore
//     re-issued. Callbacks run only on the first request; retries call
//     StopSpawn alone. Return an error only when the spawn is still running
//     in the world.
//   - HostCapabilities declares what the Host supports (B3 ③). Every Host
//     must declare it: the Runtime admits a Program on its first use only when
//     its compiled host requirements are all in the table, and refuses it
//     otherwise (ErrHostCapabilityMissing) before anything is paid. Wrappers
//     forward the wrapped Host's table (RecordingHost / ReplayHost). A zero
//     table declares nothing, so every Program with requirements is refused.
type Host interface {
	AuthorityProvider
	HostCapabilityProvider
	StateStore
	Advance(tick Tick) (WorldRevision, error)
	CurrentRevision() WorldRevision
	Read(request ReadRequest) (ReadResult, error)
	Select(request SelectRequest) (SelectResult, error)
	PayCosts(payment CostPayment) (CommitReceipt, error)
	Apply(command EffectCommand) (EffectResult, error)
	StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error)
	StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error)
	Events(after EventCursor) []RuntimeEvent
}

// HostEventCompactor is an optional single-consumer optimization. A Host must
// implement it only when the Runtime is the exclusive consumer of Events.
//
// CompactEventsThrough 允许删除这个消费游标之前的事件。恢复时 Host 世界、事件
// 队列和 Runtime checkpoint 必须来自同一份持久边界（revision / authority 相同）；
// Runtime 不能凭较旧 checkpoint 在更新的 Host 上重放并恢复世界。
// 若业务需要跨 checkpoint 重演，必须另存成对的世界快照及输入/事件日志。
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
	SelectOrderSummonTick           SelectOrderBy   = "summon_tick"
	SelectOrderSummonSequence       SelectOrderBy   = "summon_sequence"
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
