package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// invokeHandlerTransaction 是派发侧进入消息自己事务的唯一入口：在任何 handler 代码运行前检查 pipelined
// 白名单，提供引擎的完成泵，记录这次派发持有实体锁的时长；提交结果不确定时 fence 引擎。
func (mgr *NestMgr) invokeHandlerTransaction(meta HandlerMeta, es []entity.IThreadSafeEntity, name string, releaseLocks func(), call func() (any, error)) (any, error) {
	if meta.Durability == DurabilityPipelined && mgr != nil && mgr.pipelinedAllow != nil {
		if _, ok := mgr.pipelinedAllow[name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrPipelinedNotAllowed, name)
		}
	}
	// 锁持有时长：进入这里时锁已持有，释放发生在 releaseLocks（pipelined 提前放锁）或本函数返回后的
	// 派发处，以先发生者为准，在本 goroutine 上只记录一次。
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

// RunDetachedTransaction 给在持实体锁的 Nest handler 之外被调用的基础设施适配器提供一个隔离级别较低的事务边界。
// 它同样要求持久 committer，并走相同的准备 / 准入 / 接受 / 回滚生命周期；唯一省略的保证是实体加锁，由调用方负责。
//
// 已有 RollbackTx 时直接复用它；否则新建的事务与 RunIsolatedTransaction 相同，不认领当前派发的消息。
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

// RunIsolatedTransaction 总是新建自己的 strict 持久事务，即使在已有 Nest handler 内调用。它用于提交点不能随外层
// 业务事务回滚的基础设施生命周期操作。调用方必须已持有 call 需要的全部实体锁。
//
// 在 Nest 派发的消息里调用时，它从不认领消息：handler 内，以及消息自己的事务结束之后的收尾阶段（Guard post-release、
// 解锁后回调）都按嵌套独立事务处理，不改消息自己事务的提交事实：
//   - 要持久写的实体若已被外层可回滚事务（state / undo）登记回滚快照，在写任何持久记录之前返回
//     ErrNestedTransactionRollbackConflict 并自身回滚；外层是 memory handler 时不受此限。不经 DAO、用 AddMutation
//     直写的原始 mutation 按实体 ID 命中外层已快照的实体时同样拒绝。
//   - 所在消息带 Remote 批次（批次尚未收尾）时直接返回 ErrNestedTransactionInRemoteMessage，call 不执行。
//   - 持久提交或结果未知后，消息不再重排，自己的事务没提交时回复带 ErrNestedTransactionCommitted。
//   - 提交结果未知（ErrCommitIndeterminate）时，返回之前已 fence 所在的 Nest 引擎，与消息自己的事务相同；
//     外层 handler 继续执行到结束，但它自己的事务在交给 committer 之前返回 ErrNestFenced 并回滚。
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

