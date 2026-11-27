package entity

import (
	"fmt"
	"reflect"
	"slices"
)

// Immutable admission facts. A snapshot is not a lease: when using peer
// pointers outside an entity lock, Touch them first, as for a manager.Get.
// Under all acquired locks Valid rejects ABA as well as retired instances.
type CompanionDomainSnapshot struct {
	Root    IThreadSafeEntity
	Members []IThreadSafeEntity // nil for a singleton
	manager *EntityManager
	gate    *companionGate
	epoch   uint64
}

func SnapshotCompanionDomain(e IThreadSafeEntity) CompanionDomainSnapshot {
	s := CompanionDomainSnapshot{Root: e}
	g := gateOf(e)
	if g == nil {
		return s
	}
	s.gate = g
	m := e.Base().owningManager()
	if m == nil {
		return s
	}
	m.addMu.Lock()
	defer m.addMu.Unlock()
	if m.entities.Get(g.id) != e {
		return s
	}
	s.manager = m
	if g.parent != nil {
		s.Root = g.parent
		g = gateOf(g.parent)
	}
	s.gate = g
	s.epoch = g.epoch
	if len(g.children) > 0 {
		s.Members = make([]IThreadSafeEntity, 1, len(g.children)+1)
		s.Members[0] = s.Root
		for _, v := range g.children {
			s.Members = append(s.Members, v)
		}
		slices.SortFunc(s.Members[1:], func(a, b IThreadSafeEntity) int {
			if gateOf(a).id < gateOf(b).id {
				return -1
			}
			return 1
		})
	}
	return s
}
func (s CompanionDomainSnapshot) Valid() bool {
	if s.Root == nil {
		return false
	}
	if s.gate == nil {
		return !s.Root.IsRemoved() && !s.Root.IsClear()
	}
	m := s.manager
	if m == nil {
		return false
	}
	m.addMu.Lock()
	defer m.addMu.Unlock()
	return s.validLocked()
}
func (s CompanionDomainSnapshot) validLocked() bool {
	return s.manager.entities.Get(s.gate.id) == s.Root && s.gate.epoch == s.epoch && s.gate.parent == nil && !s.Root.IsRemoved()
}

// linkLocked changes the manager graph and rehomes every active child ticket.
// Both physical domains and manager.addMu MUST be held by the caller.
func linkLocked(p, c IThreadSafeEntity, attach bool) {
	pg, cg := gateOf(p), gateOf(c)
	if attach {
		cg.move(pg.home)
		cg.parent = p
		if pg.children == nil {
			pg.children = make(map[int64]IThreadSafeEntity)
		}
		pg.children[cg.id] = c
	} else {
		cg.move(cg.home)
		cg.parent = nil
		delete(pg.children, cg.id)
	}
	pg.epoch++
	cg.epoch++
}

func (m *EntityManager) checkCompanionInstance(e IThreadSafeEntity) error {
	k := relationKind(e.GetEntityKind())
	if k.typ == nil {
		if gateOf(e) != nil || IsCompanionParticipant(GetEntityKindFromID(e.ID())) {
			return ErrCompanionKind
		}
		return nil
	}
	g := gateOf(e)
	if reflect.TypeOf(e) != k.typ || g == nil || g.id != e.ID() || !canonicalCompanionID(g.id, e.GetEntityKind()) {
		return fmt.Errorf("%w: %T ID=%d has no registered stable entrance", ErrCompanionKind, e, e.ID())
	}
	return nil
}

// Called under addMu, BEFORE adding to the manager's visible entity index.
// The child is still private; CreateInScope holds its gate. Only TryAcquire
// is permitted here. There are NO manager tombstones keyed by ID: the caller
// must supply the trusted declaration again on cold restore.
func (m *EntityManager) initializeCompanionLocked(e IThreadSafeEntity, param *EntityCreateParam) error {
	if param == nil {
		return nil
	}
	g := gateOf(e)
	if g == nil {
		return nil
	}
	decl := param.CompanionOf
	if decl == 0 {
		return nil
	}
	pid, err := PrincipalID(g.id)
	if err != nil || pid != decl {
		return ErrCompanionRelation
	}
	if !param.IsCreate {
		g.declared = decl
		return nil
	}
	p := m.entities.Get(decl)
	pg := gateOf(p)
	if pg == nil || p.IsRemoved() {
		return ErrCompanionRelation
	}
	if pg.retiring != 0 {
		return ErrCompanionBusy
	}
	if param.CompanionDetached {
		g.declared = decl
		return nil
	}
	if p.Base().GroupLockID() != 0 || e.Base().GroupLockID() != 0 || p.Base().GroupTransitionPending() || e.Base().GroupTransitionPending() {
		return ErrEntityGroupDomain
	}
	if companionNeedsRemoteGrant(p) || companionNeedsRemoteGrant(e) {
		return fmt.Errorf("%w: create declared detached, then attach with a proved exclusive grant", ErrRemoteFenced)
	}
	if !pg.home.take(nil, true) {
		return ErrCompanionBusy
	}
	defer pg.home.give()
	g.declared = decl
	linkLocked(p, e, true)
	return nil
}

// acquireRetireDomains runs with e's logical ticket held, outside addMu. Graph
// edges cannot move under this domain. Retiring a root also splits all its
// children, so protect their independent entrances before changing any edge.
func (m *EntityManager) acquireRetireDomains(e IThreadSafeEntity, revoked bool) (func(), error) {
	g := gateOf(e)
	if g == nil {
		return func() {}, nil
	}
	m.addMu.Lock()
	if !revoked && (g.reservations != 0 || g.retiring != 0) {
		m.addMu.Unlock()
		return nil, ErrCompanionBusy
	}
	var targets []*companionDomain
	if g.parent != nil {
		targets = append(targets, g.home)
	}
	for _, c := range g.children {
		cg := gateOf(c)
		if !revoked && (cg.reservations != 0 || cg.retiring != 0) {
			m.addMu.Unlock()
			return nil, ErrCompanionBusy
		}
		targets = append(targets, cg.home)
	}
	g.retiring++
	m.addMu.Unlock()
	unmark := func() { m.addMu.Lock(); g.retiring--; m.addMu.Unlock() }
	for i, d := range targets {
		if !d.take(nil, !revoked) {
			for _, prev := range targets[:i] {
				prev.give()
			}
			unmark()
			return nil, ErrCompanionBusy
		}
	}
	return func() {
		unmark()
		for _, d := range targets {
			d.give()
		}
	}, nil
}
func (m *EntityManager) retireCompanionLocked(e IThreadSafeEntity, deleted bool) {
	g := gateOf(e)
	if g == nil {
		return
	}
	if g.parent != nil {
		linkLocked(g.parent, e, false)
	}
	for _, c := range g.children {
		linkLocked(e, c, false)
	}
	// Real deletion ends this principal identity, including declarations on
	// currently detached children. Bounded by registered kinds, not entities.
	if deleted && !IsCompanionKind(GetEntityKindFromID(g.id)) {
		if r := companionRegistry.Load(); r != nil {
			for i, k := range r {
				if k.parent != GetEntityKindFromID(g.id) {
					continue
				}
				cid, err := CompanionID(g.id, EntityKind(i))
				if err != nil {
					continue
				}
				cg := gateOf(m.entities.Get(cid))
				if cg != nil && cg.declared == g.id {
					cg.declared = 0
					cg.epoch++
				}
			}
		}
	}
	g.declared = 0
	g.epoch++
}
