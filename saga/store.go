package saga

import (
	"context"
	"time"
)

type ClaimRequest struct {
	Owner         string
	Now           time.Time
	LeaseDuration time.Duration
	Limit         int
}

type Query struct {
	Type              string
	DefinitionVersion uint32
	Statuses          []Status
	UpdatedBefore     time.Time
	Limit             int
}

// ApplyRequest 的 CloseOperation 写 tombstone 时，Receipt 非空表示操作“带结果关闭”（协调器接收了它的
// completion），为空表示“放弃关闭”（超时用尽、saga 截止、定义缺失）。实现 CompletionHistoryStore 的
// Store 要把这个区别记进 tombstone（U-0280）。
type ApplyRequest struct {
	ExpectedVersion uint64
	ExpectedLease   Lease
	After           Record
	Outbox          *OutboxRecord
	Receipt         *Completion
	// CloseOperation atomically records that an idempotent business operation
	// can no longer advance this Saga and removes its still-pending outbox.
	CloseOperation string
}

type ApplyOutcome uint8

const (
	ApplyApplied ApplyOutcome = iota + 1
	ApplyDuplicate
)

// Store must atomically persist ApplyRequest.After, Outbox and Receipt. When
// Outbox is present it supersedes every older queued command with the same
// IdempotencyKey, so timed-out attempts cannot build an unbounded retry fanout.
// ExpectedVersion and ExpectedLease are fencing conditions; an implementation
// must return ErrConflict rather than accepting a stale coordinator.
// StartDigest 是启动身份：实现必须在全部记录读写中原样保存，不能由当前 Data 重新计算。
type Store interface {
	Create(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	GetByBusinessKey(context.Context, string, string) (Record, error)
	List(context.Context, Query) ([]Record, error)
	CompletionRecorded(context.Context, Completion) (bool, error)
	ClaimDue(context.Context, ClaimRequest) ([]Record, error)
	Apply(context.Context, ApplyRequest) (ApplyOutcome, error)
	// ClaimOutbox 必须原子检查到期时间与租约；候选扫描不能绕过并发 Nack 的退避。
	ClaimOutbox(context.Context, ClaimRequest) ([]OutboxRecord, error)
	AckOutbox(context.Context, string, Lease) error
	NackOutbox(context.Context, string, Lease, time.Time, string) error
}

// OperationClosure 是一个已关闭 operation 的关闭方式（U-0280）。
type OperationClosure uint8

const (
	// OperationClosureUnknown：没有 tombstone，或 tombstone 早于 U-0280、没有记录关闭方式。
	OperationClosureUnknown OperationClosure = iota
	// OperationClosedWithResult：协调器接收了这个 operation 的某次 completion 后关闭它。
	OperationClosedWithResult
	// OperationAbandoned：协调器没有拿到结果就关闭了它（重试用尽、saga 截止、定义缺失）。
	OperationAbandoned
)

// CompletionHistory 是协调器对一份“不在等待中”的 completion 的了解。
type CompletionHistory struct {
	// Recorded 与 Store.CompletionRecorded 相同：这份 completion 已被接收，或它的 operation 已关闭。
	Recorded bool
	// Receipt 表示这个 CommandID 的 completion 本身已被接收（不只是 operation 已关闭）。
	Receipt bool
	// Closure 是 operation tombstone 记录的关闭方式。
	Closure OperationClosure
}

// CompletionHistoryStore 是 Store 的可选扩展。实现它的 Store 让协调器区分“重复的结果”和
// “放弃之后才到的成功”：后者说明那一步已经生效、却没有被纳入补偿，协调器记 ERROR 与计数
// （saga.completion.late_after_abandon_total），但不重开终态。没实现的 Store 一律按重复处理。
type CompletionHistoryStore interface {
	CompletionHistory(context.Context, Completion) (CompletionHistory, error)
}

type Publisher interface {
	PublishSagaCommand(context.Context, Command) error
}

type PublishFunc func(context.Context, Command) error

func (fn PublishFunc) PublishSagaCommand(ctx context.Context, command Command) error {
	return fn(ctx, command)
}
