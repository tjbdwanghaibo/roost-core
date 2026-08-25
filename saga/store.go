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
type Store interface {
	Create(context.Context, Record) error
	Get(context.Context, string) (Record, error)
	GetByBusinessKey(context.Context, string, string) (Record, error)
	List(context.Context, Query) ([]Record, error)
	CompletionRecorded(context.Context, Completion) (bool, error)
	ClaimDue(context.Context, ClaimRequest) ([]Record, error)
	Apply(context.Context, ApplyRequest) (ApplyOutcome, error)
	ClaimOutbox(context.Context, ClaimRequest) ([]OutboxRecord, error)
	AckOutbox(context.Context, string, Lease) error
	NackOutbox(context.Context, string, Lease, time.Time, string) error
}

type Publisher interface {
	PublishSagaCommand(context.Context, Command) error
}

type PublishFunc func(context.Context, Command) error

func (fn PublishFunc) PublishSagaCommand(ctx context.Context, command Command) error {
	return fn(ctx, command)
}
