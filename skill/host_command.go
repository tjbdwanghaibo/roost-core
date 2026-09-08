package skill

type EffectCommand struct {
	Meta    CommandMeta
	Payload EffectCommandPayload
}

type EffectCommandPayload interface{ isEffectCommandPayload() }

type TeleportCommand struct {
	Target      EntityID
	Destination Position
	OnBlocked   string
}
type KnockbackCommand struct {
	Target   EntityID
	From     Position
	Distance int64
}
type PullCommand struct {
	Target   EntityID
	Toward   Position
	Distance int64
}
type ResourceCommand struct {
	Target    EntityID
	Resource  ResourceHandle
	Operation string
	Amount    int64
}
type SpawnCommand struct {
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
	AttributeOverrides []SpawnAttributeOverride
	ParameterBindings  []SpawnParameterBinding
	Transactional      bool
}

type SpawnAttributeOverride struct {
	Attribute AttributeHandle
	Value     int64
}

type SpawnParameterBinding struct {
	Name  string
	Value RuntimeValue
}

func (TeleportCommand) isEffectCommandPayload()    {}
func (KnockbackCommand) isEffectCommandPayload()   {}
func (PullCommand) isEffectCommandPayload()        {}
func (ResourceCommand) isEffectCommandPayload()    {}
func (SpawnCommand) isEffectCommandPayload()       {}
func (OwnedEntityCommand) isEffectCommandPayload() {}

type EffectResult struct {
	Commit  CommitReceipt
	Value   RuntimeValue
	Payload EffectResultPayload
}

type ExpectedFailureReason string

const (
	ExpectedFailureNone                ExpectedFailureReason = "none"
	ExpectedFailureInvalidTarget       ExpectedFailureReason = "invalid_target"
	ExpectedFailureInvalidPosition     ExpectedFailureReason = "invalid_position"
	ExpectedFailureCapacityReached     ExpectedFailureReason = "capacity_reached"
	ExpectedFailurePolicyRejected      ExpectedFailureReason = "policy_rejected"
	ExpectedFailurePermissionDenied    ExpectedFailureReason = "permission_denied"
	ExpectedFailureReferenceExpired    ExpectedFailureReason = "reference_expired"
	ExpectedFailureDestinationBlocked  ExpectedFailureReason = "destination_blocked"
	ExpectedFailureResourceUnavailable ExpectedFailureReason = "resource_unavailable"
)

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

type TeleportEffectResult struct {
	ResultOutcome
	Position Position
}

func (TeleportEffectResult) isEffectResultPayload() {}

type SpawnEffectResult struct {
	ResultOutcome
	Entities      []EntityID
	FirstEntity   EntityID
	TransactionID OwnedSpawnTransactionID
}

type StateChangeEffectResult struct {
	ResultOutcome
	Before  RuntimeValue
	After   RuntimeValue
	Applied bool
}

type AbilityChangeEffectResult struct {
	ResultOutcome
	Before  RuntimeValue
	After   RuntimeValue
	Applied bool
}

type EntityCommandEffectResult struct {
	ResultOutcome
	Applied bool
}

type SnapshotCaptureEffectResult struct {
	ResultOutcome
	Token SnapshotToken
}

type SnapshotRestoreEffectResult struct {
	ResultOutcome
	Applied                      bool
	AppliedFields, SkippedFields []string
}

func (SpawnEffectResult) isEffectResultPayload()           {}
func (StateChangeEffectResult) isEffectResultPayload()     {}
func (AbilityChangeEffectResult) isEffectResultPayload()   {}
func (EntityCommandEffectResult) isEffectResultPayload()   {}
func (SnapshotCaptureEffectResult) isEffectResultPayload() {}
func (SnapshotRestoreEffectResult) isEffectResultPayload() {}

type EffectResultPayload interface{ isEffectResultPayload() }

type CostEntry struct {
	Resource string
	Handle   ResourceHandle
	Amount   int64
}

type CostPayment struct {
	Meta    CommandMeta
	Entity  EntityID
	Entries []CostEntry
}
