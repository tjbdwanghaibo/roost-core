package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// invokeHandlerTransaction is the single dispatch-side entry into
// invokeWithTransaction: it enforces the pipelined allowlist before any
// handler code runs, supplies the engine's completion pump, and measures how
// long the dispatch held its entity locks.
func (mgr *NestMgr) invokeHandlerTransaction(meta HandlerMeta, es []entity.IThreadSafeEntity, name string, releaseLocks func(), call func() (any, error)) (any, error) {
	if meta.Durability == DurabilityPipelined && mgr != nil && mgr.pipelinedAllow != nil {
		if _, ok := mgr.pipelinedAllow[name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrPipelinedNotAllowed, name)
		}
	}
	// Lock-hold budget: the locks are already held here, and they are
	// released either through releaseLocks (the pipelined early release) or
	// by the dispatch site right after this call returns — record at
	// whichever happens first, once, on this goroutine.
	start := time.Now()
	recorded := false
	record := func() {
		if !recorded {
			recorded = true
			mgr.observeLockHold(name, time.Since(start))
		}
	}
	wrappedRelease := releaseLocks
	if releaseLocks != nil {
		wrappedRelease = func() {
			record()
			releaseLocks()
		}
	}
	ret, err := invokeWithTransaction(meta, es, mgr.committer, name, wrappedRelease, mgr.completions, call, mgr.entitySync)
	if errors.Is(err, ErrCommitIndeterminate) {
		mgr.Fence(err)
	}
	record()
	return ret, err
}

// RunDetachedTransaction gives infrastructure adapters a lower-isolation
// transaction boundary when they are invoked outside an entity-locked Nest
// handler. It still requires a durable committer and uses the same
// prepare/admit/accept/rollback lifecycle; the only omitted guarantee is
// entity locking, which remains the caller's responsibility.
//
// 已有 RollbackTx 时直接复用它；否则新建的事务与 RunIsolatedTransaction 相同，不认领当前派发的消息（RR-20260926-84）。
func RunDetachedTransaction(ctx context.Context, committer TransactionCommitter, handler string, call func() (any, error)) (any, error) {
	if call == nil {
		return nil, errors.New("nest: detached transaction call is nil")
	}
	if CurrentRollbackTx() != nil {
		return call()
	}
	if committer == nil {
		return nil, ErrCommitterRequired
	}
	release := fctx.BindBase(ctx)
	defer release()
	return runTransaction(false, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, committer, handler, nil, nil, call)
}

// RunIsolatedTransaction always creates its own strict durable transaction,
// even when called from an existing Nest handler. It is intended for
// infrastructure lifecycle operations whose commit point cannot be rolled
// back with the surrounding business transaction. Callers must already hold
// every entity lock required by call.
//
// 在 Nest 派发的消息里调用时，它从不认领消息：handler 内，以及消息自己的事务结束之后的收尾阶段（Guard post-release、
// 解锁后回调）都按嵌套独立事务处理，不改消息自己事务的提交事实（RR-20260926-84）：
//   - 要持久写的实体若已被外层可回滚事务（state / undo）登记回滚快照，在写任何持久记录之前返回
//     ErrNestedTransactionRollbackConflict 并自身回滚（RR-20260926-74）；外层是 memory handler 时不受此限。不经 DAO、用 AddMutation
//     直写的原始 mutation 按实体 ID 命中外层已快照的实体时同样拒绝（RR-20260927-07）。
//   - 所在消息带 Remote 批次（批次尚未收尾）时直接返回 ErrNestedTransactionInRemoteMessage，call 不执行（RR-20260926-75 / 84）。
//   - 持久提交或结果未知后，消息不再重排，自己的事务没提交时回复带 ErrNestedTransactionCommitted（RR-20260926-65）。
//   - 提交结果未知（ErrCommitIndeterminate）时，返回之前已 fence 所在的 Nest 引擎，与消息自己的事务相同（RR-20260926-76）；
//     外层 handler 继续执行到结束，但它自己的事务在交给 committer 之前返回 ErrNestFenced 并回滚（RR-20260927-06）。
func RunIsolatedTransaction(ctx context.Context, committer TransactionCommitter, handler string, call func() (any, error)) (any, error) {
	if call == nil {
		return nil, errors.New("nest: isolated transaction call is nil")
	}
	if committer == nil {
		return nil, ErrCommitterRequired
	}
	release := fctx.BindBase(ctx)
	defer release()
	return runTransaction(false, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, committer, handler, nil, nil, call)
}

