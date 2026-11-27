package entity

import (
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
)

var (
	ErrCompanionDetached = errors.New("entity: explicitly declared companion is detached")
	ErrCompanionKind     = errors.New("entity: invalid or ambiguous companion kind")
	ErrCompanionRelation = errors.New("entity: companion relationship is not authorized")
	ErrCompanionScope    = errors.New("entity: no unambiguous authorized companion in current handler")
	ErrCompanionBusy     = errors.New("entity: companion lock domain busy")
)

type companionKind struct {
	typ    reflect.Type
	parent EntityKind
}
type companionKinds [1 << EntityKindBits]companionKind

var companionRegistry atomic.Pointer[companionKinds]

func relationKind(kind EntityKind) companionKind {
	if r := companionRegistry.Load(); r != nil {
		return r[kind]
	}
	return companionKind{}
}
func IsCompanionKind(kind EntityKind) bool        { return relationKind(kind).parent != 0 }
func IsCompanionParticipant(kind EntityKind) bool { return relationKind(kind).typ != nil }

// RegisterCompanionKind declares the two concrete entity pointer types and their
// registered kinds at startup, before building instances. A parent may have
// several DIFFERENT child kinds; each child kind has exactly one parent kind.
// Types, kinds and roles cannot be aliased, chained or cycled.
func RegisterCompanionKind[P, C IThreadSafeEntity](principalKind, companionKind EntityKind) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	p, c := reflect.TypeFor[P](), reflect.TypeFor[C]()
	if RemoteKindShared(principalKind) || RemoteKindShared(companionKind) || p.Kind() != reflect.Pointer || c.Kind() != reflect.Pointer || p == c || principalKind == 0 || companionKind == 0 || principalKind == companionKind || kindEntryOf(principalKind) == nil || kindEntryOf(companionKind) == nil {
		return ErrCompanionKind
	}
	var r companionKinds
	if old := companionRegistry.Load(); old != nil {
		r = *old
	}
	for k, x := range r {
		if x.typ == p && k != int(principalKind) || x.typ == c && k != int(companionKind) {
			return ErrCompanionKind
		}
	}
	pp, cc := companionKindDef(p, 0), companionKindDef(c, principalKind)
	if x := r[principalKind]; x.typ != nil && x != pp {
		return ErrCompanionKind
	}
	if x := r[companionKind]; x.typ != nil && x != cc {
		return ErrCompanionKind
	}
	r[principalKind], r[companionKind] = pp, cc
	companionRegistry.Store(&r)
	return nil
}
func companionKindDef(t reflect.Type, p EntityKind) companionKind { return companionKind{t, p} }
func canonicalCompanionID(id int64, kind EntityKind) bool {
	rebuilt, err := BuildEntityID(GetUniqueIDFromEntityID(id), kind)
	return err == nil && id > 0 && rebuilt == id
}

// ID conversion is deterministic metadata, never permission to access a peer.
func CompanionID(principalID int64, childKind EntityKind) (int64, error) {
	k := relationKind(childKind).parent
	if k == 0 || !canonicalCompanionID(principalID, k) {
		return 0, fmt.Errorf("%w: parent=%d child kind=%d", ErrCompanionKind, principalID, childKind)
	}
	return BuildEntityID(GetUniqueIDFromEntityID(principalID), childKind)
}
func PrincipalID(childID int64) (int64, error) {
	c := GetEntityKindFromID(childID)
	p := relationKind(c).parent
	if p == 0 || !canonicalCompanionID(childID, c) {
		return 0, ErrCompanionKind
	}
	return BuildEntityID(GetUniqueIDFromEntityID(childID), p)
}

func normalizeCompanionCreate(param *EntityCreateParam) error {
	if param != nil && param.CompanionDetached && param.CompanionOf == 0 {
		return ErrCompanionRelation
	}
	if param == nil || param.CompanionOf == 0 {
		return nil
	}
	id, err := CompanionID(param.CompanionOf, param.Kind)
	if err != nil {
		return err
	}
	u := GetUniqueIDFromEntityID(id)
	if param.Id != 0 && param.Id != id || param.UniqueID != 0 && param.UniqueID != u {
		return fmt.Errorf("%w: child ID or unique does not match principal", ErrCompanionRelation)
	}
	param.Id = id
	param.UniqueID = u
	return nil
}
