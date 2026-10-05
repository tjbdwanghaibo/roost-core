package account

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// createRole uses the slot itself as the durable intent. Concurrent retries
// share one plan and one player ID; no side effect lives inside a CAS callback.
//
// A request for a different name than the pending plan's is refused with
// ErrRoleLimit while that plan can still complete. Once the plan's name is
// committed to another owner it never can, and the slot is released here
// too, not only on a same-name retry: the refusal never tells the client the
// pending name, so a player who simply picks another name is the ordinary
// path to this state (RR-20261001-06 残余).
func (s *Service) createRole(ctx context.Context, accountID string, serverID int32, name string) (Role, error) {
	key := slotKeyFor(accountID, serverID)
	// At most one release per call: a second pass that still meets another
	// name met a plan some other request just made, which is a live one.
	for released := false; ; released = true {
		slot, err := s.creationSlot(ctx, key, accountID, serverID, name)
		if err != nil {
			return Role{}, err
		}
		if slot.Value.AccountID != accountID || slot.Value.ServerID != serverID {
			return Role{}, fmt.Errorf("%w: slot identity mismatch", ErrConflict)
		}
		if slot.Value.PlayerID != 0 {
			if slot.Value.Creation.ID != "" && slot.Value.Creation.Name == name {
				stored, found, err := s.cfg.Roles.Get(ctx, slot.Value.PlayerID)
				if err != nil {
					return Role{}, err
				}
				if !found || stored.Value.CreationID != slot.Value.Creation.ID {
					return Role{}, fmt.Errorf("%w: completed creation has no matching role", ErrConflict)
				}
				return stored.Value.clone(), nil
			}
			s.report.Refused("create_role", "role_limit")
			return Role{}, fmt.Errorf("%w: %s already holds a role on server %d", ErrRoleLimit, accountID, serverID)
		}
		plan := slot.Value.Creation
		if plan.ID == "" || plan.PlayerID == 0 {
			return Role{}, fmt.Errorf("%w: legacy empty slot requires reconciliation", ErrConflict)
		}
		if plan.Name == name {
			return s.resumeRoleCreation(ctx, key, slot)
		}
		if released {
			return Role{}, fmt.Errorf("%w: another name is pending on server %d", ErrRoleLimit, serverID)
		}
		// 与同名重试同一个证明：名字已 committed 给别的 owner，这个计划再也不能提交名字、
		// 发布角色；只被 reserved（会过期）或仍归本计划时保留 slot。
		dead, err := s.nameCommittedElsewhere(ctx, plan.Name, creationOwner(key, plan))
		if err != nil {
			return Role{}, err
		}
		if !dead {
			return Role{}, fmt.Errorf("%w: another name is pending on server %d", ErrRoleLimit, serverID)
		}
		if err := s.compensated(s.releaseCreationSlot(ctx, key, slot, true)); err != nil {
			return Role{}, fmt.Errorf("account: release the pending plan whose name %q was committed elsewhere: %w", plan.Name, err)
		}
		s.report.Dropped("create_role.plan_released", 1)
	}
}

// creationSlot reads the slot, creating a fresh plan for name when there is
// none. Even an initial Create reply can be lost; the persisted plan is
// enough for the same request to recover on a later call without deleting it.
func (s *Service) creationSlot(ctx context.Context, key, accountID string, serverID int32, name string) (versionstore.Versioned[Slot], error) {
	slot, found, err := s.cfg.Slots.Get(ctx, key)
	if err != nil || found {
		return slot, err
	}
	playerID, err := s.cfg.Allocator.Allocate(ctx, serverID)
	if err != nil {
		return versionstore.Versioned[Slot]{}, err
	}
	if playerID == 0 {
		return versionstore.Versioned[Slot]{}, fmt.Errorf("account: allocator returned a zero player id")
	}
	plan := RoleCreation{ID: rand.Text(), PlayerID: playerID, Name: name, CreatedAtUnix: s.cfg.Now().Unix()}
	slot, created, err := s.cfg.Slots.Create(ctx, key, Slot{AccountID: accountID, ServerID: serverID, Creation: plan})
	if err != nil || created {
		return slot, err
	}
	slot, found, err = s.cfg.Slots.Get(ctx, key)
	if err != nil {
		return versionstore.Versioned[Slot]{}, err
	}
	if !found {
		return versionstore.Versioned[Slot]{}, fmt.Errorf("%w: creation slot changed during admission", ErrConflict)
	}
	return slot, nil
}

