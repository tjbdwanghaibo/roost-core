// Package rank is a leaderboard service: submit a score, read a page, read one
// owner's rank, archive a season.
//
// It is a reimplementation rather than a port. The implementation it replaces
// had, among others, these defects — each of which shapes a decision here:
//
//   - A page request with Limit == 0 was translated to ZREVRANGE 0 -1, reading
//     the whole board and fetching one payload per member. It was reachable
//     from a single client packet on a path that never defaulted the field.
//   - Ranks and scores came from two to four unsynchronized reads, so an entry
//     could be shown with a new score at its old rank, and offset paging
//     drifted under concurrent submits.
//   - Accumulating submits used add-mode with the version guard skipped, so a
//     redelivered event added the same delta up to twenty times. The
//     leaderboard was then permanently wrong with no repair path.
//   - Season archiving stopped at the first short page and wrote metadata
//     claiming a complete count. Since the archive is the only record after a
//     reset, that was silent permanent data loss.
//   - The tiebreak field was never populated by any producer, so equal scores
//     were ordered by owner id: a lower id outranked a higher one forever.
//   - The package defined no error codes, so "board id is empty" reached the
//     client as an internal server error.
package rank

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// Error codes. Every business failure has one: a caller must be able to tell
// "you sent something invalid" from "this service is broken", and the
// implementation this replaces returned CodeInternal for both.
const (
	CodeOK int32 = 0

	CodeBoardInvalid   int32 = 540101
	CodeOwnerInvalid   int32 = 540102
	CodeRangeInvalid   int32 = 540103
	CodeScoreInvalid   int32 = 540104
	CodeRequestInvalid int32 = 540105
	CodeNotFound       int32 = 540106
	CodeSeasonInvalid  int32 = 540107
	CodeConflict       int32 = 540108
)

var (
	ErrBoardInvalid   = errcode.Define(CodeBoardInvalid, "rank: board is invalid", "")
	ErrOwnerInvalid   = errcode.Define(CodeOwnerInvalid, "rank: owner id must be non-zero", "")
	ErrRangeInvalid   = errcode.Define(CodeRangeInvalid, "rank: range is invalid", "")
	ErrScoreInvalid   = errcode.Define(CodeScoreInvalid, "rank: score is invalid", "")
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "rank: request id is required", "")
	ErrNotFound       = errcode.Define(CodeNotFound, "rank: not found", "")
	ErrSeasonInvalid  = errcode.Define(CodeSeasonInvalid, "rank: season is invalid", "")
	// ErrConflict reports repeated compare-and-swap loss on one owner. Contention
	// and an unreachable backend need different operational responses, so it
	// carries its own code rather than reading as CodeInternal on the wire.
	ErrConflict = errcode.Define(CodeConflict, "rank: submit conflict", "")
)

// MaxPageSize bounds one page. A caller cannot exceed it, and — the part that
// matters — cannot bypass it by leaving the limit at zero: a non-positive
// limit is ErrRangeInvalid, not "unlimited".
const MaxPageSize = 200

// MaxBriefBytes bounds the opaque payload stored per entry, so one submit
// cannot make a page response arbitrarily large.
const MaxBriefBytes = 512

// maxAppliedRequests is how many recent request ids an entry remembers for
// idempotency. It is a bounded ring inside the record rather than a separate
// growing key, so idempotency cannot become an unbounded keyspace. Eight
// covers realistic redelivery windows; a request older than that re-applies,
// which is why producers should also make their deltas idempotent where the
// value matters.
const maxAppliedRequests = 8

// Scope is how wide a board is. It carries no behaviour — it is part of the
// board identity.
type Scope string

const (
	ScopeServer Scope = "server"
	ScopeGroup  Scope = "group"
	ScopeGlobal Scope = "global"
)

func (s Scope) valid() bool {
	switch s {
	case ScopeServer, ScopeGroup, ScopeGlobal:
		return true
	}
	return false
}

// Board identifies one leaderboard.
type Board struct {
	// ID names what is ranked, e.g. "arena" or "battle_win".
	ID string `json:"id"`
	// Scope and ScopeID say how wide it is. A global board has no ScopeID.
	Scope   Scope `json:"scope"`
	ScopeID int64 `json:"scope_id"`
	// Season partitions a board over time. It is opaque to this service:
	// nothing here advances or resets a season on its own.
	Season int64 `json:"season"`
}