// invokeWithTransaction 是消息自己的事务入口（invokeHandlerTransaction）：它可以认领当前派发的消息。
func invokeWithTransaction(meta HandlerMeta, es []entity.IThreadSafeEntity, committer TransactionCommitter, handler string, releaseLocks func(), completions *completionPump, call func() (any, error), observers ...entity.SyncCommitObserver) (any, error) {
	return runTransaction(true, meta, es, committer, handler, releaseLocks, completions, call, observers...)
}

// runTransaction 管理一次业务调用的回滚、准入和完成边界。
// setter 只记录变化；成功准入时在锁内统一收集，Sync 在解锁且提交确认后才可发送。
// releaseLocks 必须幂等：pipelined 在准入后提前释放，dispatch 还会 defer 兜底释放。
// 没有 releaseLocks 的广播路径保持锁内提交；有完成池时可转移 WAL 等待和回复所有权，
// 队列满则在当前 worker 等待。远端批次仍使用原有最终确认协议。
//
// claimsMessage 为真只来自 invokeWithTransaction（消息自己的事务入口）；RunIsolatedTransaction / RunDetachedTransaction 新建的
// 事务传假，任何时刻都不认领当前派发的消息（RR-20260926-84）。
func runTransaction(claimsMessage bool, meta HandlerMeta, es []entity.IThreadSafeEntity, committer TransactionCommitter, handler string, releaseLocks func(), completions *completionPump, call func() (any, error), observers ...entity.SyncCommitObserver) (ret any, err error) {
	msg := currentNestDispatchMsg()
	// 消息自己的事务正在执行（txInFlight），或这不是消息自己的事务入口（消息自己的事务结束后的收尾阶段：Guard post-release、
	// 解锁后回调，此时 txInFlight 已复位而 Remote 批次要到 finishRemoteWriteBatch 才收尾）：都不是消息自己的事务。
	nested := msg != nil && (msg.txInFlight || !claimsMessage)
	if nested && msg.RemoteWriteBatch != nil {
		// 带 Remote 批次的消息里嵌套独立事务：批次只能随消息自己的事务 Commit / Abort。旧实现让嵌套事务经过
		// finalizeRemoteWriteBatch / commitDurable 的批次分支，替外层 finalize 并置 remoteCommitted，外层失败时批次仍 Commit、
		// 回复误报已提交（RR-20260926-75）。维护者决定拒绝、不引入嵌套独立批次：call 不执行，不碰批次与 Msg 上的 Remote 事实。
		// 收尾阶段同样拒绝：旧实现只看 txInFlight，那时独立事务被当成消息自己的事务，外层失败后批次仍被 Commit（RR-20260926-84）。
		return nil, fmt.Errorf("%w: nested %q in message %q", ErrNestedTransactionInRemoteMessage, handler, msg.Name)
	}
	var observer entity.SyncCommitObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	syncMutation := entity.BeginSyncMutation(es, observer)
	defer func() { syncMutation.Finish(errors.Is(err, ErrCommitIndeterminate)) }()
	if syncMutation != nil {
		if scope := entity.CurrentGuardScope(); scope != nil {
			scope.Guard().AppendPostRelease(syncMutation.Release)
		}
	}
	if releaseLocks != nil {
		originalRelease := releaseLocks
		releaseLocks = func() {
			// 即使某个 release hook panic，也先清理剩余动态锁，再开放完成屏障。
			defer func() {
				if scope := entity.CurrentGuardScope(); scope != nil && scope.Guard() != nil {
					scope.Guard().ReleaseAll()
				}
				syncMutation.Release()
			}()
			originalRelease()
		}
	}
	stages := msg != nil && msg.stageMetrics
	// 本条消息自己的事务（不是 handler 里嵌套的 RunIsolatedTransaction）才在 Msg 上记录提交事实（RR-20260926-49）。
	// 纯本地消息收尾阶段里的独立事务同样不认领：旧实现以 !txInFlight 判定“消息自己的事务”，它会把外层已回滚的消息
	// 标成已提交（回复误带 ErrAfterCommitFailed），结果未知时因 tx.dispatch 非 nil 也不 fence。现在它按嵌套独立事务处理，
	// commitDurable 里 RR-65 的 nestedTxCommitted 与 RR-76 的 fence 同样适用（RR-20260926-84）。
	var owner *Msg
	if msg != nil && !nested {
		owner = msg
		owner.txInFlight = true
		owner.txNoRollback = meta.Rollback == RollbackNone
		defer func() { owner.txInFlight = false }()
	}
	if meta.Rollback == RollbackNone && meta.Durability == DurabilityMemory && (msg == nil || msg.RemoteWriteBatch == nil) {
		// 从这里起 handler 的修改不能撤销：失败也不再重排（RR-20260926-73）。
		owner.markNoRollbackHandlerStarted()
		ret, err = invokeMemoryHandler(handler, stages, call)
		if err == nil {
			admissionStart := startNestStage(stages)
			syncMutation.Admit()
			observeNestStage(handler, "admission", admissionStart)
			owner.markTransactionAdmitted()
			owner.markTransactionCommitted()
			syncMutation.Confirm()
		}
		return ret, err
	}
	var pipelinedCommitter PipelinedTransactionCommitter
	if meta.Durability == DurabilityPipelined {
		pc, ok := committer.(PipelinedTransactionCommitter)
		if !ok {
			// Deployment configuration error: report instead of silently
			// degrading to strict commits.
			return nil, ErrPipelinedCommitterRequired
		}
		// Remote write batches keep their own two-phase protocol and stay on
		// the strict path; so does broadcast, which has no early release.
		if releaseLocks != nil && (msg == nil || msg.RemoteWriteBatch == nil) {
			pipelinedCommitter = pc
		}
	}
	tx := NewRollbackTx(meta.Rollback)
	tx.syncMutation = syncMutation
	if syncMutation != nil {
		tx.AfterCommit(syncMutation.Confirm)
	}
	tx.durability = meta.Durability
	tx.handler = handler
	tx.stageMetrics = stages
	tx.dispatch = owner
	// handler 内嵌套的独立事务记下外层事务：提交前拒绝写外层会回滚的实体（RR-20260926-74）。消息自己的事务此处为 nil。
	tx.enclosing = CurrentRollbackTx()
	if err := tx.CaptureEntities(es); err != nil {
		return nil, err
	}
	if tx.policy == RollbackNone {
		// 带 Remote 批次的 memory handler：同样不撤销内存修改，开始执行后失败不再重排（RR-20260926-73）。
		owner.markNoRollbackHandlerStarted()
	}
	ret, err = callTransactionHandler(tx, call)
	if busy := tx.createLockBusy; busy != nil && tx.policy != RollbackNone && !errors.Is(err, ErrLockTimeout) {
		// handler 内新建实体遇到锁冲突而业务吞掉了错误：仍整条回滚并以锁超时重新准入（RR-20260926-48），
		// 不能提交一个缺了新实体的结果。RollbackNone 不撤销内存修改，强制重做会重复生效，交由业务决定。
		err = errors.Join(err, busy)
	}
	if failed := tx.captureFailed; failed != nil && !errors.Is(err, failed) {
		// handler 内新建（RR-20260927-11）或 Cast 取得（RR-20260927-31）的实体事务捕获失败而业务吞掉了错误：与上面的锁冲突
		// 同类，整条回滚，不能提交一个缺了该实体的回滚 / 持久化参与、却可能已登记其提交参与者的结果。捕获错误不是锁超时，
		// 不会被重新准入；业务自己返回了它时不重复拼接。
		err = errors.Join(err, failed)
	}
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback failed: %w", rbErr))
		}
		return ret, err
	}
	// handler 已退出事务上下文；准入、释放、完成按顺序显式执行。
	ctx := nestBaseContext()
	if msg != nil && msg.RemoteWriteBatch != nil {
		if finalizeErr := msg.finalizeRemoteWriteBatch(tx); finalizeErr != nil {
			if abortErr := msg.abortRemoteWriteBatchLocked(finalizeErr); abortErr != nil {
				finalizeErr = errors.Join(finalizeErr, abortErr)
			}
			return ret, tx.rejectCommit(finalizeErr)
		}
	}
	if pipelinedCommitter != nil {
		return ret, tx.commitPipelined(ctx, pipelinedCommitter, es, releaseLocks, completions, msg, ret)
	}
	return ret, tx.commitDurable(ctx, committer, msg)
}

