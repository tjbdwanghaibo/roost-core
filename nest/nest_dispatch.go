package nest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// NestDispatch 为一条消息建立独立的请求上下文和 Guard，随后按路由加载并锁定实体。
// execution.go 负责业务执行与提交；这里负责最终释放、远端确认与回复。
func NestDispatch(mgr *NestMgr, msg *Msg) {
	dispatchNest(mgr, msg, false)
}

func dispatchNest(mgr *NestMgr, msg *Msg, remoteStage bool) {
	if msg == nil {
		return
	}
	if mgr == nil {
		recycleMsg(msg)
		return
	}
	msg.getter = mgr.getter
	if remoteStage {
		var executorMu sync.Mutex
		msg.localExecutor = func(fn func()) error {
			executorMu.Lock()
			defer executorMu.Unlock()
			_, err := mgr.dispatchFastContinuation(msg, fn)
			return err
		}
	}
	msg.stageMetrics = mgr.stageMetrics
	observeNestStage(msg.Name, "queue", msg.queuedAt)
	start := time.Now()
	traceWatch := startSlowDispatchTraceWatch(msg, start)
	releaseCtx := ensureNestContext(mgr, msg)
	releaseCurrentMsg := pushCurrentNestDispatchMsg(msg)
	var err error
	var ret any
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = errors.New(fmt.Sprint(r))
			}
			slog.Error("nest dispatch panic", "err", err)
		}
		if !remoteStage && errors.Is(err, errDeclaredTargetCold) && mgr.dispatcher.remoteHandler != nil {
			// 声明目标在 handler 取得 Guard 之前被发现未加载：此时没有业务修改、没有 Remote 批次，
			// 也没有已准备的引用。不回复、不重新准入，由派发队列把同一作业（连同它在同 ID 链上的
			// 位置）交给慢池准备；慢阶段再按普通 Slow 路径加载并回到快池执行（RR-20260926-25）。
			msg.slowReroute = true
			releaseCurrentMsg()
			releaseCtx()
			cost := time.Since(start)
			emitNestTraceEvent(msg, "dispatch_done", "slow_reroute", cost)
			traceWatch.stop(cost, "slow_reroute")
			return
		}
		commitCtx := context.Background()
		if current := fctx.CurrentContext(); current != nil && current.Base != nil {
			commitCtx = current.Base
		}
		remoteStart := startNestStage(mgr.stageMetrics && msg.RemoteWriteBatch != nil)
		if remoteErr := msg.finishRemoteWriteBatch(commitCtx, err); remoteErr != nil {
			err = errors.Join(err, fmt.Errorf("nest: finish remote write batch: %w", remoteErr))
		}
		observeNestStage(msg.Name, "remote_confirm", remoteStart)
		if msg.prepared != nil {
			err = errors.Join(err, msg.localExecutor(msg.prepared.release))
			msg.prepared = nil
		}
		releaseCurrentMsg()
		releaseCtx()
		if errors.Is(err, ErrEntityGroupTransitionScheduled) {
			err = nil
		}
		if requeuePendingEntityGroupTransition(mgr, msg, err) {
			err = nil
		}
		if requeueTransientDispatch(mgr, msg, err) {
			err = nil
		}
		if msg.deferredCompletion {
			// The completion pump owns the reply: it sends RetChan (or logs
			// the failure) once the commit ticket resolves. Sending here
			// would leak a result whose durability is not decided yet.
		} else if msg.RetChan != nil {
			if err != nil {
				msg.RetChan <- err
			} else {
				msg.RetChan <- ret
			}
		} else if err != nil {
			logAsyncDispatchError(msg, err)
		}
		cost := time.Since(start)
		mgr.recordDispatch(cost)
		emitNestTraceEvent(msg, "dispatch_done", dispatchResult(err), cost)
		traceWatch.stop(cost, dispatchResult(err))
		observeDispatch(msg, err, cost)
	}()

	emitNestTraceEvent(msg, "dispatch_start", "ok", 0)
	if fenced := mgr.FenceError(); fenced != nil {
		err = fenced
		return
	}
	if current := fctx.CurrentContext(); current != nil && current.Base != nil {
		select {
		case <-current.Base.Done():
			err = errors.Join(ErrNestCanceled, current.Base.Err())
			return
		default:
		}
	}

	if remoteStage {
		current := fctx.CurrentContext()
		current.Base = entity.WithLocalExecutor(current.Base, msg.localExecutor)
	}
	prepareStart := startNestStage(mgr.stageMetrics && remoteStage)
	defer func() { observeNestStage(msg.Name, "remote_prepare", prepareStart) }()
	if err = prepareRemoteSnapshots(msg, mgr.remoteSnapshotResolver, mgr.remoteManager); err != nil {
		return
	}

	// Prepare remote entities (acquire distributed lock + load from DB)
	if msg.HasRemote {
		if err = prepareRemoteWriteBatch(mgr.remoteManager, msg); err != nil {
			return
		}
	}

	observeNestStage(msg.Name, "remote_prepare", prepareStart)
	prepareStart = time.Time{}
	if remoteStage {
		if err = prepareSlowEntities(msg); err != nil {
			return
		}
		ret, err = mgr.dispatchRemoteLogic(msg)
	} else {
		ret, err = runNestLogic(mgr, msg)
	}
}

