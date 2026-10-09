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

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
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
	msg.engine = mgr
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
			err = joinRecoveredError(err, r)
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
		if msg.replyAfterCommit() && err != nil && !errors.Is(err, ErrAfterCommitFailed) {
			// 事务已提交（纯本地：本地提交成功，RR-20260926-53；Remote：本地与 Remote 都已提交，RR-20260926-46）：
			// release hook、Close、预加载引用释放等错误只说明收尾失败，调用方必须能用
			// errors.Is(ErrAfterCommitFailed) 区分“已提交”，不能据此重复业务。
			err = fmt.Errorf("%w: %w", ErrAfterCommitFailed, err)
		}
		if msg.remotePartRejected && err != nil && !errors.Is(err, ErrRemotePartRejected) {
			// 本地事务已提交，Remote 部分被明确拒绝（只丢弃 Remote 部分，RR-20260926-58）：调用方必须能区分“部分已提交”，
			// 不能整笔重试；之前回复只有 Remote 的原因错误，判别表一行都不命中（RR-20260928-03）。与上面的 ErrAfterCommitFailed
			// （Remote 也已确认）互斥；与 Remote 结果未知（错误链上的 entity.ErrRemotePersistenceIndeterminate）互斥。
			err = fmt.Errorf("%w: %w", ErrRemotePartRejected, err)
		}
		if msg.nestedTxCommitted && err != nil && !errors.Is(err, ErrAfterCommitFailed) && !errors.Is(err, ErrNestedTransactionCommitted) {
			// 自己的事务没有提交，但 handler 内嵌套的独立事务已持久提交（或结果未知）：调用方必须能区分
			// “什么都没提交”与“部分已提交”，不能据此重试整笔业务；消息也不重排（RR-20260926-65）。
			// 带 Remote 批次的消息自己的本地事务已提交（Remote 未确认：结果未知或被拒绝）时，外层换成不说“消息失败”的表述，
			// 哨兵语义不变（RR-20260928-08，OPEN-ITEMS B19 并存形态）。
			nested := ErrNestedTransactionCommitted
			if msg.remoteCommitted {
				nested = errNestedTransactionAlsoCommitted
			}
			err = fmt.Errorf("%w: %w", nested, err)
		}
		releaseCurrentMsg()
		releaseCtx()
		if errors.Is(err, ErrEntityGroupTransitionScheduled) {
			err = nil
		}
		// 锁超时 / 组迁移等暂时性错误只对尚未提交的消息重新准入；已越过提交点的事务回复原错误（RR-20260926-49）。
		// 不能回滚的 handler 一旦开始执行，已做的修改不撤销，同样不重排（新建实体冲突 RR-20260926-64，其余 RR-20260926-73）。
		if msg.requeueAllowed(err) {
			if requeuePendingEntityGroupTransition(mgr, msg, err) {
				err = nil
			}
			if requeueTransientDispatch(mgr, msg, err) {
				err = nil
			}
		} else {
			if msg.createLockConflictNoRollback && isRequeueableDispatchError(err) && !errors.Is(err, ErrCreatedEntityLockConflict) {
				// 业务换成了别的锁超时类错误回复：补上冲突哨兵，调用方仍能判别“框架没有重排、修改未回滚”。
				err = fmt.Errorf("%w: %w", ErrCreatedEntityLockConflict, err)
			}
			if msg.noRollbackHandlerStarted && !msg.transactionPastCommitPoint(err) && isRequeueableDispatchError(err) && !errors.Is(err, ErrNonRollbackNotRequeued) {
				// 本该重排的暂时性错误因 handler 不能回滚而没有重排：调用方据此判别“框架没有重排、修改未回滚”，
				// 不能按 ErrLockTimeout 的“未执行”语义重发（RR-20260926-73）。已越过提交点的由提交哨兵说明，不叠加。
				err = fmt.Errorf("%w: %w", ErrNonRollbackNotRequeued, err)
			}
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
			// 收尾（Guard 作用域释放、解锁后回调）里的 panic 到这里时 err 已经是 handler 的结果：并进去，不覆盖（RR-20260930-20）。
			err = joinRecoveredError(err, r)
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
	if errors.Is(err, entity.ErrColdLoadInLogic) && mgr.beforeSlowPreparation() {
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
		if mgr.beforeSlowPreparation() {
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
	if errors.Is(err, entity.ErrColdLoadInLogic) && mgr.beforeSlowPreparation() {
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
	if evicted && mgr.beforeSlowPreparation() {
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
func (mgr *NestMgr) dispatchLoadedEntities(entry handlerEntry, name string, es, lockEs []entity.IThreadSafeEntity, params []any, opts ...HandlerOption) (ret any, err error) {
	if err := rejectPendingEntityGroupTransition(lockEs); err != nil {
		return nil, err
	}
	SortEntity(lockEs)
	guard, err := dispatchScopeGuard()
	if err != nil {
		return nil, err
	}
	lockStart := startNestStage(mgr.stageMetrics)
	_, releaseLocks, err := lockDispatchEntitiesForHandlerWithStore(mgr.groupLockManager(), guard, lockEs, groupStoreOf(mgr.getter))
	observeNestStage(name, "lock", lockStart)
	if err != nil {
		if errors.Is(err, ErrLockTimeout) && mgr.beforeSlowPreparation() && slices.ContainsFunc(lockEs, entity.IThreadSafeEntity.IsRemoved) {
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
	defer func() {
		// 兜底释放在自己的 recover 边界里跑：release hook 的 panic 并进已在途的返回值，而不是把它整个抛掉。
		// 之前 `defer release()` 直接 panic，handler 返回的业务错误随返回值一起丢失，runNestLogic 的 recover 再把 err 换成
		// hook 错误，回滚后的回复只剩 `release hook failed`，调用方无法判别（RR-20260930-20）。已提交路径不受影响：
		// 那时 err 为 nil，回复仍是 hook 错误本身，再由 dispatchNest 按提交事实包 ErrAfterCommitFailed（RR-20260926-53）。
		// 只包住这一次释放：业务阶段沿函数体传出的 panic 不在这里 recover，仍交给 runNestLogic。
		if r := recoverRelease(release); r != nil {
			err = joinRecoveredError(err, r)
			slog.Error("nest entity release panic", "handler", name, "err", err)
		}
	}()
	return mgr.invokeHandlerTransaction(entry.meta, es, name, release, func() (any, error) {
		return entry.handler(es, params, opts...)
	})
}

// recoverRelease 执行 release 并返回它 panic 的值（没有 panic 返回 nil）。
func recoverRelease(release func()) (recovered any) {
	defer func() { recovered = recover() }()
	release()
	return nil
}

// joinRecoveredError 把 recover 到的 panic 值并进在途的错误：err 为 nil 时结果就是 panic 的错误本身（文本与旧行为一致）；
// err 非 nil 时两者以 errors.Join 并列，errors.Is 对业务错误与 panic 原因都成立（RR-20260930-20）。
// 之前的 recover 一律 `err = recoveredErr`，收尾阶段的 panic 会把 handler 已经返回的错误整个覆盖。
func joinRecoveredError(err error, r any) error {
	recovered, ok := r.(error)
	if !ok {
		recovered = errors.New(fmt.Sprint(r))
	}
	if err == nil {
		return recovered
	}
	return errors.Join(err, recovered)
}

func firstDispatchEntityMissing(es []entity.IThreadSafeEntity) bool {
	return len(es) == 0 || es[0] == nil
}

func lockDispatchEntities(guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity) ([]entity.IThreadSafeEntity, error) {
	if guard == nil {
		return nil, ErrLockTimeout
	}
	// useTryLock（guard 已持有别的实体时改用 TryRequire，避免越锁序阻塞等待）在当前生产路径上不可达（OPEN-ITEMS B05，
	// 2026-09-27 按源码核对）：本函数唯一调用方是 group_lock.go 的 lockDispatchEntitiesWithGroup（groupID==0），往上只有
	// dispatchLoadedEntities ← singleDispatch / dispatchMany ← runNestLogic（快池派发与 remote_dispatch.go 的快续行都经它）。
	// runNestLogic 在取锁前新建 GuardScope，dispatchScopeGuard 取到的是池里已清空的 guard；其间的 Getter 只读已加载实体、不取实体锁，
	// 所以这里 GuardedCount() 恒为 0。handler 内的公开 Request / Dispatch 被 ErrSyncInHandler / ErrAsyncInHandler 拒绝，
	// 也没有别的嵌套派发入口。分支保留为防御：将来出现“在已持锁 guard 上派发”的入口时，它避免越锁序等待。
	useTryLock := guard.GuardedCount() > 0 && !guard.CheckContainAllLock(lockEs)
	acquired := make([]entity.IThreadSafeEntity, 0, len(lockEs))
	for _, e := range lockEs {
		if e == nil {
			continue
		}
		if guard.GuardedEntity(e) {
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
	if guard == nil {
		return false
	}
	return guard.TryRequireEntity(ent)
}

// releaseDispatchEntities 逆序放掉本次派发取得的实体锁。只放 acquired：Guard 本身和它上面的其他持有（handler 新建的实体、
// 被取代实例、解锁后回调）由 Guard 作用域结束时统一释放，Guard 也只在那时归还池（RR-20261006-12）。
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

// broadcastDispatch 对每个目标各执行一次 handler（每个目标一个事务）。每个目标在自己的 Guard 作用域里取锁、执行、释放
// （RR-20260930-14）：目标结束时它取得的全部锁——声明的目标实例、handler 内 Destroy 后同 ID 重建的新实例（旧实例转入
// superseded）、Cast 取得的实体——连同 post-release 回调一起释放，不跨后续目标持有。之前整轮广播共用 runNestLogic 的
// 一把 Guard、目标结束时按 ID 释放：Destroy 后同 ID 重建时按 ID 放掉的是新实例的锁，旧实例回到 eMap，它的锁一直持有到
// 整轮广播结束（RR-20260927-26 §未验证项，REMAINING §3 N28）。
func (mgr *NestMgr) broadcastDispatch(name string, ids []int64, params []any) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return
	}

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
		func() {
			// 目标自己的 Guard 作用域：handler 看到的 CurrentGuardScope 就是它；作用域结束释放本目标取得的全部锁与
			// post-release 回调，与单目标派发的边界一致。
			scope, releaseScope := entity.NewGuardScope("nest:broadcast:" + name)
			defer releaseScope()
			guard := scope.Guard()
			lockStart := startNestStage(mgr.stageMetrics)
			locked := guard.RequireEntity(e)
			observeNestStage(name, "lock", lockStart)
			if !locked {
				e.UnTouch()
				return
			}
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
				// 按实例释放本目标的实例（被同 ID 新实例取代时只放旧锁）；新实例等其余锁随作用域结束一起释放。
				guard.ReleaseEntityInstance(e)
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
