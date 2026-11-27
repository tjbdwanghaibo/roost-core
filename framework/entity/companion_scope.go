package entity

import (
	"reflect"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
)

// A Nest view is narrower than the current Guard: the same guard may serve a
// nested, isolated transaction. Only exact currently enlisted instances qualify.
type companionScope struct {
	guard   *GuardScope
	members []IThreadSafeEntity
	stage   func(*CompanionChange) error
}

var companionScopes goroutine.LocalMap[*companionScope]

func PushCompanionScope(permit entitypermit.Token, members []IThreadSafeEntity, stage func(*CompanionChange) error) func() {
	if !permit.Valid() {
		panic(ErrEntityLockScope)
	}
	gid := goroutine.GoID()
	prev, _ := companionScopes.Load(gid)
	s := &companionScope{guard: CurrentGuardScope(), members: append([]IThreadSafeEntity(nil), members...), stage: stage}
	companionScopes.Store(gid, s)
	return func() {
		if prev != nil {
			companionScopes.Store(gid, prev)
		} else {
			companionScopes.Delete(gid)
		}
	}
}
func currentCompanionScope() *companionScope {
	s, _ := companionScopes.Load(goroutine.GoID())
	if s == nil || s.guard == nil || s.guard != CurrentGuardScope() {
		return nil
	}
	return s
}
func (s *companionScope) has(e IThreadSafeEntity) bool {
	if s == nil || gateOf(e) == nil {
		return false
	}
	for _, x := range s.members {
		if e == x && !e.IsRemoved() && s.guard.guard.eMap[gateOf(e).id] == e {
			return true
		}
	}
	return false
}
func companionFromScope[T IThreadSafeEntity](wantChild bool) (zero T, err error) {
	s := currentCompanionScope()
	if s == nil || len(s.members) == 0 || !s.has(s.members[0]) {
		return zero, ErrCompanionScope
	}
	subject := s.members[0]
	var found IThreadSafeEntity
	for _, e := range s.members {
		g := gateOf(e)
		if g == nil {
			continue
		}
		if reflect.TypeOf(e) != reflect.TypeFor[T]() || !s.has(e) {
			continue
		}
		m := e.Base().owningManager()
		if m == nil {
			continue
		}
		m.addMu.Lock()
		ok := m.entities.Get(g.id) == e
		if wantChild {
			ok = ok && g.parent == subject
		} else {
			ok = ok && g.children[gateOf(subject).id] == subject
		}
		m.addMu.Unlock()
		if !ok {
			continue
		}
		if found != nil && found != e {
			return zero, ErrCompanionScope
		}
		found = e
	}
	if found == nil {
		if isDeclaredDetached(subject, reflect.TypeFor[T](), wantChild) {
			return zero, ErrCompanionDetached
		}
		return zero, ErrCompanionScope
	}
	return found.(T), nil
}

// Never classify an absent, wrong-type or same-number unrelated peer as detached.
func isDeclaredDetached(subject IThreadSafeEntity, want reflect.Type, wantChild bool) bool {
	m := subject.Base().owningManager()
	if m == nil {
		return false
	}
	g := gateOf(subject)
	m.addMu.Lock()
	defer m.addMu.Unlock()
	if m.entities.Get(g.id) != subject {
		return false
	}
	sk := GetEntityKindFromID(g.id)
	if !wantChild {
		return g.declared != 0 && g.parent == nil && relationKind(relationKind(sk).parent).typ == want
	}
	r := companionRegistry.Load()
	if r == nil {
		return false
	}
	for i, k := range r {
		if k.parent == sk && k.typ == want {
			id, err := CompanionID(g.id, EntityKind(i))
			if err != nil {
				return false
			}
			c := gateOf(m.entities.Get(id))
			return c != nil && c.declared == g.id && c.parent == nil
		}
	}
	return false
}
func GetCompanion[T IThreadSafeEntity]() (T, error) { return companionFromScope[T](true) }
func GetPrincipal[T IThreadSafeEntity]() (T, error) { return companionFromScope[T](false) }

type CompanionChange struct {
	Principal, Companion IThreadSafeEntity
	m                    *EntityManager
	attach               bool
	// Physical reservations; no extra native acquisition for an existing domain.
	domains       []*companionDomain
	done, applied bool
}

