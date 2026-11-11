package account

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
)

// createRole uses the slot itself as the durable intent. Concurrent retries
// share one plan and one player ID; no side effect lives inside a CAS callback.
//
// What to do with the slot it finds is decided by the creation table
// (decideCreation), the same one Admin.ResolvePendingCreation reads: a
// request for a different name than the pending plan's is refused with
// ErrRoleLimit while that plan can still complete, and releases the slot once
// the plan's name is committed to another owner — the refusal never tells the
// client the pending name, so a player who simply picks another name is the
// ordinary path to that state.
func (s *Service) createRole(ctx context.Context, accountID string, serverID int32, name string) (Role, error) {
	key := slotKeyFor(accountID, serverID)
	// At most one release per call: a second pass that still meets another
	// name met a plan some other request just made, which is a live one.
	released := false
	// 换服占位同样至多看一次：没有角色记录认的撤掉重来，有人认的按名额已占回答。
	holdChecked := false
	for {
		slot, found, err := s.cfg.Slots.Get(ctx, key)
		if err != nil {
			return Role{}, err
		}
		state := classifySlot(slot, found, accountID, serverID)
		if state == slotTransferHold && !holdChecked {
			// 一次已经了结的换服迟到写下的占位（transfer.go 的 releaseStrayHold）不该挡住建角：挡住的话
			// 只有那个角色自己的入口能撤它，而账号在这个服建角恰恰是被它卡住的那一方。
			holdChecked = true
			if err := s.releaseStrayHold(ctx, key); err != nil {
				return Role{}, err
			}
			continue
		}
		action := decideCreation(state, creationEntryFor(slot.Value, name), nameNotRead)
		if action == actMakePlan {
			// The stored plan — this request's, or a concurrent one's that won
			// the insert — is what the rest of the table decides on.
			if slot, err = s.makeCreationPlan(ctx, key, accountID, serverID, name); err != nil {
				return Role{}, err
			}
			state = classifySlot(slot, true, accountID, serverID)
			action = decideCreation(state, creationEntryFor(slot.Value, name), nameNotRead)
		}
		entry := creationEntryFor(slot.Value, name)
		var nameAt nameState
		if action == actReadName {
			if released {
				// The plan that took the released slot is a fresh one.
				return Role{}, fmt.Errorf("%w: another name is pending on server %d", ErrRoleLimit, serverID)
			}
			entryInDirectory, held, err := s.cfg.Names.Lookup(ctx, slot.Value.Creation.Name)
			if err != nil {
				return Role{}, err
			}
			nameAt = classifyName(entryInDirectory, held, creationOwner(key, slot.Value.Creation))
			action = decideCreation(state, entry, nameAt)
		}
		switch action {
		case actReturnRole:
			stored, found, err := s.cfg.Roles.Get(ctx, slot.Value.PlayerID)
			if err != nil {
				return Role{}, err
			}
			if !found || stored.Value.CreationID != slot.Value.Creation.ID {
				return Role{}, fmt.Errorf("%w: completed creation has no matching role", ErrConflict)
			}
			if stored.Value.ServerID != serverID {
				// 角色已经换服走了，这个 slot 是提交之后还没删掉的旧服 slot（transfer.go 的收尾）。收尾补完
				// 之前名额仍按已占用回答，与换一个名字时相同；不把绑定在别的服的角色当作本服的建角结果交回。
				s.report.Refused("create_role", "role_limit")
				return Role{}, fmt.Errorf("%w: the slot of %s on server %d still belongs to role %d, which moved to server %d; "+
					"its transfer is not settled yet", ErrRoleLimit, accountID, serverID, stored.Value.PlayerID, stored.Value.ServerID)
			}
			return stored.Value.clone(), nil
		case actResume:
			return s.resumeRoleCreation(ctx, key, slot)
		case actReleaseAndRetry:
			plan := slot.Value.Creation
			// 与同名重试同一个证明：名字已 committed 给别的 owner，这个计划再也不能提交名字、
			// 发布角色；或计划还没 admitted、名字被别人 reserved 或已无人持有
			// （均为维护者决定）——admitted 之后的事都没发生，释放不会留下角色，与同名重试对齐。只被
			// reserved 的已 admitted 计划或名字仍归本计划时表答 ErrRoleLimit，slot 保留。DeleteIf 对未 admitted 的计划
			// 只在删除时它仍未 admitted 才删（releaseCreationSlot），与并发的 admission 互斥。
			done, err := s.releaseCreationSlot(ctx, key, slot, planDead(nameAt))
			if err := s.compensated(err); err != nil {
				return Role{}, fmt.Errorf("account: release the pending plan for name %q: %w", plan.Name, err)
			}
			if done {
				s.report.Dropped("create_role.plan_released", 1)
			}
			released = true
			continue
		case actRefuseRoleLimit:
			if state == slotPublished {
				s.report.Refused("create_role", "role_limit")
				return Role{}, fmt.Errorf("%w: %s already holds a role on server %d", ErrRoleLimit, accountID, serverID)
			}
			if state == slotTransferHold {
				s.report.Refused("create_role", "role_limit")
				return Role{}, fmt.Errorf("%w: role transfer %s holds the slot of %s on server %d", ErrRoleLimit, slot.Value.TransferID, accountID, serverID)
			}
			return Role{}, fmt.Errorf("%w: another name is pending on server %d", ErrRoleLimit, serverID)
		case actRefuseConflict:
			if state == slotForeign {
				return Role{}, fmt.Errorf("%w: slot identity mismatch", ErrConflict)
			}
			return Role{}, fmt.Errorf("%w: slot has no creation plan; reconcile it offline", ErrConflict)
		default:
			return Role{}, fmt.Errorf("account: creation table answered %d for slot %d, entry %d, name %d", action, state, entry, nameAt)
		}
	}
}