// runNestLogic 的 Guard 与事务上下文始终由执行 handler 的 goroutine 持有。
// Remote 的获取和最终确认在调用者的慢 worker 上完成。
func runNestLogic(mgr *NestMgr, msg *Msg) (ret any, err error) {
	previousGetter := msg.getter
	msg.getter = loadedGetter{previousGetter}
	defer func() { msg.getter = previousGetter }()

	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = errors.New(fmt.Sprint(r))
			}
		}
	}()
	_, releaseGuardScope := entity.NewGuardScope("nest:" + msg.Name)
	releaseCurrentMsg := pushCurrentNestDispatchMsg(msg)
	defer releaseCurrentMsg()
	defer func() {
		cleanupStart := startNestStage(mgr.stageMetrics)
		releaseGuardScope()
		msg.runAfterUnlock()
		observeNestStage(msg.Name, "cleanup", cleanupStart)
	}()
	if fenced := mgr.FenceError(); fenced != nil {
		return nil, fenced
	}
	if cause := nestBaseContext().Err(); cause != nil {
		return nil, errors.Join(ErrNestCanceled, cause)
	}
	switch msg.Type {
	case MsgTypeSingle:
		ret, err = mgr.singleDispatch(msg.Name, msg.Tid, msg.Params)
	case MsgTypeMulti:
		ret, err = mgr.multiDispatch(msg.Name, msg.Tids, msg.Params)
	case MsgTypeMultiGroup:
		ret, err = mgr.multiGroupDispatch(msg.Name, msg.GroupTIds, msg.Params)
	case MsgTypeBroadcast:
		mgr.broadcastDispatch(msg.Name, msg.Tids, msg.Params)
	case MsgTypeGroupTransition:
		ret, err = mgr.groupTransitionDispatch(msg.GroupTransition)
	}
	return ret, err
}

func ensureNestContext(mgr *NestMgr, msg *Msg) func() {
	meta := fctx.RequestMeta{Source: "nest"}
	if msg != nil {
		meta.Handler = msg.Name
	}
	frame := uint64(0)
	if mgr != nil && mgr.ticker != nil {
		frame = mgr.ticker.CurrentTick()
	}
	var opts []fctx.Option
	if msg != nil && msg.Context.Valid {
		opts = append(opts, fctx.WithSnapshot(msg.Context))
	}
	c, release := fctx.NewContext(opts...)
	c.MergeMeta(meta)
	c.Frame = frame
	return release
}

func nestBaseContext() context.Context {
	if current := fctx.CurrentContext(); current != nil && current.Base != nil {
		return current.Base
	}
	return context.Background()
}

