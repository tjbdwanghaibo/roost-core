package rank

import "context"

// The transport for Rank is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .

// Rank is the cross-process contract: what ANOTHER process may ask of the
// rank service.
//
// It is deliberately smaller than Store. Reset is not here, and that omission
// is the only judgement in this file worth arguing about: it empties a whole
// board. Exposing it on the bus would let any service process wipe a
// leaderboard, and the callers that legitimately need it — a season boundary,
// an operator — want a trace id and an audit trail, which is admin's job. A
// destructive operation reachable from every peer is a destructive operation
// that will eventually be reached by accident.
//
// Everything else is here because a game process genuinely calls it: submit a
// score, read a page, find one player's rank.
//
//roost:rpc service_type=rank capability=service.rank
type Rank interface {
	// Submit applies one score under mode and returns the entry as stored.
	// requestID makes the call idempotent, which is what makes UpdateAdd safe
	// under at-least-once delivery — the confirmed defect it answers added
	// the same battle's delta 2 to 20 times.
	Submit(ctx context.Context, board Board, score Score, mode UpdateMode, requestID string) (entry Entry, err error)

	// Remove deletes an owner's entry. Idempotent.
	Remove(ctx context.Context, board Board, ownerID int64) (err error)

	// Page reads a bounded window. limit must be in [1, MaxPageSize] — a
	// non-positive limit is an error, never "unlimited".
	Page(ctx context.Context, board Board, offset int, limit int) (page Page, err error)

	// Rank reads one owner's entry.
	Rank(ctx context.Context, board Board, ownerID int64) (entry Entry, found bool, err error)

	// Around reads the window centred on one owner.
	Around(ctx context.Context, board Board, ownerID int64, radius int) (page Page, err error)

	// Size reports how many entries a board holds.
	Size(ctx context.Context, board Board) (size int64, err error)
}

var _ Rank = (*RedisStore)(nil)
