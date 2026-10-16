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

// dispatchNest 执行一条消息的一个阶段。remoteStage 为 true 时是 I/O 池上的准备阶段（Remote 获取、冷加载），
// 业务部分经 dispatchRemoteLogic 交回业务池；为 false 时直接在业务池执行 runNestLogic。
//
// 收尾顺序（defer）：声明目标冷则原位转 I/O 准备并直接返回；否则收尾 Remote 批次与预加载引用 →
// 按提交事实给错误补哨兵 → 释放上下文 → 判定是否重新准入 → 交接 await 计划 → 回复 → 记录指标。
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
			_, err := mgr.dispatchBusinessContinuation(msg, fn)
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
			// 位置）交给 I/O 池准备；I/O 阶段再按普通 IO 路径加载并回到业务池执行。
			msg.ioReroute = true
			releaseCurrentMsg()
			releaseCtx()
			cost := time.Since(start)
			emitNestTraceEvent(msg, "dispatch_done", "io_reroute", cost)
			traceWatch.stop(cost, "io_reroute")
			return
		}
		err = finishDispatchResources(mgr, msg, err)
		err = annotateCommitOutcome(msg, err)
		releaseCurrentMsg()
		releaseCtx()
		err = requeueOrAnnotateNotRequeued(mgr, msg, err)
		handOverAwaitPlan(msg, err)
		replyDispatch(msg, ret, err)
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

	// 准备 Remote 实体（取得分布式写权限并从库加载）。
	if msg.HasRemote {
		if err = prepareRemoteWriteBatch(mgr.remoteManager, msg); err != nil {
			return
		}
	}

	observeNestStage(msg.Name, "remote_prepare", prepareStart)
	prepareStart = time.Time{}
	if remoteStage {
		if err = prepareIOEntities(msg); err != nil {
			return
		}
		ret, err = mgr.dispatchRemoteLogic(msg)
	} else {
		ret, err = runNestLogic(mgr, msg)
	}
}

// finishDispatchResources 收尾 Remote 写批次（按 err 与提交事实 Commit / Abort），再经本地执行器释放
// I/O 阶段预加载的引用；收尾错误并入 err。必须在释放请求上下文之前调用（Remote 收尾使用其 Base ctx）。
func finishDispatchResources(mgr *NestMgr, msg *Msg, err error) error {
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
	return err
}

// annotateCommitOutcome 按消息上记录的提交事实给失败回复补哨兵，让调用方区分“已提交 / 部分已提交 / 未提交”。
// 三者都只在 err 非 nil 且尚未带对应哨兵时补。
func annotateCommitOutcome(msg *Msg, err error) error {
	if err == nil {
		return nil
	}
	if msg.replyAfterCommit() && !errors.Is(err, ErrAfterCommitFailed) {
		// 事务已提交（纯本地：本地提交成功；Remote：本地与 Remote 都已提交）：release hook、Close、预加载引用
		// 释放等错误只说明收尾失败，调用方必须能用 errors.Is(ErrAfterCommitFailed) 区分“已提交”，不能据此重复业务。
		err = fmt.Errorf("%w: %w", ErrAfterCommitFailed, err)
	}
	if msg.remotePartRejected && !errors.Is(err, ErrRemotePartRejected) {
		// 本地事务已提交，Remote 部分被明确拒绝（只丢弃 Remote 部分）：调用方必须能区分“部分已提交”，不能整笔重试。
		// 与上面的 ErrAfterCommitFailed（Remote 也已确认）互斥；与 Remote 结果未知（错误链上的
		// entity.ErrRemotePersistenceIndeterminate）互斥。
		err = fmt.Errorf("%w: %w", ErrRemotePartRejected, err)
	}
	if msg.nestedTxCommitted && !errors.Is(err, ErrAfterCommitFailed) && !errors.Is(err, ErrNestedTransactionCommitted) {
		// 自己的事务没有提交，但 handler 内嵌套的独立事务已持久提交（或结果未知）：调用方必须能区分
		// “什么都没提交”与“部分已提交”，不能据此重试整笔业务；消息也不重排。
		// 带 Remote 批次的消息自己的本地事务已提交（Remote 未确认：结果未知或被拒绝）时，外层换成不说“消息失败”的表述，
		// 哨兵语义不变。
		nested := ErrNestedTransactionCommitted
		if msg.remoteCommitted {
			nested = errNestedTransactionAlsoCommitted
		}
		err = fmt.Errorf("%w: %w", nested, err)
	}
	return err
}

