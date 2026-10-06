package activity

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// Admin is the operator surface: what a human can do about state the
// automatic paths have given up on — a result delivery whose attempts are
// exhausted, a participant's stuck progress proofs, and (B9) window entries
// written by something other than this package, which the sweep only skips.
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

	// ReconcileProgress completes the ledger confirmations a participant's
	// pending progress proofs are waiting on and releases the proofs, so a
	// participant refused with ErrProgressBacklog after a ledger outage can
	// score again before the entries expire. Use it once the ledger is
	// writable again.
	ReconcileProgress(ctx context.Context, key Key, participantID string, note string) (participant Participant, err error)

	// MalformedWindowEntries lists the entries of groupID's stored window
	// the sweep skips: a key that is invalid or belongs to another group, in
	// any of the three lists, and an Opening entry whose creation plan cannot
	// be executed. Nothing in this package writes such an entry and nothing
	// removes one automatically (B9, NC-42, NC-51): this is how an operator
	// finds them, the sweep's counters say only that they exist.
	MalformedWindowEntries(ctx context.Context, groupID string) (entries []MalformedWindowEntry, err error)

	// RemoveMalformedWindowEntry removes one malformed entry — every copy of
	// key in that list of groupID's window — and records the note on the
	// window. It refuses a healthy entry with ErrStatus: that would release a
	// pending slot or stop a delivery from being swept, which is not a
	// repair. It returns how many copies were removed.
	//
	// Removing an Opening entry gives up its slot. If the activity it planned
	// may still be created by a slow opener, check LookupActivity first: an
	// activity whose window entry is gone is never swept.
	RemoveMalformedWindowEntry(ctx context.Context, groupID string, list WindowList, key Key, note string) (removed int, err error)
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
			// reach past this API into the store. Written as "now" rather
			// than 0 so the owed index scores it by this moment, not by the
			// dispatch's CreatedAtUnix.
			next.NextAttemptAtUnix = nowUnix
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