// 只恢复业务调用的 panic。成功准入后不能因释放或完成异常再回滚已接受的状态。
// 业务调用期间把事务绑定到当前 Guard，handler 内 CreateInScope 新建的实体因此进入本事务（RR-20260926-35）。
func callTransactionHandler(tx *RollbackTx, call func() (any, error)) (any, error) {
	if scope := entity.CurrentGuardScope(); scope != nil && scope.Guard() != nil {
		defer scope.Guard().BindCreatedEntityCapturer(tx)()
	}
	defer func() {
		if r := recover(); r != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				slog.Error("nest rollback after panic failed", "rollback", tx.policy.String(), "err", rbErr)
				panic(errors.Join(fmt.Errorf("panic: %v", r), ErrRollbackFailed, rbErr))
			}
			panic(r)
		}
	}()
	return withRollbackTx(tx, func() (any, error) { return invokeBusinessHandler(tx.handler, tx.stageMetrics, call) })
}

// invokeMemoryHandler 执行 memory 快路径的业务调用。期间在当前 Guard 上绑定 memoryHandlerCreates，
// handler 内新建的实体因此随本 Guard 持锁到 handler 结束，并按 Cast 的锁序取锁（RR-20260926-48）。
func invokeMemoryHandler(handler string, stages bool, call func() (any, error)) (any, error) {
	if scope := entity.CurrentGuardScope(); scope != nil && scope.Guard() != nil {
		guard := scope.Guard()
		previous := guard.SwapCreatedEntityCapturer(memoryHandlerCreates{})
		defer guard.SwapCreatedEntityCapturer(previous)
	}
	return invokeBusinessHandler(handler, stages, call)
}

