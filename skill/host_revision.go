package skill

import "errors"

type WorldRevision uint64
type EventCursor uint64
type ProcessID uint64

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

type QueryMeta struct {
	RequiredRevision WorldRevision
}

type QueryResultMeta struct {
	Revision WorldRevision
}

type CommandMeta struct {
	RequiredRevision WorldRevision
	EffectIndex      EffectIndex
}

type CommitReceipt struct {
	Revision WorldRevision
	Changed  bool
}

type RuntimeEvent struct {
	Cursor    EventCursor
	Revision  WorldRevision
	Tick      Tick
	Kind      string
	Entity    EntityID
	ProcessID ProcessID
	Context   EventContext
	State     *StateChangeEvent
	Ability   *AbilityChangeEvent
	Result    *EffectResultEvent
}