func (m *EntityManager) reserveCompanion(principalID, childID int64, attach, declare bool, scope *companionScope) (*CompanionChange, error) {
	if m == nil {
		return nil, ErrCompanionRelation
	}
	pid, err := PrincipalID(childID)
	if err != nil || principalID != pid {
		return nil, ErrCompanionRelation
	}
	m.addMu.Lock()
	defer m.addMu.Unlock()
	p, c := m.entities.Get(pid), m.entities.Get(childID)
	pg, cg := gateOf(p), gateOf(c)
	if pg == nil || cg == nil || p.IsRemoved() || c.IsRemoved() || !declare && cg.declared != pid {
		return nil, ErrCompanionRelation
	}
	if scope != nil && (len(scope.members) == 0 || scope.members[0] != p && scope.members[0] != c || !scope.has(scope.members[0])) {
		return nil, ErrCompanionScope
	}
	if attach && (p.Base().GroupLockID() != 0 || c.Base().GroupLockID() != 0 || p.Base().GroupTransitionPending() || c.Base().GroupTransitionPending()) {
		return nil, ErrEntityGroupDomain
	}
	if cg.pending != nil || cg.retiring != 0 || pg.retiring != 0 {
		return nil, ErrCompanionBusy
	}
	if cg.parent != nil && cg.parent != p || !declare && attach == (cg.parent != nil) || declare && (cg.parent != nil || cg.declared != 0 && cg.declared != pid) {
		return nil, ErrCompanionRelation
	}
	ch := &CompanionChange{Principal: p, Companion: c, m: m, attach: attach}
	for _, d := range []*companionDomain{pg.home, cg.home} {
		if !d.take(nil, true) {
			for _, held := range ch.domains {
				held.give()
			}
			return nil, ErrCompanionBusy
		}
		ch.domains = append(ch.domains, d)
	}
	pg.reservations++
	cg.reservations++
	cg.pending = ch
	return ch, nil
}

// Apply is only for an accepted local commit, before releasing ANY locks. A
// rolled-back creation can revoke an endpoint; stale tokens never resurrect it.
func (ch *CompanionChange) Apply() {
	if ch == nil || ch.done || ch.applied {
		return
	}
	ch.m.addMu.Lock()
	defer ch.m.addMu.Unlock()
	p, c := gateOf(ch.Principal), gateOf(ch.Companion)
	if ch.m.entities.Get(p.id) == ch.Principal && ch.m.entities.Get(c.id) == ch.Companion && c.pending == ch {
		linkLocked(ch.Principal, ch.Companion, ch.attach)
	}
	ch.applied = true
}
func (ch *CompanionChange) Release() {
	if ch == nil || ch.done {
		return
	}
	ch.done = true
	ch.m.addMu.Lock()
	p, c := gateOf(ch.Principal), gateOf(ch.Companion)
	p.reservations--
	c.reservations--
	c.pending = nil
	ch.m.addMu.Unlock()
	for _, d := range ch.domains {
		d.give()
	}
}
func changeInScope(childID int64, attach bool) error {
	s := currentCompanionScope()
	if s == nil || s.stage == nil {
		return ErrCompanionScope
	}
	pid, err := PrincipalID(childID)
	if err != nil {
		return err
	}
	var m *EntityManager
	for _, e := range s.members {
		if g := gateOf(e); g != nil && (g.id == pid || g.id == childID) && s.has(e) {
			owner := e.Base().owningManager()
			if owner == nil || m != nil && m != owner {
				return ErrCompanionScope
			}
			m = owner
		}
	}
	if m == nil {
		return ErrCompanionScope
	}
	ch, err := m.reserveCompanion(pid, childID, attach, false, s)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			ch.Release()
		}
	}()
	err = s.stage(ch)
	accepted = err == nil
	return err
}
func AttachCompanion(childID int64) error { return changeInScope(childID, true) }
func DetachCompanion(childID int64) error { return changeInScope(childID, false) }
func companionNeedsRemoteGrant(e IThreadSafeEntity) bool {
	_, actual := e.(IThreadSafeRemoteEntity)
	return actual || IsEntityKindRemoteManaged(GetEntityKindFromID(gateOf(e).id))
}
func (m *EntityManager) changeOutside(childID int64, attach bool) error {
	if insideAnyGuard() {
		return ErrCompanionScope
	}
	p, err := PrincipalID(childID)
	if err != nil {
		return err
	}
	if IsEntityKindRemoteManaged(GetEntityKindFromID(p)) || IsEntityKindRemoteManaged(GetEntityKindFromID(childID)) {
		return ErrCompanionScope
	}
	ch, err := m.reserveCompanion(p, childID, attach, false, nil)
	if err != nil {
		return err
	}
	defer ch.Release()
	if companionNeedsRemoteGrant(ch.Principal) || companionNeedsRemoteGrant(ch.Companion) {
		return ErrRemoteFenced
	}
	ch.Apply()
	return nil
}
func (m *EntityManager) AttachCompanion(childID int64) error { return m.changeOutside(childID, true) }
func (m *EntityManager) DetachCompanion(childID int64) error { return m.changeOutside(childID, false) }

// DeclareDetachedCompanion is an explicit trusted restore operation, NOT ID
// discovery. Call only after independently validating the authoritative record.
// Neither peer is authorized for composed work until a separate Attach.
func (m *EntityManager) DeclareDetachedCompanion(principalID, childID int64) error {
	if insideAnyGuard() {
		return ErrCompanionScope
	}
	ch, err := m.reserveCompanion(principalID, childID, false, true, nil)
	if err != nil {
		return err
	}
	defer ch.Release()
	m.addMu.Lock()
	cg := gateOf(ch.Companion)
	cg.declared = principalID
	cg.epoch++
	m.addMu.Unlock()
	return nil
}
