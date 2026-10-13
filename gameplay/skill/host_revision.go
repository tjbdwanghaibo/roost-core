package skill

import "errors"

// WorldRevision 是世界状态的修订号，EventCursor 是 Host 事件流的游标，SpawnID 标识一个衍生物。
type WorldRevision uint64
type EventCursor uint64
type SpawnID uint64

var (
	ErrRevisionUnavailable       = errors.New("skill: required world revision is unavailable")
	ErrEntityNotFound            = errors.New("skill: entity not found")
	ErrInsufficientResource      = errors.New("skill: insufficient resource")
	ErrRuntimeValueMissing       = errors.New("skill: runtime value is missing")
	ErrRuntimeTypeMismatch       = errors.New("skill: runtime value type mismatch")
	ErrRuntimeQuantityMismatch   = errors.New("skill: runtime quantity mismatch")
	ErrRuntimeArithmeticOverflow = errors.New("skill: runtime arithmetic overflow")
	ErrCombatHandleInvalid       = errors.New("skill: combat handle is not authorized")
	ErrCombatPolicyUnsupported   = errors.New("skill: combat policy is unsupported")
	ErrHostContractViolation     = errors.New("skill: host contract violation")
)

// QueryMeta 是查询要求的最低世界 revision。
type QueryMeta struct {
	RequiredRevision WorldRevision
}

// QueryResultMeta 是查询结果所在的世界 revision。
type QueryResultMeta struct {
	Revision WorldRevision
}

// CommandMeta 是命令要求的世界 revision 与效果下标。
type CommandMeta struct {
	RequiredRevision WorldRevision
	EffectIndex      EffectIndex
}

// CommitReceipt 是一次 Host 修改的回执：提交后的 revision 与是否有变化。
type CommitReceipt struct {
	Revision WorldRevision
	Changed  bool
}

// RuntimeEvent 是 Host 事件流或 Runtime 诊断里的一条事件。
type RuntimeEvent struct {
	Cursor   EventCursor
	Revision WorldRevision
	Tick     Tick
	Kind     string
	Entity   EntityID
	SpawnID  SpawnID
	Context  EventContext
	State    *StateChangeEvent
	Ability  *AbilityChangeEvent
	Result   *EffectResultEvent
}