// rejectCommit 只用于明确拒绝；不确定结果必须 abandon 并交给 fencing/recovery。
//
// 走到这里的都是写任何持久记录之前的拒绝：Remote 批次 FinalizeLocked 拒绝、准备提交记录失败、嵌套事务写外层快照（RR-74）、
// fence 之后不交给 committer（RR-20260927-06）、committer / Enqueue 明确拒绝（acceptPersistence 的失败恒为结果未知，不会到这里）。
// 统一带 ErrCommitRejected（RR-20260927-32）：之前只有 committer 拒绝带它，其余原样返回，判别表按“从上到下第一个命中”
// 一行都不命中。只增加 errors.Is 命中，原因链不变；已带时不重复包。
func (tx *RollbackTx) rejectCommit(cause error) error {
	if !errors.Is(cause, ErrCommitRejected) {
		cause = errors.Join(ErrCommitRejected, cause)
	}
	if rbErr := tx.Rollback(); rbErr != nil {
		return errors.Join(cause, ErrRollbackFailed, rbErr)
	}
	return cause
}

func (tx *RollbackTx) commitDurable(ctx context.Context, committer TransactionCommitter, msg *Msg) error {
	if err := tx.durableCommit(ctx, committer); err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			if tx.dispatch == nil {
				// 嵌套独立事务结果未知：WAL 里可能已有它，外层消息同样不能重排（RR-20260926-65）。
				msg.markNestedTransactionCommitted()
				// 与消息自己的事务同一规则：返回业务之前 fence 引擎（同 invokeHandlerTransaction 的 mgr.Fence 入口与错误）。
				// 旧实现只在消息自己的事务返回结果未知时 fence，业务吞掉嵌套事务的错误后引擎照常受理新请求（RR-20260926-76）。
				// 契约（REMAINING §3 N22，维护者 2026-09-30 决定不加哨兵）：业务吞掉这个错误、而外层自己没有要持久的记录
				// （含 memory handler；带 Remote 批次的从 RR-20260930-12 起被 fence 拒绝，不在此列）时，回复是成功——
				// 结果未知只能从此后请求得到的 ErrNestFenced 得知。嵌套事务的错误应当原样带进回复（判别表第 1 / 5 行）。
				msg.fenceEngine(err)
			}
			if msg != nil && msg.RemoteWriteBatch != nil {
				err = errors.Join(err, msg.markRemoteWriteIndeterminateLocked(err))
			}
			tx.abandon()
			return err
		}
		if msg != nil && msg.RemoteWriteBatch != nil {
			err = errors.Join(err, msg.abortRemoteWriteBatchLocked(err))
		}
		return tx.rejectCommit(err)
	}
	// 持久提交点已越过：此后无论释放、回调、回复报什么错，这条消息都不能重新准入（RR-20260926-49）。
	tx.dispatch.markTransactionAdmitted()
	tx.dispatch.markTransactionCommitted()
	if tx.dispatch == nil && tx.accepted {
		// handler 内嵌套的独立事务（RunIsolatedTransaction 等，不认领消息）已持久提交：外层消息此后失败也不能
		// 重排，否则独立事务会再提交一次；回复由 dispatchNest 标上 ErrNestedTransactionCommitted（RR-20260926-65）。
		// 记录为空（accepted 为假）时没有任何内容持久化，不算越过提交点。
		msg.markNestedTransactionCommitted()
	}
	if msg != nil && msg.RemoteWriteBatch != nil {
		// 持久提交已成功：此后 AfterCommit、释放锁、release hook 的任何失败都不能让
		// Remote 批次 Abort。记录这个事实，而不是让收尾去猜错误类型（RR-20260926-32）。
		msg.remoteCommitted = true
		msg.remoteSyncMutation = tx.syncMutation
	}
	if notifier, ok := committer.(TransactionReleaseNotifier); ok {
		txID := tx.ID()
		if msg != nil && msg.RemoteWriteBatch != nil {
			msg.addAfterUnlock(func() { notifier.TransactionReleased(txID) })
		} else {
			tx.AfterCommit(func() { notifier.TransactionReleased(txID) })
		}
	}
	return tx.Commit()
}