// ReconcileProgress implements Admin.
//
// A participant's pending set holds request ids whose participant write
// LANDED — an id joins it inside the very compare-and-set that moved the
// score — and whose ledger mark did not. So the confirmation each one lacks is
// a fact this service already knows, and writing the mark is finishing the
// two-step ApplyProgress could not finish, not asserting anything about the
// client. That is what makes this safe to run at any time: once the mark is
// written a replay of that id is answered by the ledger before it reaches the
// participant record, exactly as if the original call had completed.
//
// Why an operator entry at all, when ApplyProgress reclaims a proof by itself
// once the ledger's TTL reaps the entry (RR-20261001-05): the TTL is the
// client retry horizon, thirty minutes by default, and a ledger outage that
// strands a busy participant with MaxProgressWindow lost marks would refuse it
// for that long. Writing the marks ends the refusal now.
//
// Marks are written one by one before the participant is touched, and a mark
// that fails stops the call with nothing released: the marks already written
// are idempotent, so a retry resumes where this one stopped. An entry the
// ledger has already reaped needs no mark — its proof is past the horizon —
// and markReservationApplied leaves it gone rather than resurrecting it.
//
// Like ReopenDispatch this is owner-only (no bus transport) and records the
// operator's note on the record it changed.
func (s *Service) ReconcileProgress(ctx context.Context, key Key, participantID string, note string) (Participant, error) {
	participantKey := ParticipantKey{Activity: key, ParticipantID: participantID}
	if err := participantKey.Validate(); err != nil {
		return Participant{}, err
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return Participant{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	observed, found, err := s.lookupParticipant(ctx, participantKey)
	if err != nil {
		return Participant{}, err
	}
	if !found {
		return Participant{}, fmt.Errorf("%w: participant %q of activity %s", ErrMissing, participantID, key)
	}
	pending := observed.PendingRequestIDs
	if observed.ProgressProofVersion == 0 {
		// A record written before the pending set existed: its recent ring
		// is the conservative pending set ApplyProgress would derive.
		pending = observed.AppliedRequestIDs
	}
	for _, id := range pending {
		if err := s.markReservationApplied(ctx, RequestKey{Activity: key, ParticipantID: participantID, RequestID: id}, nowUnix); err != nil {
			return Participant{}, fmt.Errorf("activity: reconcile progress of %q: mark request %q: %w", participantID, id, err)
		}
	}
	var result Participant
	_, _, err = s.cfg.Participants.Update(ctx, participantKey, func(current Participant, found bool) (Participant, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: participant %q of activity %s", ErrMissing, participantID, key)
		}
		next := current.clone()
		if next.ProgressProofVersion == 0 {
			next.PendingRequestIDs = cloneStrings(next.AppliedRequestIDs)
			next.ProgressProofVersion = 1
		}
		// Only the ids whose marks this call wrote are released; one that
		// joined the pending set since the read above keeps its proof.
		next.PendingRequestIDs = slices.DeleteFunc(next.PendingRequestIDs, func(id string) bool { return slices.Contains(pending, id) })
		next.AdminNote = note
		next.AdminActionAtUnix = nowUnix
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		return Participant{}, err
	}
	s.report.Accepted("admin.reconcile_progress")
	return result, nil
}

// MalformedWindowEntries implements Admin.
func (s *Service) MalformedWindowEntries(ctx context.Context, groupID string) ([]MalformedWindowEntry, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	window, found, err := s.cfg.Windows.Get(ctx, groupID)
	if err != nil || !found {
		return nil, err
	}
	return storedEntryProblems(groupID, window.Value), nil
}

// storedEntryProblems is readWindowEntries' malformed list plus the usable
// Opening entries whose plan cannot be executed (openingIntentProblem): the
// reader leaves a plan alone because an entry whose activity exists is
// confirmed whatever its plan says, but for an operator it is still corrupt.
// A legacy entry with no plan is not listed: the sweep reclaims it.
func storedEntryProblems(groupID string, stored Window) []MalformedWindowEntry {
	entries := readWindowEntries(groupID, stored)
	out := entries.malformed
	for _, entry := range entries.usable.Opening {
		if entry.Intent == nil {
			continue
		}
		if reason := openingIntentProblem(entry, groupID); reason != "" {
			out = append(out, MalformedWindowEntry{List: WindowOpening, Key: entry.Key, Reason: reason})
		}
	}
	return out
}

// RemoveMalformedWindowEntry implements Admin.
//
// Like ReopenDispatch, every decision is inside the compare-and-set: whether
// the entry is still there and still malformed is judged on the value being
// replaced, not on an earlier read, so an entry repaired or removed by
// someone else in between is answered ErrMissing or ErrStatus, never removed
// twice or removed healthy.
func (s *Service) RemoveMalformedWindowEntry(ctx context.Context, groupID string, list WindowList, key Key, note string) (int, error) {
	if strings.TrimSpace(groupID) == "" {
		return 0, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	if !list.valid() {
		return 0, fmt.Errorf("%w: window list %q is not one of keys, opening, delivering", ErrInvalid, list)
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return 0, err
	}
	nowUnix := s.cfg.Now().Unix()
	var removed int
	_, _, err = s.cfg.Windows.Update(ctx, groupID, func(current Window, found bool) (Window, bool, error) {
		removed = 0
		if !found {
			return current, false, fmt.Errorf("%w: group %q has no window", ErrMissing, groupID)
		}
		malformed := false
		for _, problem := range storedEntryProblems(groupID, current) {
			if problem.List == list && problem.Key == key {
				malformed = true
				break
			}
		}
		next := current.clone()
		switch list {
		case WindowKeys:
			next.Keys = slices.DeleteFunc(next.Keys, func(k Key) bool { return k == key })
			removed = len(current.Keys) - len(next.Keys)
		case WindowOpening:
			next.Opening = slices.DeleteFunc(next.Opening, func(e OpeningEntry) bool { return e.Key == key })
			removed = len(current.Opening) - len(next.Opening)
		case WindowDelivering:
			next.Delivering = slices.DeleteFunc(next.Delivering, func(k Key) bool { return k == key })
			removed = len(current.Delivering) - len(next.Delivering)
		}
		if removed == 0 {
			return current, false, fmt.Errorf("%w: %s is not in the %s list of group %q", ErrMissing, key, list, groupID)
		}
		if !malformed {
			removed = 0
			return current, false, fmt.Errorf("%w: %s in the %s list of group %q is a healthy entry; removing it is "+
				"not a repair", ErrStatus, key, list, groupID)
		}
		next.AdminNote = note
		next.AdminActionAtUnix = nowUnix
		return next, true, nil
	})
	if err != nil {
		return 0, err
	}
	s.report.Accepted("admin.remove_malformed_window_entry")
	slog.Warn("activity: operator removed a malformed window entry", "group_id", groupID, "list", list,
		"activity_id", key.ActivityID, "phase", key.Phase, "key_group_id", key.GroupID, "removed", removed, "note", note)
	return removed, nil
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
