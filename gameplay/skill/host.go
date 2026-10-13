package skill

// Host 是 Runtime 驱动的世界权威接口。实现必须遵守以下并发与确定性契约：
//
//   - 每个 Host 方法都在 Runtime 持有内部锁时调用：调用严格串行，实现不需要为并发的 Runtime 回调自行同步。
//   - Host 方法不得重入 Runtime（Start、Advance、Input、Cancel、StateDeltas 等）。Runtime 的锁不可重入，
//     重入即死锁。世界对技能效果的反应放进 Events，由 Runtime 在确定的时点轮询。
//   - Host 方法不得阻塞在通道、被其它调用 Runtime 的 goroutine 持有的锁，或延迟无界的 I/O 上。一次阻塞的
//     Host 调用会卡住这个 Runtime 上的所有施法者。
//   - 结果必须是请求 revision 下世界状态的确定函数。墙钟时间、map 迭代顺序与 goroutine 时序都不得影响结果；
//     回放与 checkpoint 恢复会重发同样的调用，必须得到同样的回答。
//   - StopSpawn 必须幂等：停止一个已停止或 Host 不认识的衍生物直接成功，不产生第二次副作用（MemoryHost
//     返回当前 revision、不发事件）。所有停止入口（施法失败与施法内停止、衍生物启动失败、tick 驱动回收、
//     RemoveProgram、Shutdown）都经同一个停止函数：StopSpawn 失败时 Runtime 把衍生物标成 stop_pending，
//     之后按退避在后续 tick 重试同一次停止（RuntimeOptions.SpawnStopRetryBackoff / SpawnStopRetryLimit），
//     Shutdown 之后继续推进或从 checkpoint 恢复时同样重试，Shutdown / RemoveProgram 被再次调用时也会再停一次；
//     所以 Host 实际停了却报告失败的停止会被重发。回调只在第一次请求时运行；重试先补做尚未成功的 carry 解除，
//     再调 StopSpawn。只有衍生物仍在世界里运行时才返回错误。
//   - HostCapabilities 声明 Host 支持的能力，每个 Host 都必须声明：Program 第一次使用时，只有编译出的 Host
//     需求全部在表里才被准入，否则在扣任何费用之前以 ErrHostCapabilityMissing 拒绝。包装型 Host
//     （RecordingHost / ReplayHost）转发被包装 Host 的表。零值表什么也不声明，任何有需求的 Program 都会被拒绝。
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

// HostEventCompactor 是可选的单消费者优化：只有 Runtime 是 Events 唯一的消费者时，Host 才能实现它。
//
// CompactEventsThrough 允许删除这个消费游标之前的事件。恢复时 Host 世界、事件
// 队列和 Runtime checkpoint 必须来自同一份持久边界（revision / authority 相同）；
// Runtime 不能凭较旧 checkpoint 在更新的 Host 上重放并恢复世界。
// 若业务需要跨 checkpoint 重演，必须另存成对的世界快照及输入/事件日志。
type HostEventCompactor interface {
	CompactEventsThrough(EventCursor)
}

// InputPositionResolver 提供权威的阻挡位置事实；确定性的距离与路径计算仍留在 Runtime 里。
type InputPositionResolver interface {
	ResolveInputPosition(InputPositionRequest) (Position, bool)
}

// InputPositionRequest 是一次输入位置解析请求。
type InputPositionRequest struct {
	Caster   EntityID
	Position Position
	Policy   string
}

// AbilityRelationProvider 提供施放者与技能实例拥有者之间的权威关系，不把技能实例状态暴露给世界查询接口。
type AbilityRelationProvider interface {
	AbilityOwnerRelation(viewer, owner EntityID) (string, bool)
}

// ReadRequest 是一次 Host 取值请求；Payload 是 ResourceRead / PositionRead / AttributeRead 之一。
type ReadRequest struct {
	Meta    QueryMeta
	Payload ReadPayload
}

// ReadPayload 是取值请求载荷的封闭联合。
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

// ReadResult 是取值结果。
type ReadResult struct {
	Meta  QueryResultMeta
	Value RuntimeValue
}

// SelectRequest 是一次目标选择请求：形状、过滤、排序与上限。
type SelectRequest struct {
	Meta        QueryMeta
	Caster      EntityID
	ElementKind string
	Shape       SelectShape
	Filters     []SelectFilter
	Order       SelectOrder
	Limit       int
}

// SelectResult 是目标选择结果。
type SelectResult struct {
	Meta      QueryResultMeta
	Selection Selection
}

// SelectShape 是选择形状的封闭联合，具体形状见下面的 *SelectShape 类型。
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

// SelectFilter 是选择过滤条件的封闭联合，具体条件见下面的 *SelectFilter 类型。
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

// SelectOrderBy 是选择结果的排序键，SelectDirection 是排序方向。
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

// SelectOrder 是选择结果的排序方式。
type SelectOrder struct {
	By        SelectOrderBy
	Direction SelectDirection
}