func (mgr *NestMgr) singleDispatch(name string, id int64, params []any) (any, error) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return nil, ErrHandlerNotFound
	}
	fullID, err := entity.NormalizeFullID(id, entity.EntityKindNone)
	if err != nil {
		return nil, err
	}
	meta := entity.ResolveEntityID(fullID)
	loadStart := startNestStage(mgr.stageMetrics)
	e, err := mgr.dispatchGetter().Get(nestBaseContext(), meta.FullID, meta.Category)
	observeNestStage(name, "load", loadStart)
	if errors.Is(err, entity.ErrColdLoadInLogic) && beforeSlowPreparation() {
		// 准入时已加载、执行前被驱逐：handler 尚未开始，原位转慢准备（RR-20260926-25）。
		return nil, declaredTargetCold(err)
	}
	if err != nil {
		return nil, err
	}
	// The production getter reports "no such entity" as (nil, nil): the
	// manager missed and there was no loader, or the loader legitimately
	// found nothing. Touching that is a nil dereference, and what the caller
	// then sees is whatever the top-level recovery made of it rather than
	// ErrEntityNotFound — "this entity does not exist" and "the framework
	// broke" become the same answer (RR-20260919-09). multi and multiGroup in
	// this file already check; these two did not.
	if e == nil {
		return nil, ErrEntityNotFound
	}
	if !e.Touch() {
		if beforeSlowPreparation() {
			// 读取之后、引用之前被驱逐：对象已摘除不可再用；慢准备会重新加载或确认不存在。
			return nil, declaredTargetCold(ErrEntityNotFound)
		}
		return nil, ErrEntityNotFound
	}
	defer e.UnTouch()
	es := []entity.IThreadSafeEntity{e}
	return mgr.dispatchLoadedEntities(entry, name, es, es, params)
}

func (mgr *NestMgr) multiDispatch(name string, ids []int64, params []any) (any, error) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return nil, ErrHandlerNotFound
	}
	return mgr.dispatchMany(entry, name, ids, params)
}

func (mgr *NestMgr) multiGroupDispatch(name string, groups [][]int64, params []any) (any, error) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return nil, ErrHandlerNotFound
	}
	ids := make([]int64, 0)
	groupLen := make([]int, len(groups))
	for i, group := range groups {
		groupLen[i] = len(group)
		ids = append(ids, group...)
	}
	return mgr.dispatchMany(entry, name, ids, params, HandlerOptionWithGroup(groupLen))
}

// dispatchMany 保留调用者的实体顺序与 nil 占位，另建锁集合用于排序。
// Touch 记录单独保存，业务修改参数切片也不会影响引用归还；重复参数按次数配对。
func (mgr *NestMgr) dispatchMany(entry handlerEntry, name string, ids []int64, params []any, opts ...HandlerOption) (any, error) {
	fullIDs, categories, err := normalizeFullIDs(ids)
	if err != nil {
		return nil, err
	}
	loadStart := startNestStage(mgr.stageMetrics)
	es, err := mgr.dispatchGetter().GetMany(nestBaseContext(), fullIDs, categories)
	observeNestStage(name, "load", loadStart)
	if errors.Is(err, entity.ErrColdLoadInLogic) && beforeSlowPreparation() {
		return nil, declaredTargetCold(err)
	}
	if err != nil {
		return nil, err
	}
	lockEs := make([]entity.IThreadSafeEntity, 0, len(es))
	touchedEs := make([]entity.IThreadSafeEntity, 0, len(es))
	evicted := false
	for i, e := range es {
		if e != nil && e.Touch() {
			lockEs = append(lockEs, e)
			touchedEs = append(touchedEs, e)
		} else {
			evicted = evicted || e != nil
			es[i] = nil
		}
	}
	defer func() {
		for _, e := range touchedEs {
			e.UnTouch()
		}
	}()
	if evicted && beforeSlowPreparation() {
		// 声明目标在读取与引用之间被驱逐：可选目标也不能静默当成缺失，交给慢准备重新判定。
		return nil, declaredTargetCold(ErrEntityNotFound)
	}
	if firstDispatchEntityMissing(es) {
		return nil, ErrEntityNotFound
	}
	return mgr.dispatchLoadedEntities(entry, name, es, lockEs, params, opts...)
}

// dispatchLoadedEntities 统一普通调用的组迁移检查、排序加锁和执行收尾。
// 调用方负责配对 Touch/UnTouch；这里先释放锁，再返回调用方归还引用。
// es 是业务参数，lockEs 是已 Touch 的锁集合；多实体时两者不可共用底层数组。
func (mgr *NestMgr) dispatchLoadedEntities(entry handlerEntry, name string, es, lockEs []entity.IThreadSafeEntity, params []any, opts ...HandlerOption) (any, error) {
	if err := rejectPendingEntityGroupTransition(lockEs); err != nil {
		return nil, err
	}
	SortEntity(lockEs)
	guard := entity.GetEntityGuard()
	lockStart := startNestStage(mgr.stageMetrics)
	_, releaseLocks, err := lockDispatchEntitiesForHandlerWithStore(mgr.groupLockManager(), guard, lockEs, groupStoreOf(mgr.getter))
	observeNestStage(name, "lock", lockStart)
	if err != nil {
		if errors.Is(err, ErrLockTimeout) && beforeSlowPreparation() && slices.ContainsFunc(lockEs, entity.IThreadSafeEntity.IsRemoved) {
			// 引用之后、取锁之前被驱逐（RequireEntity 拒绝已摘除实体）：handler 仍未开始，
			// 原位转慢准备，不走会排到同 ID 后继之后的延迟重新准入。
			return nil, declaredTargetCold(err)
		}
		return nil, err
	}
	if mgr.stageMetrics {
		originalRelease := releaseLocks
		releaseLocks = func() {
			defer observeNestStage(name, "release", time.Now())
			originalRelease()
		}
	}
	// 提前释放的异常由提交路径接管；defer 兜底不能再次抛出同一个 panic。
	// Once.Do 只执行一次，OnceFunc 会在后续调用中重放第一次的 panic。
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(releaseLocks) }
	defer release()
	return mgr.invokeHandlerTransaction(entry.meta, es, name, release, func() (any, error) {
		return entry.handler(es, params, opts...)
	})
}