// requeueOrAnnotateNotRequeued 决定失败消息是否由框架重新准入；已重排时返回 nil（不回复）。
//
// 锁超时 / 组迁移等暂时性错误只对尚未越过提交点的消息重新准入；已越过提交点的事务回复原错误。不能回滚的 handler
// 一旦开始执行，已做的修改不撤销，同样不重排，此时给回复补上说明“框架没有重排、修改未回滚”的哨兵。
func requeueOrAnnotateNotRequeued(mgr *NestMgr, msg *Msg, err error) error {
	if errors.Is(err, ErrEntityGroupTransitionScheduled) {
		err = nil
	}
	if msg.requeueAllowed(err) {
		if requeuePendingEntityGroupTransition(mgr, msg, err) {
			err = nil
		}
		if requeueTransientDispatch(mgr, msg, err) {
			err = nil
		}
		return err
	}
	if msg.createLockConflictNoRollback && isRequeueableDispatchError(err) && !errors.Is(err, ErrCreatedEntityLockConflict) {
		// 业务换成了别的锁超时类错误回复：补上冲突哨兵，调用方仍能判别“框架没有重排、修改未回滚”。
		err = fmt.Errorf("%w: %w", ErrCreatedEntityLockConflict, err)
	}
	if msg.noRollbackHandlerStarted && !msg.transactionPastCommitPoint(err) && isRequeueableDispatchError(err) && !errors.Is(err, ErrNonRollbackNotRequeued) {
		// 本该重排的暂时性错误因 handler 不能回滚而没有重排：调用方不能按 ErrLockTimeout 的“未执行”语义重发。
		// 已越过提交点的由提交哨兵说明，不叠加。
		err = fmt.Errorf("%w: %w", ErrNonRollbackNotRequeued, err)
	}
	return err
}

// handOverAwaitPlan 处理本段登记的 await 计划：失败则取消；成功则把计划的启动挂到出队之后，并把最终回复的
// 所有权转交给计划（当前段不回复）。
func handOverAwaitPlan(msg *Msg, err error) {
	if msg.awaitPlan == nil {
		return
	}
	plan := msg.awaitPlan
	msg.awaitPlan = nil
	if err != nil {
		plan.cancel()
		return
	}
	msg.afterQueue = plan.start
	msg.RetChan = nil // 最终回复已经转交给 plan，当前段不提前回复。
}

// replyDispatch 回复同步调用方，或记录异步消息的失败。回复已由完成泵接管（pipelined 延迟完成）时不在这里发送：
// 完成泵在提交票据有结论后回复，这里发送会泄漏一个持久性尚未确定的结果。
func replyDispatch(msg *Msg, ret any, err error) {
	switch {
	case msg.deferredCompletion:
	case msg.RetChan != nil:
		if err != nil {
			msg.RetChan <- err
		} else {
			msg.RetChan <- ret
		}
	case err != nil:
		logAsyncDispatchError(msg, err)
	}
}

