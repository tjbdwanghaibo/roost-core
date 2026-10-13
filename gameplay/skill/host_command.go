package skill

// EffectCommand 是 Runtime 交给 Host.Apply 的一条效果命令；Payload 是各 *Command 类型之一。
type EffectCommand struct {
	Meta    CommandMeta
	Payload EffectCommandPayload
}

// EffectCommandPayload 是效果命令载荷的封闭联合。
type EffectCommandPayload interface{ isEffectCommandPayload() }

// TeleportCommand 把目标传送到目的地；OnBlocked 决定目的地被阻挡时的处理。
type TeleportCommand struct {
	Target      EntityID
	Destination Position
	OnBlocked   string
}

// KnockbackCommand 把目标从 From 推开 Distance。
type KnockbackCommand struct {
	Target   EntityID
	From     Position
	Distance int64
}

// PullCommand 把目标朝 Toward 拉近 Distance。
type PullCommand struct {
	Target   EntityID
	Toward   Position
	Distance int64
}

// ResourceCommand 对目标的资源做 set / add / spend / sub。
type ResourceCommand struct {
	Target    EntityID
	Resource  ResourceHandle
	Operation string
	Amount    int64
}

// SummonCommand 为 Owner 召唤 Count 个单位模板实例；Transactional 时走 Preview / Commit / Rollback 两阶段。
type SummonCommand struct {
	Owner              EntityID
	GameplayDigest     string
	SourceSkillID      string
	SourceCastID       CastID
	SourceEffectIndex  EffectIndex
	Template           UnitTemplateHandle
	Position           Position
	Count              int
	DurationTicks      Tick
	GameplayTags       []GameplayTagHandle
	AttributeOverrides []SummonAttributeOverride
	ParameterBindings  []SummonParameterBinding
	Transactional      bool
}

// SummonAttributeOverride 覆盖召唤物的一个属性值。
type SummonAttributeOverride struct {
	Attribute AttributeHandle
	Value     int64
}

// SummonParameterBinding 是传给召唤物模板的一个具名参数。
type SummonParameterBinding struct {
	Name  string
	Value RuntimeValue
}

func (TeleportCommand) isEffectCommandPayload()    {}
func (KnockbackCommand) isEffectCommandPayload()   {}
func (PullCommand) isEffectCommandPayload()        {}
func (ResourceCommand) isEffectCommandPayload()    {}
func (SummonCommand) isEffectCommandPayload()      {}
func (OwnedEntityCommand) isEffectCommandPayload() {}

// EffectResult 是 Host.Apply 的结果：提交回执、结果值与按命令种类的结果载荷。
type EffectResult struct {
	Commit  CommitReceipt
	Value   RuntimeValue
	Payload EffectResultPayload
}

// ExpectedFailureReason 是效果可预期的失败原因（不是错误），供 effect result 分支判断。
type ExpectedFailureReason string

const (
	ExpectedFailureNone               ExpectedFailureReason = "none"
	ExpectedFailureInvalidTarget      ExpectedFailureReason = "invalid_target"
	ExpectedFailureInvalidPosition    ExpectedFailureReason = "invalid_position"
	ExpectedFailureCapacityReached    ExpectedFailureReason = "capacity_reached"
	ExpectedFailurePolicyRejected     ExpectedFailureReason = "policy_rejected"
	ExpectedFailurePermissionDenied   ExpectedFailureReason = "permission_denied"
	ExpectedFailureReferenceExpired   ExpectedFailureReason = "reference_expired"
	ExpectedFailureDestinationBlocked ExpectedFailureReason = "destination_blocked"
)

// ResultOutcome 是效果结果共有的成功标志与失败原因。
type ResultOutcome struct {
	Succeeded     bool
	FailureReason ExpectedFailureReason
}

func successfulResultOutcome() ResultOutcome {
	return ResultOutcome{Succeeded: true, FailureReason: ExpectedFailureNone}
}

func failedResultOutcome(reason ExpectedFailureReason) ResultOutcome {
	return ResultOutcome{Succeeded: false, FailureReason: reason}
}

// TeleportEffectResult 是传送的结果与最终位置。
type TeleportEffectResult struct {
	ResultOutcome
	Position Position
}

func (TeleportEffectResult) isEffectResultPayload() {}

// SummonEffectResult 是召唤的结果：召唤出的实体与（两阶段时的）事务 ID。
type SummonEffectResult struct {
	ResultOutcome
	Entities      []EntityID
	FirstEntity   EntityID
	TransactionID OwnedSummonTransactionID
}

// StateChangeEffectResult 是持久状态修改的结果与前后值。
type StateChangeEffectResult struct {
	ResultOutcome
	Before  RuntimeValue
	After   RuntimeValue
	Applied bool
}

// AbilityChangeEffectResult 是技能实例状态修改的结果与前后值。
type AbilityChangeEffectResult struct {
	ResultOutcome
	Before  RuntimeValue
	After   RuntimeValue
	Applied bool
}

// EntityCommandEffectResult 是对召唤物实体命令的结果。
type EntityCommandEffectResult struct {
	ResultOutcome
	Applied bool
}

// SnapshotCaptureEffectResult 是快照捕获的结果与签发的 token。
type SnapshotCaptureEffectResult struct {
	ResultOutcome
	Token SnapshotToken
}

// SnapshotRestoreEffectResult 是快照恢复的结果与应用 / 跳过的字段。
type SnapshotRestoreEffectResult struct {
	ResultOutcome
	Applied                      bool
	AppliedFields, SkippedFields []string
}

func (SummonEffectResult) isEffectResultPayload()          {}
func (StateChangeEffectResult) isEffectResultPayload()     {}
func (AbilityChangeEffectResult) isEffectResultPayload()   {}
func (EntityCommandEffectResult) isEffectResultPayload()   {}
func (SnapshotCaptureEffectResult) isEffectResultPayload() {}
func (SnapshotRestoreEffectResult) isEffectResultPayload() {}

// EffectResultPayload 是效果结果载荷的封闭联合。
type EffectResultPayload interface{ isEffectResultPayload() }

// CostEntry 是一项费用：资源与数量。
type CostEntry struct {
	Resource string
	Handle   ResourceHandle
	Amount   int64
}

// CostPayment 是 Host.PayCosts 的请求：对一个实体原子地支付全部费用项。
type CostPayment struct {
	Meta    CommandMeta
	Entity  EntityID
	Entries []CostEntry
}