func firstDispatchEntityMissing(es []entity.IThreadSafeEntity) bool {
	return len(es) == 0 || es[0] == nil
}

func lockDispatchEntities(guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity) ([]entity.IThreadSafeEntity, error) {
	if guard == nil {
		return nil, ErrLockTimeout
	}
	useTryLock := guard.GuardedCount() > 0 && !guard.CheckContainAllLock(lockEs)
	acquired := make([]entity.IThreadSafeEntity, 0, len(lockEs))
	for _, e := range lockEs {
		if e == nil {
			continue
		}
		if guard.Guarded(e.GUId()) {
			continue
		}
		ok := false
		if useTryLock {
			ok = tryRequireDispatchEntity(guard, e)
		} else {
			ok = guard.RequireEntity(e)
		}
		if !ok {
			releaseDispatchEntities(guard, acquired)
			return nil, ErrLockTimeout
		}
		acquired = append(acquired, e)
	}
	return acquired, nil
}

func tryRequireDispatchEntity(guard *entity.EntityGuard, ent entity.IThreadSafeEntity) bool {
	if guard == nil || ent == nil {
		return false
	}
	gID := ent.GUId()
	mu := ent.GetMutex()
	if gID == 0 || mu == nil {
		return false
	}
	if guard.Guarded(gID) {
		return true
	}
	if !mu.TryLock() {
		return false
	}
	if ent.IsClear() || ent.IsRemoved() {
		mu.Unlock()
		return false
	}
	guard.GuardEntity(ent)
	return true
}

func releaseDispatchLocks(guard *entity.EntityGuard, acquired []entity.IThreadSafeEntity) {
	if guard == nil {
		return
	}
	if entity.CurrentGuardScope() == nil {
		entity.EntityGuardRelease(guard)
		return
	}
	releaseDispatchEntities(guard, acquired)
}

func releaseDispatchEntities(guard *entity.EntityGuard, acquired []entity.IThreadSafeEntity) {
	if guard == nil {
		return
	}
	for i := len(acquired) - 1; i >= 0; i-- {
		if acquired[i] != nil {
			guard.ReleaseEntity(acquired[i].GUId())
		}
	}
}