// runNestLogic 在业务池上建立 Guard 作用域并按消息类型派发。Guard 与事务上下文始终由执行 handler 的 goroutine 持有；
// Remote 的获取和最终确认在调用者的 I/O worker 上完成。
func runNestLogic(mgr *NestMgr, msg *Msg) (ret any, err error) {
	previousGetter := msg.getter
	msg.getter = loadedGetter{previousGetter}
	defer func() { msg.getter = previousGetter }()

	defer func() {
		if r := recover(); r != nil {
			// 收尾（Guard 作用域释放、解锁后回调）里的 panic 到这里时 err 已经是 handler 的结果：并进去，不覆盖。
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
	if msg.resumeEntry != nil {
		return mgr.dispatchMany(*msg.resumeEntry, msg.Name, msg.Tids, nil)
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

// ensureNestContext 为消息建立 fctx 请求上下文（带消息快照、handler 元信息与当前帧号），返回释放函数。
func ensureNestContext(mgr *NestMgr, msg *Msg) func() {
	meta := fctx.RequestMeta{Source: "nest"}
	if msg != nil {
		meta.Handler = msg.Name
	}
	frame := uint64(0)
	if mgr != nil && mgr.ticker != nil {
		frame = mgr.ticker.CurrentTick()
	}
	var c *fctx.Context
	var release func()
	if msg != nil && msg.Context.Valid {
		// 直接传递选项，避免为每条消息构造可增长切片并让快照闭包逃逸。
		c, release = fctx.NewContext(fctx.WithSnapshot(msg.Context))
	} else {
		c, release = fctx.NewContext()
	}
	c.MergeMeta(meta)
	c.Frame = frame
	return release
}

// nestBaseContext 返回当前请求上下文的 Base ctx；没有时为 Background。
func nestBaseContext() context.Context {
	if current := fctx.CurrentContext(); current != nil && current.Base != nil {
		return current.Base
	}
	return context.Background()
}

// singleDispatch 派发单目标消息：取已加载实体、Touch 后进入统一的加锁与执行。
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
	if errors.Is(err, entity.ErrColdLoadInLogic) && mgr.beforeIOPreparation() {
		// 准入时已加载、执行前被驱逐：handler 尚未开始，原位转 I/O 准备。
		return nil, declaredTargetCold(err)
	}
	if err != nil {
		return nil, targetLoadError(err)
	}
	// 生产 Getter 以 (nil, nil) 表示“实体不存在”（未命中且没有 loader，或 loader 确认不存在），必须回复
	// ErrEntityNotFound，不能解引用 nil 让顶层恢复把“不存在”和“框架出错”变成同一个回答。
	if e == nil {
		return nil, ErrEntityNotFound
	}
	if !e.Touch() {
		if mgr.beforeIOPreparation() {
			// 读取之后、引用之前被驱逐：对象已摘除不可再用；I/O 准备会重新加载或确认不存在。
			return nil, declaredTargetCold(ErrEntityNotFound)
		}
		return nil, ErrEntityNotFound
	}
	defer e.UnTouch()
	es := []entity.IThreadSafeEntity{e}
	return mgr.dispatchLoadedEntities(entry, name, es, es, params)
}

// multiDispatch 派发多目标消息。
func (mgr *NestMgr) multiDispatch(name string, ids []int64, params []any) (any, error) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return nil, ErrHandlerNotFound
	}
	return mgr.dispatchMany(entry, name, ids, params)
}

// multiGroupDispatch 派发分组多目标消息：展开为一个 ID 列表，组边界经 HandlerOptionWithGroup 交给 handler。
func (mgr *NestMgr) multiGroupDispatch(name string, groups [][]int64, params []any) (any, error) {
	entry, ok := mgr.getHandlerEntry(NewHandlerName(name))
	if !ok || entry.handler == nil {
		return nil, ErrHandlerNotFound
	}
	groupLen := make([]int, len(groups))
	total := 0
	for i, group := range groups {
		groupLen[i] = len(group)
		total += len(group)
	}
	// 外部 Msg 的各组不一定连续；只预分配展开容量，保留顺序、重复项与组边界。
	ids := make([]int64, 0, total)
	for _, group := range groups {
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
	if errors.Is(err, entity.ErrColdLoadInLogic) && mgr.beforeIOPreparation() {
		return nil, declaredTargetCold(err)
	}
	if err != nil {
		return nil, targetLoadError(err)
	}
	// 锁集合会排序，Touch 集合保留归还次数；共用一次分配，但容量边界互不重叠。
	storage := make([]entity.IThreadSafeEntity, 2*len(es))
	lockEs := storage[:0:len(es)]
	touchedEs := storage[len(es):len(es):len(storage)]
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
	if evicted && mgr.beforeIOPreparation() {
		// 声明目标在读取与引用之间被驱逐：可选目标也不能静默当成缺失，交给 I/O 准备重新判定。
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
		if errors.Is(err, ErrLockTimeout) && mgr.beforeIOPreparation() && slices.ContainsFunc(lockEs, entity.IThreadSafeEntity.IsRemoved) {
			// 引用之后、取锁之前被驱逐（RequireEntity 拒绝已摘除实体）：handler 仍未开始，
			// 原位转 I/O 准备，不走会排到同 ID 后继之后的延迟重新准入。
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
		// 兜底释放在自己的 recover 边界里跑：release hook 的 panic 并进已在途的返回值，而不是把它整个抛掉，
		// 否则 handler 返回的业务错误会丢失，回滚后的回复只剩 hook 错误。已提交路径不受影响：那时 err 为 nil，
		// 回复是 hook 错误本身，再由 dispatchNest 按提交事实包 ErrAfterCommitFailed。
		// 只包住这一次释放：业务阶段沿函数体传出的 panic 不在这里 recover，仍交给 runNestLogic。
		if r := recoverRelease(release); r != nil {
			err = joinRecoveredError(err, r)
			slog.Error("nest entity release panic", "handler", name, "err", err)
		}
	}()
	return mgr.invokeHandlerTransaction(entry.meta, es, name, release, func() (any, error) {
		msg := currentNestDispatchMsg()
		if msg != nil {
			msg.handlerEntities = es
			defer func() { msg.handlerEntities = nil }()
		}
		return entry.handler(es, params, opts...)
	})
}

// recoverRelease 执行 release 并返回它 panic 的值（没有 panic 返回 nil）。
func recoverRelease(release func()) (recovered any) {
	defer func() { recovered = recover() }()
	release()
	return nil
}

// joinRecoveredError 把 recover 到的 panic 值并进在途的错误：err 为 nil 时结果就是 panic 的错误本身；
// err 非 nil 时两者以 errors.Join 并列，errors.Is 对业务错误与 panic 原因都成立，收尾阶段的 panic 不会覆盖
// handler 已经返回的错误。
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

// firstDispatchEntityMissing 报告主目标（第一个声明目标）是否缺失；其余目标允许为 nil 占位。
func firstDispatchEntityMissing(es []entity.IThreadSafeEntity) bool {
	return len(es) == 0 || es[0] == nil
}

// lockDispatchEntities 在无锁组时按已排序的 lockEs 依次取实体锁（已持有的跳过），任何一把取不到就放掉本次已取的锁
// 并返回 ErrLockTimeout。
func lockDispatchEntities(guard *entity.EntityGuard, lockEs []entity.IThreadSafeEntity) ([]entity.IThreadSafeEntity, error) {
	if guard == nil {
		return nil, ErrLockTimeout
	}
	// useTryLock（guard 已持有别的实体时改用 TryRequire，避免越锁序阻塞等待）在当前生产路径上不可达（2026-09-27 按源码
	// 核对）：本函数唯一调用方是 group_lock.go 的 lockDispatchEntitiesWithGroup（groupID==0），往上只有
	// dispatchLoadedEntities ← singleDispatch / dispatchMany ← runNestLogic（业务池派发与 remote_dispatch.go 的业务续行都经它）。
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
// 被取代实例、解锁后回调）由 Guard 作用域结束时统一释放，Guard 也只在那时归还池。
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

// broadcastDispatch 对每个目标各执行一次 handler（每个目标一个事务）。每个目标在自己的 Guard 作用域里取锁、执行、释放：
// 目标结束时它取得的全部锁——声明的目标实例、handler 内 Destroy 后同 ID 重建的新实例（旧实例转入 superseded）、
// Cast 取得的实体——连同 post-release 回调一起释放，不跨后续目标持有。某个目标加载失败、取锁失败或 handler panic
// 只记录日志，不影响后续目标。
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
		// 跳过，不解引用：下面的逐目标恢复边界从 Touch 之后才开始，nil 在这里会逃到整条派发的恢复边界，
		// 让后续目标全部被静默丢弃。
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
			// 广播没有提前放锁闭包：pipelined handler 在这条路径上按 strict 锁内提交执行。
			if _, err := mgr.invokeHandlerTransaction(entry.meta, oneEntity, name, nil, func() (any, error) {
				return entry.handler(oneEntity, params)
			}); err != nil {
				slog.Debug("nest broadcast handler failed", "id", meta.FullID, "handler", name, "err", err)
			}
		}()
	}
}

// normalizeFullIDs 把目标 ID 规范为完整 ID 并解析各自的分类。
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

// SortEntity 把实体按锁序（先锁组、再 GUId）排序，保证多实体取锁无死锁。
func SortEntity(es []entity.IThreadSafeEntity) {
	slices.SortFunc(es, func(a, b entity.IThreadSafeEntity) int {
		return compareEntityIDs(a.GUId(), b.GUId())
	})
}

// compareEntityIDs 是锁序比较：先比锁组，再比 GUId。
func compareEntityIDs(guidI, guidJ int64) int {
	groupI, groupJ := entity.GetEntityGroup(guidI), entity.GetEntityGroup(guidJ)
	if groupI != groupJ {
		return cmp.Compare(groupI, groupJ)
	}
	return cmp.Compare(guidI, guidJ)
}

// prepareRemoteWriteBatch 在派发前为消息的 Remote 目标准入一个带 fence 的写批次（取得写权限并加载）。
func prepareRemoteWriteBatch(manager entity.IRemoteEntityManager, msg *Msg) error {
	ids := extractRemoteIds(msg)
	if len(ids) == 0 {
		return nil
	}
	// 一个 Remote 写批次对应一个事务；广播每个实体一个事务，无法给批次提供匹配的原子提交 / 回滚边界。
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

// extractRemoteIds 收集消息目标里 ID 带 Remote 能力位且 kind 为 Remote 托管的实体 ID。
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
