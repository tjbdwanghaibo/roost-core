package nest

import (
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
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
// 目标不存在、或在等锁期间被 Destroy / 仅内存卸载时返回满足 errors.Is(err, ErrEntityNotFound) 的错误（RR-20260926-73）。
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
		if guard.GuardedEntity(e) {
			continue
		}
		// 等锁之前记下目标 ID：等锁期间实体被清理后 e.GUId() 已归零，错误文本会变成 id=0（RR-20260926-73 复核残留）。
		id := e.GUId()
		if !e.Touch() {
			releaseCastEntities(guard, lockedNow)
			return nil, ErrEntityNotFound
		}
		if !guard.RequireEntity(e) {
			e.UnTouch()
			releaseCastEntities(guard, lockedNow)
			if e.IsRemoved() || e.IsClear() {
				// 等锁期间目标被 Destroy / 仅内存卸载：目标已不在，不是暂时性锁冲突。旧实现一律返回 ErrLockTimeout，
				// 不能回滚的 handler 因此被整条重排、已做的修改重复生效（RR-20260926-73）。
				return nil, fmt.Errorf("%w: id=%d was removed while waiting for its lock", ErrEntityNotFound, id)
			}
			return nil, ErrLockTimeout
		}
		e.UnTouch()
		lockedNow = append(lockedNow, e)
	}
	if mutation := entity.CurrentSyncMutation(); mutation != nil {
		mutation.Include(lockedNow)
	}
	// 动态取得的实体属于当前业务事务，回滚/持久化参与不能依赖是否装配 Sync。
	// 捕获失败时在事务上记下（RR-20260927-31）：之前只把错误还给业务，业务吞掉后事务照常提交，一个未进入回滚 / 持久化
	// 参与的实体随事务落地。现在与 CreateInScope 捕获失败（RR-20260927-11）同一机制，handler 结束时整条回滚；已取得的锁
	// 仍由 Guard 在事务结束时统一释放。
	if tx := CurrentRollbackTx(); tx != nil {
		if err := tx.CaptureEntities(lockedNow); err != nil {
			tx.noteCaptureFailed(err)
			return nil, err
		}
	}
	return es, nil
}

// ReleaseCast 在无事务的上下文中提前释放动态实体 e 这个实例的锁。
// 事务或正式同步作用域内保留锁，直到回滚或准入捕获完成后由 Guard 统一释放。
// 按实例释放（RR-20260927-26）：handler 内 Destroy e 后又新建了同 ID 的实例时，ReleaseCast(e) 只释放 e 自己的锁，
// 不会放掉新实例的锁；之前按 e.GUId() 释放，e 的清理被别的访问者推迟、ID 尚未清零时释放的是新实例。
func ReleaseCast(e entity.IThreadSafeEntity) {
	if CurrentRollbackTx() != nil || entity.CurrentSyncMutation() != nil {
		return
	}
	if e == nil || entity.CurrentGuardScope() == nil {
		return
	}
	entity.GetEntityGuard().ReleaseEntityInstance(e)
}

// releaseCastEntities 归还本次 Cast 刚取得的锁（取锁中途失败）。这些实例刚由本 goroutine 加锁、就是各自 ID 的当前登记，
// 按实例释放与按 ID 等价；用按实例的入口与 ReleaseCast 保持同一语义。
func releaseCastEntities(guard *entity.EntityGuard, es []entity.IThreadSafeEntity) {
	for _, e := range es {
		guard.ReleaseEntityInstance(e)
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