// creationOwner is the directory owner of one plan's name reservation. It is
// operation-specific so an abandoned attempt cannot reuse or compensate a
// later attempt's reservation on the same account/slot.
func creationOwner(key string, plan RoleCreation) directory.Owner {
	return directory.Owner(key + "/" + plan.ID)
}

// compensated reports a failed compensation and returns err unchanged. A
// release or cancel that fails leaves a slot or a reservation behind; the
// error is returned, but a returned error nobody aggregates is how such a
// stranded slot stayed invisible before (NC-50, Config.Metrics).
func (s *Service) compensated(err error) error {
	if err != nil {
		s.report.Dropped("rollback.failed", 1)
	}
	return err
}

func (s *Service) resumeRoleCreation(ctx context.Context, key string, slot versionstore.Versioned[Slot]) (Role, error) {
	plan := slot.Value.Creation
	// An operation-specific owner prevents an abandoned attempt from reusing
	// or compensating a later attempt's reservation on the same account/slot.
	owner := creationOwner(key, plan)
	claim, err := s.cfg.Names.Reserve(ctx, plan.Name, owner, s.cfg.ClaimTTL)
	if err != nil {
		if errors.Is(err, directory.ErrKeyTaken) {
			s.report.Refused("create_role", "name_taken")
			// An admitted plan whose lapsed reservation was merely RE-RESERVED
			// by someone else keeps its slot: that reservation can expire and
			// the plan can still complete. A name COMMITTED to another owner
			// is final for this plan — Commit needs the token the entry now
			// carries, and a committed entry leaves only by its owner's
			// Release, which nothing here calls — so the plan can never
			// publish, and holding the slot would block this account on this
			// server for good (RR-20261001-06).
			dead, lookupErr := s.nameCommittedElsewhere(ctx, plan.Name, owner)
			cleanup := errors.Join(lookupErr, s.compensated(s.releaseCreationSlot(ctx, key, slot, dead)))
			if dead && cleanup == nil {
				s.report.Dropped("create_role.plan_released", 1)
			}
			return Role{}, errors.Join(fmt.Errorf("%w: %q", ErrNameTaken, plan.Name), cleanup)
		}
		return Role{}, err
	}
	// Another retry may have committed the name after our slot snapshot.
	// Verify its durable admission before treating that commit as foreign.
	if claim.ExpiresAt.IsZero() && plan.Claim.Token != claim.Token {
		current, found, err := s.cfg.Slots.Get(ctx, key)
		if err != nil {
			return Role{}, err
		}
		if found && current.Value.Creation.ID == plan.ID {
			plan = current.Value.Creation
			slot = current
		}
	}
	if claim.ExpiresAt.IsZero() && (plan.Claim.Token == "" || claim.Token != plan.Claim.Token) {
		return Role{}, errors.Join(fmt.Errorf("%w: %q", ErrNameTaken, plan.Name), s.compensated(s.releaseCreationSlot(ctx, key, slot, false)))
	}
	admitted, _, err := s.cfg.Slots.Update(ctx, key, func(current Slot, found bool) (Slot, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: slot %s vanished during create", ErrConflict, key)
		}
		if current.Creation.ID != plan.ID {
			return current, false, fmt.Errorf("%w: creation identity changed", ErrConflict)
		}
		if current.PlayerID != 0 {
			return current, false, nil
		}
		if current.Creation.Claim.Token != plan.Claim.Token && current.Creation.Claim.Token != claim.Token {
			return current, false, fmt.Errorf("%w: newer name reservation is already admitted", ErrConflict)
		}
		if current.Creation.Admitted && current.Creation.Claim.Token == claim.Token {
			return current, false, nil
		}
		current.Creation.Claim = claim
		current.Creation.Admitted = true
		return current, true, nil
	})
	// A failed admission can itself have committed; keep the intent and claim.
	if err != nil {
		return Role{}, err
	}
	role := Role{CreationID: plan.ID, PlayerID: plan.PlayerID, AccountID: slot.Value.AccountID, ServerID: slot.Value.ServerID, Name: plan.Name, CreatedAtUnix: plan.CreatedAtUnix}
	stored, created, err := s.cfg.Roles.Create(ctx, role.PlayerID, role)
	if err != nil {
		return Role{}, err
	}
	if !created {
		var found bool
		stored, found, err = s.cfg.Roles.Get(ctx, role.PlayerID)
		if err != nil {
			return Role{}, err
		}
		if !found {
			return Role{}, fmt.Errorf("%w: role disappeared during creation recovery", ErrConflict)
		}
	}
	if stored.Value.CreationID != plan.ID || stored.Value.AccountID != role.AccountID || stored.Value.ServerID != role.ServerID || stored.Value.Name != role.Name {
		// A definitely occupied foreign ID cannot be our uncertain Create.
		// Cancel is token-fenced; slot removal is fenced by the creation identity.
		cleanup := errors.Join(s.compensated(s.cfg.Names.Cancel(ctx, claim)), s.compensated(s.releaseCreationSlot(ctx, key, admitted, true)))
		return Role{}, errors.Join(fmt.Errorf("%w: player id %d is already in use", ErrConflict, role.PlayerID), cleanup)
	}
	role = stored.Value.clone()
	if _, err = s.cfg.Names.Commit(ctx, claim); err != nil {
		return Role{}, err
	}
	_, _, err = s.cfg.Slots.Update(ctx, key, func(current Slot, found bool) (Slot, bool, error) {
		if !found || current.Creation.ID != plan.ID {
			return current, false, fmt.Errorf("%w: slot %s vanished during create or changed identity", ErrConflict, key)
		}
		if current.PlayerID == role.PlayerID {
			return current, false, nil
		}
		if current.PlayerID != 0 {
			return current, false, fmt.Errorf("%w: slot occupant changed", ErrConflict)
		}
		current.PlayerID = role.PlayerID
		return current, true, nil
	})
	if err != nil {
		actual, found, readErr := s.cfg.Slots.Get(ctx, key)
		if readErr != nil {
			return role, errors.Join(err, fmt.Errorf("account: slot outcome unknown: %w", readErr))
		}
		if !found || actual.Value.Creation.ID != plan.ID || actual.Value.PlayerID != role.PlayerID {
			return role, err
		}
	}
	s.report.Accepted("create_role")
	return role.clone(), nil
}

