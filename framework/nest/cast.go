package nest

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// Cast 的错误：目标未在当前段声明、Getter 缺失、不在实体上下文中、目标非法、按锁序不能等待、类型不符。
var (
	ErrCastUndeclaredTarget = errors.New("nest: cast target not declared in this execution segment")
	ErrCastGetterNotSet     = errors.New("nest: cast getter not set")
	ErrCastNoContext        = errors.New("nest: cast requires entity context")
	ErrCastInvalidTarget    = errors.New("nest: invalid cast target")
	ErrCastDeadlockRisk     = errors.New("nest: cast deadlock risk")
	ErrCastTypeMismatch     = errors.New("nest: cast type mismatch")
)

// CastTarget 描述要在当前实体上下文中锁定的一个实体。
type CastTarget struct {
	ID int64
}

// NewCastTarget 以完整 ID 构造 Cast 目标。
func NewCastTarget(id int64) CastTarget {
	return CastTarget{ID: id}
}

// CastOne 在当前实体上下文中取得并锁定一个实体，按 E 断言类型。
func CastOne[E entity.IThreadSafeEntity](id int64) (E, error) {
	return CastTargetOne[E](NewCastTarget(id))
}

// CastTargetOne 取得并锁定一个显式目标，按 E 断言类型。
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

// CastTwo 一次取得并锁定两个目标，分别按 E1、E2 断言类型。
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

// CastThree 一次取得并锁定三个目标，分别按 E1、E2、E3 断言类型。
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

// CastMulti 在当前实体 Guard 作用域内取得并锁定 targets。目标必须已在当前段的声明目标中，且按锁序可等待；
// 取得的实体纳入当前 Sync 变更与事务。
// 目标不存在、或在等锁期间被 Destroy / 仅内存卸载时返回满足 errors.Is(err, ErrEntityNotFound) 的错误。
func CastMulti(targets ...CastTarget) ([]entity.IThreadSafeEntity, error) {
	if fctx.InIOWorker() {
		return nil, fctx.ErrGuardInIOWorker
	}
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
	declaredIDs := dispatchIDs(current)
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
		// 当前段的全部访问目标必须事先登记 tail，不能持锁扩张依赖。
		if !slices.Contains(declaredIDs, meta.FullID) {
			return nil, fmt.Errorf("%w: id=%d", ErrCastUndeclaredTarget, meta.FullID)
		}
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
		// 等锁之前记下目标 ID：等锁期间实体被清理后 e.GUId() 已归零，错误文本会变成 id=0。
		id := e.GUId()
		if !e.Touch() {
			releaseCastEntities(guard, lockedNow)
			return nil, ErrEntityNotFound
		}
		if !guard.RequireEntity(e) {
			e.UnTouch()
			releaseCastEntities(guard, lockedNow)
			if e.IsRemoved() || e.IsClear() {
				// 等锁期间目标被 Destroy / 仅内存卸载：目标已不在，不是暂时性锁冲突，不能返回 ErrLockTimeout
				// 让消息被整条重排（不能回滚的 handler 已做的修改会重复生效）。
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
	// 捕获失败时在事务上记下：业务即使吞掉错误，handler 结束时也整条回滚（与 CreateInScope 捕获失败同一机制），
	// 不让一个未进入回滚 / 持久化参与的实体随事务落地。已取得的锁仍由 Guard 在事务结束时统一释放。
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
// 按实例释放：handler 内 Destroy e 后又新建了同 ID 的实例时，ReleaseCast(e) 只释放 e 自己的锁，
// 不会放掉新实例的锁（按 ID 释放在 e 的清理被推迟、ID 尚未清零时会放掉新实例）。
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

// refuseUndeclaredRemoteTargets 是 Cast 的 Remote 实体门：它不取得任何东西，只决定 Cast 能否继续。
//
// Remote 托管实体在分布式所有权保护下写入，保护在派发运行之前取得：消息把 Remote 目标声明为 RemoteAccess，
// prepareRemoteWriteBatch 按锁序取得并交给 Guard 作用域，所以 handler Cast 到它们时实体已被持有，直接复用。
// 在 handler 中途、已持本地实体锁且不知道调用方截止时间的情况下再取分布式锁，正是声明步骤要避免的锁序与超时
// 风险，所以未声明的 Remote 目标直接拒绝，不当场加锁。结果只有三种：
//   - 全部 Remote 托管目标都已被本次派发持有（或没有）：返回 nil，Cast 只走本地锁；
//   - 不在任何派发中出现未声明的 Remote 托管目标：返回说明缺少派发的错误；
//   - 派发中出现未声明的 Remote 托管目标：ErrRemoteWriteCapabilityDisabled（“must be declared before dispatch”）。
//
// 只有 Remote 托管 kind 受此限制（shouldPrepareRemoteID）：不由本服务托管的 Remote 能力 kind 在这里是普通本地实体。
// 将来若需要动态 Remote Cast，须为锁序与截止时间做专门设计，不能加 hook。
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
