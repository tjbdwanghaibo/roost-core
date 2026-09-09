package nest

import (
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

var (
	ErrCastGetterNotSet  = errors.New("nest: cast getter not set")
	ErrCastNoContext     = errors.New("nest: cast requires entity context")
	ErrCastInvalidTarget = errors.New("nest: invalid cast target")
	ErrCastDeadlockRisk  = errors.New("nest: cast deadlock risk")
	ErrCastTypeMismatch  = errors.New("nest: cast type mismatch")
)

// CastTarget describes an entity to lock in the current entity context.
type CastTarget struct {
	ID int64
}

// NewCastTarget builds a full-ID cast target.
func NewCastTarget(id int64) CastTarget {
	return CastTarget{ID: id}
}

// CastOne retrieves and locks one entity in the current entity context.
func CastOne[E entity.IThreadSafeEntity](id int64) (E, error) {
	return CastTargetOne[E](NewCastTarget(id))
}

// CastTargetOne retrieves and locks one explicit target.
func CastTargetOne[E entity.IThreadSafeEntity](target CastTarget) (E, error) {
	var zero E
	es, err := CastMulti(target)
	if err != nil {
		return zero, err
	}
	if len(es) != 1 || es[0] == nil {
		return zero, ErrEntityNotFound
	}
	ret, ok := es[0].(E)
	if !ok {
		return zero, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, target.ID, es[0])
	}
	return ret, nil
}

func CastTwo[E1, E2 entity.IThreadSafeEntity](t1, t2 CastTarget) (E1, E2, error) {
	var zero1 E1
	var zero2 E2
	es, err := CastMulti(t1, t2)
	if err != nil {
		return zero1, zero2, err
	}
	if len(es) != 2 || es[0] == nil || es[1] == nil {
		return zero1, zero2, ErrEntityNotFound
	}
	e1, ok := es[0].(E1)
	if !ok {
		return zero1, zero2, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, t1.ID, es[0])
	}
	e2, ok := es[1].(E2)
	if !ok {
		return zero1, zero2, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, t2.ID, es[1])
	}
	return e1, e2, nil
}

func CastThree[E1, E2, E3 entity.IThreadSafeEntity](t1, t2, t3 CastTarget) (E1, E2, E3, error) {
	var zero1 E1
	var zero2 E2
	var zero3 E3
	es, err := CastMulti(t1, t2, t3)
	if err != nil {
		return zero1, zero2, zero3, err
	}
	if len(es) != 3 || es[0] == nil || es[1] == nil || es[2] == nil {
		return zero1, zero2, zero3, ErrEntityNotFound
	}
	e1, ok := es[0].(E1)
	if !ok {
		return zero1, zero2, zero3, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, t1.ID, es[0])
	}
	e2, ok := es[1].(E2)
	if !ok {
		return zero1, zero2, zero3, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, t2.ID, es[1])
	}
	e3, ok := es[2].(E3)
	if !ok {
		return zero1, zero2, zero3, fmt.Errorf("%w: id=%d entity=%T", ErrCastTypeMismatch, t3.ID, es[2])
	}
	return e1, e2, e3, nil
}

