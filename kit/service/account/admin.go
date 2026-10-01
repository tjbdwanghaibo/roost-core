package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// Admin is the operator surface: the one thing a human can do about a role
// creation plan the automatic paths cannot finish.
//
// # Why this exists
//
// The slot is the durable creation plan (RR-20260929-19). Once a plan is
// admitted — its name reserved, the reservation written on the slot — a
// storage error after that point is recovered forward by the next same-name
// CreateRole, never by deleting a role whose Create may have committed. That
// is right, and it has one dead end: the name reservation lapses after
// ClaimTTL, and if ANOTHER account takes the name in the meantime the plan can
// never commit it. A same-name retry then answers ErrNameTaken and a
// different name answers ErrRoleLimit ("another name is pending"), so the
// account can never create a role on that server again, and before this file
// no code path could change that except editing Redis by hand.
//
// createRole releases the slot by itself when the name is COMMITTED elsewhere,
// because that fact is final (RR-20261001-06). This entry covers what the
// automatic path cannot prove: the name is only reserved by someone else and
// may yet lapse, or the name is free and the player simply wants a different
// one — a plan whose name NameRules no longer accepts, for instance, is
// refused before it can ever resume.
//
// # No bus transport, and no enumeration
//
// There is no //roost:rpc marker, for the reason platform's Admin has none:
// releasing a slot lets a second role be created on a server where the rule is
// one, and the bus carries no caller identity this service can verify. It is
// reachable only in the process that OWNS the account service.
//
// It does not enumerate stuck slots. The operator arrives with an account id
// and a server — the player's complaint names both — and Config.Slots keyed by
// slotKeyFor answers the rest.
type Admin interface {
	// ResolvePendingCreation abandons the pending role plan an account holds
	// on one server and frees the slot, so the account can create a role
	// there again. It returns the abandoned plan: its PlayerID names the
	// unpublished role record the plan may have left behind, which is kept
	// for audit and is never playable.
	//
	// It refuses, with ErrNotResolvable, what it cannot prove safe: a slot
	// with no pending plan, a plan that already published its role, a legacy
	// slot with no creation identity (a published legacy role may hang off
	// it), and a plan that still holds its own name — reserved, because an
	// attempt may be in flight and the reservation lapses on its own; or
	// committed, because that plan completes by an ordinary same-name
	// CreateRole and abandoning it would have to burn or release a committed
	// name.
	ResolvePendingCreation(ctx context.Context, accountID string, serverID int32, note string) (plan RoleCreation, err error)
}

// MaxAdminNoteBytes bounds an operator note. It is stored on the account and
// read back by whoever looks at it next.
const MaxAdminNoteBytes = 512

// ResolvePendingCreation implements Admin.
//
// Order of operations, and why:
//
//  1. Read the slot and the name entry and decide. Nothing is written for a
//     refusal.
//  2. Write the note on the ACCOUNT. The slot is about to be deleted, so the
//     note cannot live there; the account is the record that remains. It is
//     written before the release so that a release fenced out by a concurrent
//     change (step 3) still leaves the attempt visible — "note without
//     effect" is reviewable, "effect without note" is not.
//  3. Delete the slot with DeleteIf: the version read in step 1 plus the plan
//     identity and PlayerID == 0. Any concurrent CreateRole that admits a new
//     reservation or publishes the role bumps the version, so the release
//     cannot land on a slot that moved after the decision; it reports
//     ErrConflict and the operator looks again. An in-flight attempt that
//     admitted BEFORE the read holds a live reservation, which step 1 refuses.
//
// The role record the plan may have created is not deleted. It is never
// playable (roleReady needs the slot to name it) and never resumable (a fresh
// plan mints a new creation id), and deleting a record that might be a
// player's is the kind of automatic cleanup this package does not do.
func (s *Service) ResolvePendingCreation(ctx context.Context, accountID string, serverID int32, note string) (RoleCreation, error) {
	if strings.TrimSpace(accountID) == "" {
		return RoleCreation{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return RoleCreation{}, err
	}
	key := slotKeyFor(accountID, serverID)
	slot, found, err := s.cfg.Slots.Get(ctx, key)
	if err != nil {
		return RoleCreation{}, err
	}
	if !found {
		return RoleCreation{}, fmt.Errorf("%w: %s holds no role slot on server %d", ErrNotResolvable, accountID, serverID)
	}
	if slot.Value.AccountID != accountID || slot.Value.ServerID != serverID {
		return RoleCreation{}, fmt.Errorf("%w: slot identity mismatch", ErrConflict)
	}
	if slot.Value.PlayerID != 0 {
		return RoleCreation{}, fmt.Errorf("%w: %s already published role %d on server %d; nothing is pending",
			ErrNotResolvable, accountID, slot.Value.PlayerID, serverID)
	}
	plan := slot.Value.Creation
	if plan.ID == "" || plan.PlayerID == 0 {
		return RoleCreation{}, fmt.Errorf("%w: the slot of %s on server %d predates creation identities; a "+
			"published legacy role may depend on it, reconcile it offline (RR-20260929-19)", ErrNotResolvable, accountID, serverID)
	}
	owner := directory.Owner(key + "/" + plan.ID)
	entry, held, err := s.cfg.Names.Lookup(ctx, plan.Name)
	if err != nil {
		return RoleCreation{}, err
	}
	if held && entry.Owner == owner {
		switch entry.State {
		case directory.StateCommitted:
			return RoleCreation{}, fmt.Errorf("%w: plan %s of %s on server %d already committed name %q; it "+
				"completes by CreateRole with the same name", ErrNotResolvable, plan.ID, accountID, serverID, plan.Name)
		default:
			return RoleCreation{}, fmt.Errorf("%w: plan %s of %s on server %d holds a live reservation of %q "+
				"until %s; an attempt may be in flight, retry after it lapses", ErrNotResolvable, plan.ID, accountID,
				serverID, plan.Name, time.Unix(entry.ExpiresAtUnix, 0).UTC().Format(time.RFC3339))
		}
	}
	nowUnix := s.cfg.Now().Unix()
	_, _, err = s.cfg.Accounts.Update(ctx, accountID, func(current Account, found bool) (Account, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
		}
		// Account holds no reference fields, so the copy is the clone.
		next := current
		next.AdminNote = note
		next.AdminActionAtUnix = nowUnix
		return next, true, nil
	})
	if err != nil {
		return RoleCreation{}, err
	}
	deleter, ok := s.cfg.Slots.(versionstore.ConditionalDeleter[string, Slot])
	if !ok {
		return RoleCreation{}, fmt.Errorf("account: slot store must support atomic identity-checked DeleteIf")
	}
	err = deleter.DeleteIf(ctx, key, slot, func(current Slot) bool {
		return current.PlayerID == 0 && current.Creation.ID == plan.ID
	})
	if err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			return RoleCreation{}, fmt.Errorf("%w: the slot of %s on server %d changed while being resolved; "+
				"look again (%v)", ErrConflict, accountID, serverID, err)
		}
		return RoleCreation{}, err
	}
	s.report.Accepted("admin.resolve_pending_creation")
	return plan, nil
}

// validateAdminNote requires a reason and bounds it.
//
// Required, with no default: releasing a slot is a human assertion that the
// plan is to be abandoned, and the next person to look at the account has to
// be able to find out why.
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
