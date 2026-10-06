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

// ApplyRequest 的 CloseOperation 写 tombstone 时，Receipt 是成功表示操作“带结果关闭”（协调器接收了它的
// 成功 completion，计入 CompletedSteps），其余都是“放弃关闭”：没有 Receipt（超时用尽、saga 截止、人工
// Compensate、定义缺失），或以失败关闭（可重试失败用尽、拒绝）——这些关闭都没有记下任何生效的结果。
// 实现 CompletionHistoryStore 的 Store 要把这个区别记进 tombstone（U-0280）。
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
	// OperationClosedWithResult：协调器接收了这个 operation 的某次成功 completion 后关闭它。
	OperationClosedWithResult
	// OperationAbandoned：协调器没有接收成功就关闭了它（超时或可重试失败用尽、拒绝、saga 截止、
	// 人工 Compensate、定义缺失）；之后到达的成功说明那一步已生效却未被计入。
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
// “放弃之后才到的成功”：后者说明那一步已经生效、却没有被纳入补偿。正向的协调器把 saga 带回补偿、只补偿那一步
// （saga 方向 ④，终态会被重开），补偿方向的记 ERROR 与计数（saga.completion.late_after_abandon_total）。
// 没实现的 Store 一律按重复处理，迟到生效的步骤不会被补偿。
type CompletionHistoryStore interface {
	CompletionHistory(context.Context, Completion) (CompletionHistory, error)
}

// LateSuccessAlarmStore 是 Store 的可选扩展（B1）：在 operation tombstone 上记下“这个操作在第 incarnation 代
// 放弃之后迟到的成功已经告警”，first=true 表示这是第一次（原子判定，并发调用只有一个得到 true）。
// 同一个已生效的成功会多次送达（effect 重投、过期投递的回放、JetStream 重投），协调器按（操作，代际）只告警一次。
// 没有 tombstone 时返回 false。没实现它的 Store 每次送达都告警。
type LateSuccessAlarmStore interface {
	MarkLateSuccessAlarm(ctx context.Context, completion Completion, incarnation uint32) (first bool, err error)
}

type Publisher interface {
	PublishSagaCommand(context.Context, Command) error
}

type PublishFunc func(context.Context, Command) error

func (fn PublishFunc) PublishSagaCommand(ctx context.Context, command Command) error {
	return fn(ctx, command)
}