func (tx *RollbackTx) commitPipelined(ctx context.Context, committer PipelinedTransactionCommitter, es []entity.IThreadSafeEntity, releaseLocks func(), pump *completionPump, msg *Msg, ret any) error {
	// 准备和 Enqueue 是锁内的拒绝边界；准入后只允许完成或报告结果不确定。
	ticket, err := tx.pipelinedEnqueue(ctx, committer)
	if ticket != nil {
		// WAL 已接纳这条记录：即使随后准入失败或 ticket 结果未知，重新准入都会让它再执行一次（RR-20260926-49）。
		tx.dispatch.markTransactionAdmitted()
	}
	if err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			tx.abandon()
			return err
		}
		return tx.rejectCommit(err)
	}
	if ticket != nil {
		stampCommitLSN(es, tx.syncMutation, ticket.LSN())
	}
	tx.runAfterAdmission()
	if notifier, ok := committer.(TransactionReleaseNotifier); ok {
		txID := tx.ID()
		tx.AfterCommit(func() { notifier.TransactionReleased(txID) })
	}
	if ticket != nil && pump != nil && msg != nil {
		// 排序位置在锁内占用；完成执行还必须等待所有实体实际解锁。
		handoff := prepareCompletion(pump, msg, es, tx, ticket, tx.handler, ret)
		if !handoff.deferred {
			defer handoff.releaseOrder()
		}
		// 准入后的 release panic 只能作为完成错误报告，不能跳过 WAL 等待和回复。
		// releaseErr 在关闭屏障前写入，完成 goroutine 在屏障后读取。
		handoff.releaseErr = releaseAfterAdmission(releaseLocks)
		close(handoff.unlocked)
		if !handoff.deferred {
			waitStart := startNestStage(tx.stageMetrics)
			<-ticket.Done()
			observeNestStage(tx.handler, "durable_wait", waitStart)
			handoff.complete(ticket.Err())
		}
		// 两种路径的回复均归 complete，调用方不再回复或操作 tx。
		return nil
	}
	releaseErr := releaseAfterAdmission(releaseLocks)
	if ticket != nil {
		waitStart := time.Now()
		<-ticket.Done()
		if tx.stageMetrics {
			observeNestStage(tx.handler, "durable_wait", waitStart)
		}
		metrics.ObserveDuration("nest.pipelined.durable_wait", metrics.Labels{"handler": tx.handler}, time.Since(waitStart))
		if err := ticket.Err(); err != nil {
			tx.abandon()
			return errors.Join(releaseErr, err)
		}
	}
	// 记录已持久（或没有需要持久的内容）：此后的释放 / 回调错误由 dispatchNest 包 ErrAfterCommitFailed（RR-20260926-53）。
	tx.dispatch.markTransactionCommitted()
	return errors.Join(releaseErr, tx.commit(true))
}

// 原释放闭包负责用 defer 清理剩余锁；这里仅把异常转为完成错误。
// 不捕获业务阶段的 panic，也不把释放异常当作准入拒绝或触发回滚。
func releaseAfterAdmission(release func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if cause, ok := r.(error); ok {
				err = fmt.Errorf("%w: %w", ErrEntityReleaseFailed, cause)
			} else {
				err = fmt.Errorf("%w: %v", ErrEntityReleaseFailed, r)
			}
		}
	}()
	release()
	return nil
}

// 水位覆盖初始参数与 Guard 动态取得的实体；独立调用仍可仅提供参数集合。
func stampCommitLSN(es []entity.IThreadSafeEntity, mutation *entity.SyncMutation, lsn uint64) {
	mutation.SetLastCommitLSN(lsn)
	if scope := entity.CurrentGuardScope(); scope != nil && scope.Guard() != nil {
		for _, e := range scope.Guard().Entities() {
			if e != nil && e.Base() != nil {
				e.Base().SetLastCommitLSN(lsn)
			}
		}
	}
	for _, e := range es {
		if e != nil && e.Base() != nil {
			e.Base().SetLastCommitLSN(lsn)
		}
	}
}

func invokeBusinessHandler(handler string, stages bool, call func() (any, error)) (any, error) {
	defer observeNestStage(handler, "handler", startNestStage(stages))
	return call()
}
