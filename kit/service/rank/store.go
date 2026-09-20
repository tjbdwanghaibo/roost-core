package rank

import (
	"context"
	"fmt"
)

// Store is a leaderboard's persistence contract.
//
// Note what is absent: there is no unconditional write of a whole board, and
// no way to touch the ordering structure independently of the entry it orders.
// Both existed in the implementation this replaces — the second one is what
// produced ordering members with no matching payload, which silently truncated
// season archives.
type Store interface {
	// Submit applies one score under mode and returns the entry as stored,
	// with its rank. requestID makes the call idempotent: replaying it
	// returns the stored state without applying the change again, which is
	// what makes UpdateAdd safe under at-least-once delivery.
	Submit(ctx context.Context, board Board, score Score, mode UpdateMode, requestID string) (Entry, error)

	// Remove deletes an owner's entry. It is idempotent.
	Remove(ctx context.Context, board Board, ownerID int64) error

	// Page reads a bounded window. offset is zero-based; limit must be in
	// [1, MaxPageSize] — a non-positive limit is an error, never "unlimited".
	// Ranks and scores come from one read, so they cannot disagree.
	Page(ctx context.Context, board Board, offset, limit int) (Page, error)

	// Rank reads one owner's entry.
	Rank(ctx context.Context, board Board, ownerID int64) (Entry, bool, error)

	// Around reads a window centred on one owner.
	Around(ctx context.Context, board Board, ownerID int64, radius int) (Page, error)

	// Reset empties a board. It is deliberately separate from archiving: this
	// service will not let a reset be the thing that also loses the data.
	Reset(ctx context.Context, board Board) error

	// Size reports how many entries a board holds.
	Size(ctx context.Context, board Board) (int64, error)
}

func validateSubmit(board Board, score Score, mode UpdateMode, requestID string) error {
	if err := board.Validate(); err != nil {
		return err
	}
	if score.OwnerID == 0 {
		return ErrOwnerInvalid
	}
	if !mode.valid() {
		return fmt.Errorf("%w: unknown mode %q", ErrScoreInvalid, mode)
	}
	if len(score.Brief) > MaxBriefBytes {
		return fmt.Errorf("%w: brief is %d bytes, limit %d", ErrScoreInvalid, len(score.Brief), MaxBriefBytes)
	}
	// An accumulating submit is not replay-safe on its own, so it must carry
	// something to deduplicate on. Requiring it here — rather than trusting
	// producers — is what stops a redelivered event from adding twice.
	if mode == UpdateAdd && requestID == "" {
		return fmt.Errorf("%w: mode %q requires a request id", ErrRequestInvalid, mode)
	}
	return nil
}

// validateRange enforces the page bound. A non-positive limit is invalid
// rather than unlimited: the implementation this replaces translated a zero
// limit into a full-board scan reachable from one client packet.
func validateRange(offset, limit int) error {
	if offset < 0 {
		return fmt.Errorf("%w: negative offset", ErrRangeInvalid)
	}
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrRangeInvalid, limit)
	}
	if limit > MaxPageSize {
		return fmt.Errorf("%w: limit %d exceeds %d", ErrRangeInvalid, limit, MaxPageSize)
	}
	return nil
}
