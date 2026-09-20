package activity

import (
	"context"
	"fmt"
	"strings"
)

// Admin is the operator surface: the one thing a human can do about a result
// delivery the automatic paths have given up on.
//
// # Why this exists
//
// A dispatch whose attempt budget runs out reaches DispatchExhausted, and both
// automatic paths then refuse it — AttemptDispatch hands out no further
// attempt, AckDispatch refuses a late acknowledgement. So a game server never
// receives the aggregated result of an activity its players took part in, and
// before this file no code path could change that. It is the same shape as
// platform's exhausted order: a terminal state with nothing on the other side
// of it.
//
// It is reached the same way, too: a game server that is down for longer than
// DispatchMaxAttempts × the backoff exhausts every dispatch aimed at it.
//
// # No bus transport, and no enumeration
//
// There is no //roost:rpc marker, for the reason platform's Admin has none:
// this operation is more dangerous than anything on the Coordinator interface,
// and the bus carries no caller identity this service can verify. It is
// reachable only in the process that OWNS the activity service.
//
// It also does not enumerate exhausted dispatches. An operator asking "which
// game servers are missing results" already has the two halves of that
// question answerable per key — LookupActivity says the activity finished,
// LookupDispatch says whether a given game acked — and the expected game set
// is on the Activity itself, written down when it was opened. Enumerating would
// need an index outside the compare-and-set that sets the terminal state, so it
// could disagree with the records it indexes.
type Admin interface {
	// ReopenDispatch returns an exhausted dispatch to the retry queue with a
	// fresh attempt budget. Use it after the receiving game server is back.
	ReopenDispatch(ctx context.Context, key Key, gameSID int32, note string) (dispatch Dispatch, err error)
}

// MaxAdminNoteBytes bounds an operator note. It is stored on the dispatch and
// read back by whoever looks at it next.
const MaxAdminNoteBytes = 512

// ReopenDispatch implements Admin.
//
// The ACK TOKEN IS PRESERVED, and that is the load-bearing decision here
// rather than an omission.
//
// A game server can have received a result, applied it, and then failed to
// acknowledge — a lost response, a restart between the two — after which the
// dispatch keeps retrying and eventually exhausts. Reopening then delivers the
// same result again. Whether that double-applies depends entirely on the game
// deduplicating, and the only key it can deduplicate on is the token. Minting
// a fresh token on reopen would hand the same result under a new identity and
// break exactly the callers that did the right thing.
//
// This is the same rule mail's claim token follows: server-generated, constant
// for the life of the thing it identifies, so a retry cannot buy a new
// idempotency key.
//
// Every decision is inside the compare-and-set. Reopening is allowed from
// DispatchExhausted and nothing else:
//
//   - DispatchAcked is refused. The game already confirmed it applied the
//     result; re-dispatching would ask it to apply it again, and an
//     acknowledged dispatch is the one case where we KNOW that would be a
//     second application rather than a retry.
//   - DispatchPending is refused distinctly: it is already retryable, so an
//     operator whose call timed out and retried gets an answer it can act on.
func (s *Service) ReopenDispatch(ctx context.Context, key Key, gameSID int32, note string) (Dispatch, error) {
	if err := key.Validate(); err != nil {
		return Dispatch{}, err
	}
	if gameSID <= 0 {
		return Dispatch{}, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return Dispatch{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	var reopened Dispatch
	_, _, err = s.cfg.Dispatches.Update(ctx, DispatchKey{Activity: key, GameSID: gameSID},
		func(current Dispatch, found bool) (Dispatch, bool, error) {
			if !found {
				return current, false, fmt.Errorf("%w: activity %s game %d",
					ErrDispatchMissing, key, gameSID)
			}
			switch current.State {
			case DispatchExhausted:
				// The only reopenable state.
			case DispatchPending:
				return current, false, fmt.Errorf("%w: activity %s game %d is already retryable "+
					"(attempt %d of %d)", ErrNotResolvable, key, gameSID,
					current.Attempts, current.MaxAttempts)
			default:
				return current, false, fmt.Errorf("%w: activity %s game %d is %s; the game "+
					"already confirmed it applied this result", ErrNotResolvable,
					key, gameSID, current.State)
			}
			next := current
			next.State = DispatchPending
			next.Attempts = 0
			// Due immediately: the operator reopened it because the receiving
			// game is back, and a backoff they did not set is a reason to
			// reach past this API into the store.
			next.NextAttemptAtUnix = 0
			next.ExhaustedAtUnix = 0
			// next.Token is deliberately NOT regenerated. See the doc comment:
			// it is the only key a game server can deduplicate a re-delivered
			// result on.
			next.Reopens++
			next.AdminNote = note
			next.AdminActionAtUnix = nowUnix
			reopened = next
			return next, true, nil
		})
	if err != nil {
		return Dispatch{}, err
	}
	s.report.Accepted("admin.reopen_dispatch")
	return reopened, nil
}

// validateAdminNote requires a reason and bounds it.
//
// Required, with no default: an intervention that records no reason cannot be
// reviewed, and the next person to look at the dispatch sees that someone
// changed it without being able to find out why.
func validateAdminNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an operator note is required; an intervention with no "+
			"recorded reason cannot be reviewed", ErrAdminNoteRequired)
	}
	if len(trimmed) > MaxAdminNoteBytes {
		return "", fmt.Errorf("%w: note is %d bytes, limit %d",
			ErrAdminNoteRequired, len(trimmed), MaxAdminNoteBytes)
	}
	return trimmed, nil
}

var _ Admin = (*Service)(nil)