// CastMulti retrieves and locks targets in the current entity guard scope.
func CastMulti(targets ...CastTarget) ([]entity.IThreadSafeEntity, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: empty targets", ErrCastInvalidTarget)
	}
	if entity.CurrentGuardScope() == nil {
		return nil, ErrCastNoContext
	}
	current := currentNestDispatchMsg()
	if current == nil {
		return nil, ErrCastNoContext
	}
	getter := current.getter
	if getter == nil {
		return nil, ErrCastGetterNotSet
	}

	guard := entity.GetEntityGuard()
	metas := make([]entity.EntityIDMeta, len(targets))
	ids := make([]int64, len(targets))
	categories := make([]entity.EntityCategory, len(targets))
	for i, target := range targets {
		if target.ID == 0 {
			return nil, fmt.Errorf("%w: index=%d id=0", ErrCastInvalidTarget, i)
		}
		fullID, err := entity.NormalizeFullID(target.ID, entity.EntityKindNone)
		if err != nil {
			return nil, fmt.Errorf("%w: index=%d id=%d: %v", ErrCastInvalidTarget, i, target.ID, err)
		}
		meta := entity.ResolveEntityID(fullID)
		metas[i] = meta
		ids[i] = meta.FullID
		categories[i] = meta.Category
	}
	if !guard.CheckContainAllIDs(ids) {
		return nil, fmt.Errorf("%w: ids=%v", ErrCastDeadlockRisk, ids)
	}

	if err := refuseUndeclaredRemoteTargets(guard, metas); err != nil {
		return nil, err
	}

	es, err := getter.GetMany(nestBaseContext(), ids, categories)
	if err != nil {
		return nil, err
	}
	if len(es) != len(targets) {
		return nil, fmt.Errorf("%w: getter returned %d entities for %d targets", ErrCastInvalidTarget, len(es), len(targets))
	}

	lockEs := make([]entity.IThreadSafeEntity, 0, len(es))
	for i, e := range es {
		if e == nil {
			return nil, fmt.Errorf("%w: index=%d id=%d", ErrEntityNotFound, i, targets[i].ID)
		}
		lockEs = append(lockEs, e)
	}
	if !guard.CheckContainAllLock(lockEs) {
		return nil, fmt.Errorf("%w: ids=%v", ErrCastDeadlockRisk, ids)
	}
	SortEntity(lockEs)

	lockedNow := make([]entity.IThreadSafeEntity, 0, len(lockEs))
	for _, e := range lockEs {
		if guard.Guarded(e.GUId()) {
			continue
		}
		if !e.Touch() {
			releaseCastEntities(guard, lockedNow)
			return nil, ErrEntityNotFound
		}
		if !guard.RequireEntity(e) {
			e.UnTouch()
			releaseCastEntities(guard, lockedNow)
			return nil, ErrLockTimeout
		}
		e.UnTouch()
		lockedNow = append(lockedNow, e)
	}
	return es, nil
}

// ReleaseCast releases one cast entity before the current context ends.
func ReleaseCast(e entity.IThreadSafeEntity) {
	if e == nil || entity.CurrentGuardScope() == nil {
		return
	}
	entity.GetEntityGuard().ReleaseEntity(e.GUId())
}

func releaseCastEntities(guard *entity.EntityGuard, es []entity.IThreadSafeEntity) {
	for _, e := range es {
		guard.ReleaseEntity(e.GUId())
	}
}

// refuseUndeclaredRemoteTargets is the remote-entity gate of a cast. It never
// acquires anything; it only decides whether the cast may proceed.
//
// A remote-managed entity is written under a distributed ownership guard, and
// that guard is taken BEFORE the dispatch runs: the message declares its remote
// targets as RemoteAccess, prepareRemoteWriteBatch locks them in lock order and
// hands them to the guard scope, so by the time a handler casts to one of them
// the entity is already Guarded and the cast just reuses it. Taking a
// distributed lock in the middle of a handler — while local entity locks are
// already held, with the caller's deadline unknown — is exactly the lock-order
// and timeout hazard the declaration step exists to avoid, so a remote target
// that was not declared is refused rather than locked on the spot.
//
// Consequently there are only three outcomes:
//   - every remote-managed target is already held by this dispatch (or there is
//     none): nil, the cast continues on local locks only;
//   - an undeclared remote-managed target outside any dispatch: an error naming
//     the missing dispatch, since there is no message to have declared it on;
//   - an undeclared remote-managed target inside a dispatch:
//     ErrRemoteWriteCapabilityDisabled with "must be declared before dispatch".
//
// This used to return an entity.RemoteEntityRelease as well, and CastMulti
// carried plumbing (Msg.RemoteReleases, a deferred release, releaseRemoteEntities
// at dispatch end) for a release that this function could never produce. That
// dead channel was removed on 2026-09-09; if dynamic remote casting is ever
// wanted, it needs a real design for lock order and deadlines, not a hook.
//
// Only remote-MANAGED kinds are gated (shouldPrepareRemoteID): a remote-capable
// kind that is not managed by this service is an ordinary local entity here.
func refuseUndeclaredRemoteTargets(guard *entity.EntityGuard, metas []entity.EntityIDMeta) error {
	undeclared := 0
	for _, meta := range metas {
		if !shouldPrepareRemoteID(meta) {
			continue
		}
		if guard.Guarded(meta.FullID) {
			continue
		}
		undeclared++
	}
	if undeclared == 0 {
		return nil
	}
	if currentNestDispatchMsg() == nil {
		return fmt.Errorf("remote entity cast requires an active Nest dispatch")
	}
	return fmt.Errorf("%w: remote targets must be declared before dispatch", entity.ErrRemoteWriteCapabilityDisabled)
}
