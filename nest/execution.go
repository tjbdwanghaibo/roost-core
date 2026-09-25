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
	return invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, committer, handler, nil, nil, call)
}

// RunIsolatedTransaction always creates its own strict durable transaction,
// even when called from an existing Nest handler. It is intended for
// infrastructure lifecycle operations whose commit point cannot be rolled
// back with the surrounding business transaction. Callers must already hold
// every entity lock required by call.
func RunIsolatedTransaction(ctx context.Context, committer TransactionCommitter, handler string, call func() (any, error)) (any, error) {
	if call == nil {
		return nil, errors.New("nest: isolated transaction call is nil")
	}
	if committer == nil {
		return nil, ErrCommitterRequired
	}
	release := fctx.BindBase(ctx)
	defer release()
	return invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, committer, handler, nil, nil, call)
}

// invokeWithTransaction 管理一次业务调用的回滚、准入和完成边界。
// setter 只记录变化；成功准入时在锁内统一收集，Sync 在解锁且提交确认后才可发送。
// releaseLocks 必须幂等：pipelined 在准入后提前释放，dispatch 还会 defer 兜底释放。
// 没有 releaseLocks 的广播路径保持锁内提交；有完成池时可转移 WAL 等待和回复所有权，
// 队列满则在当前 worker 等待。远端批次仍使用原有最终确认协议。
func invokeWithTransaction(meta HandlerMeta, es []entity.IThreadSafeEntity, committer TransactionCommitter, handler string, releaseLocks func(), completions *completionPump, call func() (any, error), observers ...entity.SyncCommitObserver) (ret any, err error) {
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
	msg := currentNestDispatchMsg()
	stages := msg != nil && msg.stageMetrics
	if meta.Rollback == RollbackNone && meta.Durability == DurabilityMemory && (msg == nil || msg.RemoteWriteBatch == nil) {
		ret, err = invokeBusinessHandler(handler, stages, call)
		if err == nil {
			admissionStart := startNestStage(stages)
			syncMutation.Admit()
			observeNestStage(handler, "admission", admissionStart)
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
	if err := tx.CaptureEntities(es); err != nil {
		return nil, err
	}
	ret, err = callTransactionHandler(tx, call)
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
func callTransactionHandler(tx *RollbackTx, call func() (any, error)) (any, error) {
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

// rejectCommit 只用于明确拒绝；不确定结果必须 abandon 并交给 fencing/recovery。
func (tx *RollbackTx) rejectCommit(cause error) error {
	if rbErr := tx.Rollback(); rbErr != nil {
		return errors.Join(cause, ErrRollbackFailed, rbErr)
	}
	return cause
}

func (tx *RollbackTx) commitDurable(ctx context.Context, committer TransactionCommitter, msg *Msg) error {
	if err := tx.durableCommit(ctx, committer); err != nil {
		if errors.Is(err, ErrCommitIndeterminate) {
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