// releaseCreationSlot only compensates a definite pre-role refusal, or a plan
// that is provably dead — a foreign allocator ID, or a name committed to
// another owner — in which case admission no longer protects the slot.
// Ordinary/unknown storage failures never enter here.
func (s *Service) releaseCreationSlot(ctx context.Context, key string, slot versionstore.Versioned[Slot], planDead bool) error {
	deleter, ok := s.cfg.Slots.(versionstore.ConditionalDeleter[string, Slot])
	if !ok {
		return fmt.Errorf("account: slot store must support atomic identity-checked DeleteIf")
	}
	err := deleter.DeleteIf(ctx, key, slot, func(current Slot) bool {
		return current.PlayerID == 0 && current.Creation.ID == slot.Value.Creation.ID && (planDead || !current.Creation.Admitted)
	})
	if errors.Is(err, versionstore.ErrVersionMismatch) {
		return nil
	}
	return err
}

// nameCommittedElsewhere reports whether name is permanently held by an owner
// other than ours. A reservation, ours or anyone's, is not that: it lapses.
func (s *Service) nameCommittedElsewhere(ctx context.Context, name string, owner directory.Owner) (bool, error) {
	entry, found, err := s.cfg.Names.Lookup(ctx, name)
	if err != nil {
		return false, err
	}
	return found && entry.State == directory.StateCommitted && entry.Owner != owner, nil
}

// Pending roles are not playable. Legacy published roles have no CreationID.
func (s *Service) roleReady(ctx context.Context, role Role) error {
	if role.CreationID == "" {
		return nil
	}
	slot, found, err := s.cfg.Slots.Get(ctx, slotKeyFor(role.AccountID, role.ServerID))
	if err != nil {
		return err
	}
	if !found || slot.Value.PlayerID != role.PlayerID || slot.Value.Creation.ID != role.CreationID {
		return fmt.Errorf("%w: role creation is pending reconciliation", ErrConflict)
	}
	return nil
}