// makeCreationPlan inserts a fresh plan for name into an absent slot and
// returns the stored slot — this plan, or the one a concurrent request
// inserted first. Even an initial Create reply can be lost; the persisted
// plan is enough for the same request to recover on a later call without
// deleting it.
func (s *Service) makeCreationPlan(ctx context.Context, key, accountID string, serverID int32, name string) (versionstore.Versioned[Slot], error) {
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
	slot, found, err := s.cfg.Slots.Get(ctx, key)
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
// stranded slot would stay invisible (hence Config.Metrics).
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
			// is final for this plan, so holding the slot would block this
			// account on this server for good. The creation
			// table says which; a failed read is judged on the weakest fact
			// the refusal proves — someone else holds the name.
			name := nameReservedElsewhere
			entry, held, lookupErr := s.cfg.Names.Lookup(ctx, plan.Name)
			if lookupErr == nil {
				name = classifyName(entry, held, owner)
			}
			return Role{}, errors.Join(fmt.Errorf("%w: %q", ErrNameTaken, plan.Name), lookupErr, s.refuseResumedName(ctx, key, slot, name))
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
		// Committed under our owner but a token this plan never admitted.
		return Role{}, errors.Join(fmt.Errorf("%w: %q", ErrNameTaken, plan.Name), s.refuseResumedName(ctx, key, slot, nameCommittedByPlan))
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
		_, releaseErr := s.releaseCreationSlot(ctx, key, admitted, true)
		cleanup := errors.Join(s.compensated(s.cfg.Names.Cancel(ctx, claim)), s.compensated(releaseErr))
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

// refuseResumedName is the table's answer once a same-name resume was
// refused its name: release the slot when the plan is dead or was never
// admitted, keep it otherwise. A dead plan actually released is counted as
// create_role.plan_released; a failed release as rollback.failed.
func (s *Service) refuseResumedName(ctx context.Context, key string, slot versionstore.Versioned[Slot], name nameState) error {
	state := classifySlot(slot, true, slot.Value.AccountID, slot.Value.ServerID)
	if decideCreation(state, entrySameNameRefused, name) != actReleaseRefuseNameTaken {
		return nil
	}
	released, err := s.releaseCreationSlot(ctx, key, slot, planDead(name))
	if err != nil {
		return s.compensated(err)
	}
	if released && planDead(name) {
		s.report.Dropped("create_role.plan_released", 1)
	}
	return nil
}

// releaseCreationSlot only compensates a definite pre-role refusal, or a plan
// that is provably dead — a foreign allocator ID, or a name committed to
// another owner — in which case admission no longer protects the slot.
// Ordinary/unknown storage failures never enter here.
//
// released reports whether THIS call deleted the slot. A delete fenced out
// because the slot already moved — another request released it first, or
// re-admitted it — is not an error and not a release: two different-name
// requests racing on one dead plan must not count plan_released twice for one
// delete.
func (s *Service) releaseCreationSlot(ctx context.Context, key string, slot versionstore.Versioned[Slot], planDead bool) (released bool, err error) {
	deleter, ok := s.cfg.Slots.(versionstore.ConditionalDeleter[string, Slot])
	if !ok {
		return false, fmt.Errorf("account: slot store must support atomic identity-checked DeleteIf")
	}
	err = deleter.DeleteIf(ctx, key, slot, func(current Slot) bool {
		return current.PlayerID == 0 && current.Creation.ID == slot.Value.Creation.ID && (planDead || !current.Creation.Admitted)
	})
	if errors.Is(err, versionstore.ErrVersionMismatch) {
		return false, nil
	}
	return err == nil, err
}

// Pending roles are not playable, and neither is a role without a creation
// identity: every role this version writes carries one, and roles written
// before creation plans existed are not supported (maintainer 2026-10-13).
func (s *Service) roleReady(ctx context.Context, role Role) error {
	_, err := s.readySlot(ctx, role)
	return err
}

// readySlot 返回角色当前可用的 slot：在它的绑定服上、属于这个角色和它的建角计划、属于当前这一代绑定
// （Slot.BindEpoch，换服见 transfer.go）。换服的占位属于下一代，提交后没删掉的旧服 slot 属于上一代，
// 都不算；所以一个角色同一时刻至多有一个可用的 slot。
func (s *Service) readySlot(ctx context.Context, role Role) (Slot, error) {
	if role.CreationID == "" {
		return Slot{}, fmt.Errorf("%w: role %d has no creation identity", ErrConflict, role.PlayerID)
	}
	slot, found, err := s.cfg.Slots.Get(ctx, slotKeyFor(role.AccountID, role.ServerID))
	if err != nil {
		return Slot{}, err
	}
	if !found || !slotServes(slot.Value, role) {
		return Slot{}, fmt.Errorf("%w: role creation is pending reconciliation", ErrConflict)
	}
	return slot.Value, nil
}

// slotServes 判断 slot 是不是 role 当前可用的那一个：指向这个角色、是它的建角计划、属于它当前这一代绑定。
// readySlot 与 RoleOnServer 共用这一条，“角色可用”只有一种说法。调用方保证 slot 在角色的绑定服上、
// 角色带着建角身份（CreationID 非空）。
func slotServes(slot Slot, role Role) bool {
	return role.CreationID != "" && slot.PlayerID == role.PlayerID && slot.Creation.ID == role.CreationID && slot.BindEpoch == role.BindEpoch
}