func (b Board) Validate() error {
	if b.ID == "" {
		return fmt.Errorf("%w: id is empty", ErrBoardInvalid)
	}
	if !b.Scope.valid() {
		return fmt.Errorf("%w: unknown scope %q", ErrBoardInvalid, b.Scope)
	}
	if b.Scope == ScopeGlobal && b.ScopeID != 0 {
		return fmt.Errorf("%w: a global board has no scope id", ErrBoardInvalid)
	}
	if b.Scope != ScopeGlobal && b.ScopeID == 0 {
		return fmt.Errorf("%w: scope %q needs a scope id", ErrBoardInvalid, b.Scope)
	}
	if b.Season < 0 {
		return fmt.Errorf("%w: negative season", ErrBoardInvalid)
	}
	return nil
}

// String renders the board identity used as a storage key component.
func (b Board) String() string {
	return fmt.Sprintf("%s:%s:%d:%d", b.ID, b.Scope, b.ScopeID, b.Season)
}

// Score is what one owner has on a board.
type Score struct {
	OwnerID int64 `json:"owner_id"`
	// Value is what is ranked; higher ranks first.
	Value int64 `json:"value"`
	// Tie breaks equal Values, and **smaller ranks higher**. The canonical
	// use is the time the value was reached, so whoever got there first ranks
	// higher. When a submit leaves it zero the store fills it with the submit
	// time, which is why equal scores are no longer ordered by owner id — the
	// defect that made a low id outrank a high one permanently.
	Tie int64 `json:"tie"`
	// Brief is opaque payload the service stores and returns unchanged,
	// bounded by MaxBriefBytes. This service does not resolve display data.
	Brief []byte `json:"brief,omitempty"`
}

// UpdateMode says how a submit combines with the stored value.
type UpdateMode string

const (
	// UpdateSet replaces the value. Safe to replay.
	UpdateSet UpdateMode = "set"
	// UpdateMax keeps the higher value. Safe to replay.
	UpdateMax UpdateMode = "max"
	// UpdateAdd accumulates. **Not** safe to replay, which is why a submit
	// using it requires a RequestID and is deduplicated against the entry's
	// recent request ring.
	UpdateAdd UpdateMode = "add"
)

func (m UpdateMode) valid() bool {
	switch m {
	case UpdateSet, UpdateMax, UpdateAdd:
		return true
	}
	return false
}

// Entry is one ranked row. Rank and Score come from the same read, so they
// cannot disagree.
type Entry struct {
	// Rank is 1-based within the requested page's board.
	Rank  int64 `json:"rank"`
	Score Score `json:"score"`
}

// Page is a bounded window onto a board.
type Page struct {
	Entries []Entry `json:"entries"`
	// Total is the board size at the time of the read.
	Total int64 `json:"total"`
}

// Error maps an error to the code and reason a client sees.
//
// It matches roost-kit's servicerpc.Error convention, which is what an RPC
// envelope is filled from.
//
// It is short because the sentinels carry their own codes: errcode.ClientError
// finds the code through any depth of fmt.Errorf wrapping, so there is no
// per-sentinel table here to keep in step with the one above. A hand-written
// switch over every sentinel is the shape this replaces, and it is a second
// list that a newly added error silently falls off.
//
// Two behaviours are relied on rather than incidental:
//
//   - When an error wraps two coded errors with "%w: %w", the FIRST one wins.
//     That is what makes a refusal which wraps a caller's own reason report
//     the refusal, which is what the client has to be told.
//   - An error this package cannot classify reports errcode.CodeInternal, not
//     a code of its own. Answering "the store failed" for an unclassified bug
//     is a guess presented as a diagnosis — and a catch-all code of that shape
//     is what the previous constant block had, with nothing able to produce it
//     deliberately.
func Error(err error) (int32, string) {
	if err == nil {
		return CodeOK, ""
	}
	return errcode.ClientError(err)
}

// Code is Error without the reason, for callers that only switch on the code.
func Code(err error) int32 {
	code, _ := Error(err)
	return code
}