// runTransaction 管理一次业务调用的回滚、准入和完成边界。顺序：
//  1. 归一 durability；嵌套独立事务遇到带 Remote 批次的消息直接拒绝；
//  2. 开始 Sync 变更（setter 只标脏，成功准入时在锁内统一冻结，解锁且提交确认后才可发送）；
//  3. memory + RollbackNone 走快路径：执行即生效，无持久记录；
//  4. 其余建立 RollbackTx、捕获实体、执行 handler，失败则整条回滚；
//  5. 带 Remote 批次的消息先 finalize 批次，再按 pipelined / 持久提交收尾。
//
// releaseLocks 必须幂等：pipelined 在准入后提前释放，dispatch 还会 defer 兜底释放。没有 releaseLocks 的广播路径
// 保持锁内提交；有完成池时可转移 WAL 等待和回复的所有权，队列满则在当前 worker 等待。Remote 批次保留两阶段
// 最终确认协议，走 strict 路径。
//
// claimsMessage 为真只来自 invokeWithTransaction（消息自己的事务入口）；RunIsolatedTransaction / RunDetachedTransaction
// 新建的事务传假，任何时刻都不认领当前派发的消息。
func runTransaction(claimsMessage bool, meta HandlerMeta, es []entity.IThreadSafeEntity, committer TransactionCommitter, handler string, releaseLocks func(), completions *completionPump, call func() (any, error), observers ...entity.SyncCommitObserver) (ret any, err error) {
	if meta.Durability == durabilityUnset {
		// 注册表里的 meta 已在注册时归一（validateHandlerMeta）；这里只兜包内直接传 meta 的调用。
		if meta.Rollback != RollbackNone {
			return nil, fmt.Errorf("%w: handler %q declares rollback=%s but no Durability", ErrDurabilityUnset, handler, meta.Rollback)
		}
		meta.Durability = DurabilityMemory
	}
	msg := currentNestDispatchMsg()
	// 消息自己的事务正在执行（txInFlight），或这不是消息自己的事务入口（消息自己的事务结束后的收尾阶段：Guard post-release、
	// 解锁后回调，此时 txInFlight 已复位而 Remote 批次要到 finishRemoteWriteBatch 才收尾）：都不是消息自己的事务。
	nested := msg != nil && (msg.txInFlight || !claimsMessage)
	if nested && msg.RemoteWriteBatch != nil {
		// 带 Remote 批次的消息里嵌套独立事务：批次只能随消息自己的事务 Commit / Abort。嵌套事务若经过批次分支，
		// 会替外层 finalize 并置 remoteCommitted，外层失败时批次仍被 Commit、回复误报已提交。维护者决定直接拒绝、
		// 不引入嵌套独立批次：call 不执行，不碰批次与 Msg 上的 Remote 事实；收尾阶段同样拒绝。
		// 历史：RR-20260926-75、RR-20260926-84。
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
	// 只有消息自己的事务（不是 handler 里嵌套的 RunIsolatedTransaction）才在 Msg 上记录提交事实（owner 非 nil）。
	// 纯本地消息收尾阶段里的独立事务同样不认领，按嵌套独立事务处理：commitDurable 对它标记 nestedTxCommitted，
	// 结果未知时 fence 引擎。
	var owner *Msg
	if msg != nil && !nested {
		owner = msg
		owner.txInFlight = true
		owner.txNoRollback = meta.Rollback == RollbackNone
		defer func() { owner.txInFlight = false }()
	}
	if meta.Rollback == RollbackNone && meta.Durability == DurabilityMemory && (msg == nil || msg.RemoteWriteBatch == nil) {
		// 从这里起 handler 的修改不能撤销：失败也不再重排。
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
			// 部署配置错误：直接报告，不静默降级为 strict 提交。
			return nil, ErrPipelinedCommitterRequired
		}
		// Remote 批次保留自己的两阶段协议，走 strict 路径；没有提前放锁的广播同样走 strict 路径。
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
	// handler 内嵌套的独立事务记下外层事务：提交前拒绝写外层会回滚的实体。消息自己的事务此处为 nil。
	tx.enclosing = CurrentRollbackTx()
	if err := tx.CaptureEntities(es); err != nil {
		return nil, err
	}
	if tx.policy == RollbackNone {
		// 带 Remote 批次的 memory handler：同样不撤销内存修改，开始执行后失败不再重排。
		owner.markNoRollbackHandlerStarted()
	}
	ret, err = callTransactionHandler(tx, call)
	if busy := tx.createLockBusy; busy != nil && tx.policy != RollbackNone && !errors.Is(err, ErrLockTimeout) {
		// handler 内新建实体遇到锁冲突而业务吞掉了错误：仍整条回滚并以锁超时重新准入，
		// 不能提交一个缺了新实体的结果。RollbackNone 不撤销内存修改，强制重做会重复生效，交由业务决定。
		err = errors.Join(err, busy)
	}
	if failed := tx.captureFailed; failed != nil && !errors.Is(err, failed) {
		// handler 内新建或 Cast 取得的实体事务捕获失败而业务吞掉了错误：与上面的锁冲突同类，整条回滚，不能提交
		// 一个缺了该实体的回滚 / 持久化参与、却可能已登记其提交参与者的结果。捕获错误不是锁超时，不会被重新准入；
		// 业务自己返回了它时不重复拼接。
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

// callTransactionHandler 在事务上下文内执行业务调用，只恢复业务调用的 panic（先回滚再重新 panic）。
// 成功准入后不能因释放或完成异常再回滚已接受的状态，所以准入之后的步骤不在这里。
// 业务调用期间把事务绑定到当前 Guard，handler 内 CreateInScope 新建的实体因此进入本事务。
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
// handler 内新建的实体因此随本 Guard 持锁到 handler 结束，并按 Cast 的锁序取锁。
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
// 走到这里的都是写任何持久记录之前的拒绝：Remote 批次 FinalizeLocked 拒绝、准备提交记录失败、嵌套事务写外层快照、
// fence 之后不交给 committer、committer / Enqueue 明确拒绝（acceptPersistence 的失败恒为结果未知，不会到这里）。
// 全部统一带 ErrCommitRejected，原因链不变；已带时不重复包。
func (tx *RollbackTx) rejectCommit(cause error) error {
	if !errors.Is(cause, ErrCommitRejected) {
		cause = errors.Join(ErrCommitRejected, cause)
	}
	if rbErr := tx.Rollback(); rbErr != nil {
		return errors.Join(cause, ErrRollbackFailed, rbErr)
	}
	return cause
}

// commitDurable 是 memory / async / strict（以及回退到 strict 路径的 pipelined）的锁内提交：交给 committer，
// 明确拒绝则回滚，结果未知则 abandon 并 fence；成功后记录提交事实并执行 Commit 收尾。
func (tx *RollbackTx) commitDurable(ctx context.Context, committer TransactionCommitter, msg *Msg) error {
	if err := tx.durableCommit(ctx, committer); err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
			if tx.dispatch == nil {
				// 嵌套独立事务结果未知：WAL 里可能已有它，外层消息同样不能重排。
				msg.markNestedTransactionCommitted()
				// 与消息自己的事务同一规则：返回业务之前 fence 引擎（同 invokeHandlerTransaction 的 mgr.Fence 入口与错误）。
				// 契约（维护者 2026-09-30 决定不加哨兵）：业务吞掉这个错误、而外层自己没有要持久的记录（含 memory handler；
				// 带 Remote 批次的会被 fence 拒绝，不在此列）时，回复是成功——结果未知只能从此后请求得到的 ErrNestFenced
				// 得知。嵌套事务的错误应当原样带进回复。
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
	// 持久提交点已越过：此后无论释放、回调、回复报什么错，这条消息都不能重新准入。
	tx.dispatch.markTransactionAdmitted()
	tx.dispatch.markTransactionCommitted()
	if tx.dispatch == nil && tx.accepted {
		// handler 内嵌套的独立事务（RunIsolatedTransaction 等，不认领消息）已持久提交：外层消息此后失败也不能
		// 重排，否则独立事务会再提交一次；回复由 dispatchNest 标上 ErrNestedTransactionCommitted。
		// 记录为空（accepted 为假）时没有任何内容持久化，不算越过提交点。
		msg.markNestedTransactionCommitted()
	}
	if msg != nil && msg.RemoteWriteBatch != nil {
		// 持久提交已成功：此后 AfterCommit、释放锁、release hook 的任何失败都不能让
		// Remote 批次 Abort。记录这个事实，而不是让收尾去猜错误类型。
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

// commitPipelined 是 pipelined 提交：锁内准备并 Enqueue（唯一拒绝点），提前放锁，然后等记录持久后完成。
// 有完成泵时 WAL 等待与回复交给 completion（队列满则在本 worker 等待），本函数返回后调用方不再回复或操作 tx。
func (tx *RollbackTx) commitPipelined(ctx context.Context, committer PipelinedTransactionCommitter, es []entity.IThreadSafeEntity, releaseLocks func(), pump *completionPump, msg *Msg, ret any) error {
	// 准备和 Enqueue 是锁内的拒绝边界；准入后只允许完成或报告结果不确定。
	ticket, err := tx.pipelinedEnqueue(ctx, committer)
	if ticket != nil {
		// WAL 已接纳这条记录：即使随后准入失败或 ticket 结果未知，重新准入都会让它再执行一次。
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
	// 记录已持久（或没有需要持久的内容）：此后的释放 / 回调错误由 dispatchNest 包 ErrAfterCommitFailed。
	tx.dispatch.markTransactionCommitted()
	return errors.Join(releaseErr, tx.commit(true))
}

// releaseAfterAdmission 在准入后释放锁，把释放时的 panic 转为 ErrEntityReleaseFailed 完成错误。
// 原释放闭包负责用 defer 清理剩余锁；这里不捕获业务阶段的 panic，也不把释放异常当作准入拒绝或触发回滚。
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

// stampCommitLSN 给本事务涉及的实体记下提交 LSN，供 Sync 按持久水位外发。覆盖初始参数与 Guard 动态取得的
// 实体；不在 Guard 内的独立调用只覆盖参数集合。
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

// invokeBusinessHandler 执行业务调用，按需记录 handler 阶段耗时。
func invokeBusinessHandler(handler string, stages bool, call func() (any, error)) (any, error) {
	defer observeNestStage(handler, "handler", startNestStage(stages))
	return call()
}
