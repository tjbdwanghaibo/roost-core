package account

import (
	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// 建角判定表（维护者决定 B9，docs/feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md）。
//
// 一个账号在一个区服的建角由两样持久事实决定：名额（slot，同时是持久的建角计划）和计划名字在
// directory 里的条目。此前每个入口各自写判断，同一个已判定的事实只在部分入口生效：RR-20260929-19
// （计划不持久）→ RR-20261001-06（名字被别人 committed 的死计划只在同名重试里释放）→ 其残余（换名
// 请求不判死计划）。所以入口不再各写各的：先把两样事实归类（classifySlot / classifyName，Admin 与
// createRole 共用同一份），再查 decideCreation，按返回的动作执行。表的全部格子见
// creation_table_test.go。
//
// 表只管“进入哪条路”。执行中的事实——Reserve 之后的 admission CAS、Roles.Create 撞上别人的
// PlayerID、Commit、发布 CAS——是执行步骤自己的结果，不在表里。

// slotState is what the slot says about one account on one server.
type slotState uint8

const (
	// slotAbsent: no slot. Nothing is pending or published.
	slotAbsent slotState = iota + 1
	// slotForeign: the record names another account or server — corrupt.
	slotForeign
	// slotLegacy: no role and no creation identity, written before plans
	// existed (RR-20260929-19); a published legacy role may hang off it.
	slotLegacy
	// slotPendingUnadmitted: a plan whose name reservation is not admitted
	// on the slot yet. Nothing after admission has happened, so releasing it
	// cannot strand a role.
	slotPendingUnadmitted
	// slotPendingAdmitted: a plan whose reservation is admitted. Storage
	// errors from here on are recovered forward, never by deleting the slot,
	// unless the plan is provably dead.
	slotPendingAdmitted
	// slotPublished: the role exists and the slot names it.
	slotPublished
)

// nameState is where the plan's name stands in the directory, relative to
// the plan's own reservation owner (creationOwner).
type nameState uint8

const (
	// nameNotRead: the entry did not read it. A same-name create resumes
	// first and lets the atomic Reserve answer; it reads the name only after
	// Reserve refused.
	nameNotRead nameState = iota + 1
	// nameFree: no live entry (a lapsed reservation reads as free).
	nameFree
	nameReservedByPlan
	nameCommittedByPlan
	nameReservedElsewhere
	// nameCommittedElsewhere is final for the plan: Commit needs the token
	// the entry now carries and a committed entry leaves only by its owner's
	// Release, so the plan can never publish (RR-20261001-06).
	nameCommittedElsewhere
)

// creationEntry is which path is asking.
type creationEntry uint8

const (
	// entrySameName: CreateRole with the plan's own name (or, with no plan,
	// any name — the new plan will carry it).
	entrySameName creationEntry = iota + 1
	// entrySameNameRefused: the same-name resume's Reserve was refused with
	// ErrKeyTaken (or found the name committed under a token the plan never
	// admitted); the name has been read since.
	entrySameNameRefused
	// entryOtherName: CreateRole with a name other than the pending plan's.
	entryOtherName
	// entryResolve: the operator's Admin.ResolvePendingCreation.
	entryResolve
)

// creationAction is what the entry does next.
type creationAction uint8

const (
	// actMakePlan: no slot — create a plan for the requested name and decide again.
	actMakePlan creationAction = iota + 1
	// actReturnRole: the slot published this name's plan; return that role.
	actReturnRole
	// actResume: drive the plan forward (Reserve → admit → Create → Commit → publish).
	actResume
	// actReadName: the decision depends on the name; read it and decide again.
	actReadName
	// actReleaseAndRetry: the plan is dead; release the slot and decide again
	// for the same request (at most once per call).
	actReleaseAndRetry
	// actReleaseRefuseNameTaken: answer ErrNameTaken and release the slot —
	// the plan is dead, or it was never admitted, so the refusal is definite.
	actReleaseRefuseNameTaken
	// actRefuseNameTaken: answer ErrNameTaken and keep the admitted plan: the
	// name may yet come back to it.
	actRefuseNameTaken
	// actRefuseRoleLimit: one role per account per server, or another plan
	// that may still complete holds the slot.
	actRefuseRoleLimit
	// actRefuseConflict: the slot cannot be acted on automatically.
	actRefuseConflict
	// actResolveRelease: the operator abandons the plan; release the slot.
	actResolveRelease
	// actRefuseUnresolvable: the operator cannot safely abandon this slot.
	actRefuseUnresolvable
)

// decideCreation is the table. It is a pure function of the two classified
// facts and the entry, so every cell has one answer for every entry — the
// property the RR-19 / RR-06 chain lacked.
func decideCreation(slot slotState, entry creationEntry, name nameState) creationAction {
	switch slot {
	case slotForeign:
		return actRefuseConflict
	case slotAbsent:
		if entry == entryResolve {
			return actRefuseUnresolvable
		}
		return actMakePlan
	case slotLegacy:
		if entry == entryResolve {
			return actRefuseUnresolvable
		}
		return actRefuseConflict
	case slotPublished:
		switch entry {
		case entrySameName, entrySameNameRefused:
			return actReturnRole
		case entryResolve:
			return actRefuseUnresolvable
		default:
			return actRefuseRoleLimit
		}
	}
	// A pending plan, admitted or not.
	if name == nameNotRead && entry != entrySameName {
		return actReadName
	}
	switch entry {
	case entrySameName:
		return actResume
	case entrySameNameRefused:
		if name == nameCommittedElsewhere || slot == slotPendingUnadmitted {
			return actReleaseRefuseNameTaken
		}
		return actRefuseNameTaken
	case entryOtherName:
		// Only a plan that can never complete gives way to another name; one
		// that may still complete keeps the slot (ErrRoleLimit).
		if name == nameCommittedElsewhere {
			return actReleaseAndRetry
		}
		return actRefuseRoleLimit
	default: // entryResolve
		// The plan still holds its own name: reserved, so an attempt may be in
		// flight; or committed, so it completes by a same-name CreateRole.
		if name == nameReservedByPlan || name == nameCommittedByPlan {
			return actRefuseUnresolvable
		}
		return actResolveRelease
	}
}

// planDead reports whether the name state proves the plan can never
// complete. It is what lets a release touch an admitted plan.
func planDead(name nameState) bool { return name == nameCommittedElsewhere }

// classifySlot reads the slot facts the table needs. found=false is
// slotAbsent.
func classifySlot(slot versionstore.Versioned[Slot], found bool, accountID string, serverID int32) slotState {
	switch {
	case !found:
		return slotAbsent
	case slot.Value.AccountID != accountID || slot.Value.ServerID != serverID:
		return slotForeign
	case slot.Value.PlayerID != 0:
		return slotPublished
	case slot.Value.Creation.ID == "" || slot.Value.Creation.PlayerID == 0:
		return slotLegacy
	case slot.Value.Creation.Admitted:
		return slotPendingAdmitted
	default:
		return slotPendingUnadmitted
	}
}

// classifyName places a directory lookup of the plan's name relative to the
// plan's owner. Any state other than committed is a reservation, which
// lapses.
func classifyName(entry directory.Entry, found bool, owner directory.Owner) nameState {
	switch {
	case !found:
		return nameFree
	case entry.Owner == owner && entry.State == directory.StateCommitted:
		return nameCommittedByPlan
	case entry.Owner == owner:
		return nameReservedByPlan
	case entry.State == directory.StateCommitted:
		return nameCommittedElsewhere
	default:
		return nameReservedElsewhere
	}
}

// creationEntryFor is the create path's entry: the same name when the slot's
// plan (pending or published) carries it, another name otherwise. A slot
// with no creation identity has no name to match.
func creationEntryFor(slot Slot, name string) creationEntry {
	if slot.Creation.ID != "" && slot.Creation.Name == name {
		return entrySameName
	}
	return entryOtherName
}