func (mgr *NestMgr) broadcastDispatch(name string, ids []int64, params []any) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return
	}
	guard := entity.GetEntityGuard()
	defer entity.EntityGuardRelease(guard)

	oneEntity := make([]entity.IThreadSafeEntity, 1)
	for _, id := range ids {
		fullID, err := entity.NormalizeFullID(id, entity.EntityKindNone)
		if err != nil {
			continue
		}
		meta := entity.ResolveEntityID(fullID)
		loadStart := startNestStage(mgr.stageMetrics)
		e, err := func() (value entity.IThreadSafeEntity, err error) {
			// 加载约束 panic 也属于这个广播目标；记录原因并继续独立的后续目标。
			defer func() {
				if r := recover(); r != nil {
					if cause, ok := r.(error); ok {
						err = cause
					} else {
						err = fmt.Errorf("getter panic: %v", r)
					}
				}
			}()
			return mgr.dispatchGetter().Get(nestBaseContext(), meta.FullID, meta.Category)
		}()
		observeNestStage(name, "load", loadStart)
		if err != nil {
			slog.Error("nest broadcast target load failed", "id", meta.FullID, "handler", name, "err", err)
			continue
		}
		// Skip, do not dereference: the per-entity recovery below starts
		// AFTER Touch, so a nil here escaped to the whole dispatch's recovery
		// and every entity after this one was silently dropped
		// (RR-20260919-09).
		if e == nil || !e.Touch() {
			continue
		}
		lockStart := startNestStage(mgr.stageMetrics)
		locked := guard.RequireEntity(e)
		observeNestStage(name, "lock", lockStart)
		if !locked {
			e.UnTouch()
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("nest broadcast handler panic", "id", meta.FullID, "handler", name, "err", r)
				}
			}()
			// 清理各自独立：释放 hook panic 也必须归还 Touch，并留在逐实体恢复边界内。
			defer func() { oneEntity[0] = nil }()
			defer e.UnTouch()
			defer func() {
				defer observeNestStage(name, "release", startNestStage(mgr.stageMetrics))
				guard.ReleaseEntity(e.GUId())
			}()
			oneEntity[0] = e
			// Broadcast has no early-release closure: pipelined handlers run
			// with strict in-lock commit semantics on this path.
			if _, err := mgr.invokeHandlerTransaction(entry.meta, oneEntity, name, nil, func() (any, error) {
				return entry.handler(oneEntity, params)
			}); err != nil {
				slog.Debug("nest broadcast handler failed", "id", meta.FullID, "handler", name, "err", err)
			}
		}()
	}
}

func normalizeFullIDs(ids []int64) ([]int64, []entity.EntityCategory, error) {
	fullIDs := make([]int64, len(ids))
	fullIDCategories := make([]entity.EntityCategory, len(ids))
	for i, id := range ids {
		fullID, err := entity.NormalizeFullID(id, entity.EntityKindNone)
		if err != nil {
			return nil, nil, err
		}
		meta := entity.ResolveEntityID(fullID)
		fullIDs[i] = meta.FullID
		fullIDCategories[i] = meta.Category
	}
	return fullIDs, fullIDCategories, nil
}

// SortEntity sorts entities for deadlock-free lock acquisition.
func SortEntity(es []entity.IThreadSafeEntity) {
	slices.SortFunc(es, func(a, b entity.IThreadSafeEntity) int {
		return compareEntityIDs(a.GUId(), b.GUId())
	})
}

func SortEntityId(guids []int64) {
	slices.SortFunc(guids, compareEntityIDs)
}

func compareEntityIDs(guidI, guidJ int64) int {
	groupI, groupJ := entity.GetEntityGroup(guidI), entity.GetEntityGroup(guidJ)
	if groupI != groupJ {
		return cmp.Compare(groupI, groupJ)
	}
	return cmp.Compare(guidI, guidJ)
}

// prepareRemoteWriteBatch admits one fenced transaction before dispatch.
func prepareRemoteWriteBatch(manager entity.IRemoteEntityManager, msg *Msg) error {
	ids := extractRemoteIds(msg)
	if len(ids) == 0 {
		return nil
	}
	// A remote write batch is one transaction. Broadcast invokes one
	// transaction per entity and therefore cannot provide a matching atomic
	// commit/rollback boundary for the batch.
	if msg.Type == MsgTypeBroadcast {
		return ErrRemoteBroadcastUnsupported
	}
	prepareCtx := context.Background()
	if current := fctx.CurrentContext(); current != nil && current.Base != nil {
		prepareCtx = current.Base
	}
	if manager == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	batch, err := manager.PrepareRemoteWriteBatch(prepareCtx, ids)
	if err != nil {
		return err
	}
	if batch == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	msg.setRemoteWriteBatch(batch)
	return nil
}

// extractRemoteIds collects entity IDs whose ID remote bit and entity kind
// indicate remote-managed lifecycle.
func extractRemoteIds(msg *Msg) []int64 {
	var ids []int64
	if msg.Tid != 0 {
		meta := entity.ResolveEntityID(msg.Tid)
		if shouldPrepareRemoteID(meta) {
			ids = append(ids, meta.FullID)
		}
	}
	for _, id := range msg.Tids {
		meta := entity.ResolveEntityID(id)
		if shouldPrepareRemoteID(meta) {
			ids = append(ids, meta.FullID)
		}
	}
	for _, group := range msg.GroupTIds {
		for _, id := range group {
			meta := entity.ResolveEntityID(id)
			if shouldPrepareRemoteID(meta) {
				ids = append(ids, meta.FullID)
			}
		}
	}
	return ids
}
