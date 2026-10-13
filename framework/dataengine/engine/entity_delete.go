package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// entityDeleteHandler 是框架发起的实体删除事务在 WAL 中的 handler 名。
const entityDeleteHandler = "__dataengine_entity_delete"

// entityDeletePreparer 由生成 Entity 实现：在事务内登记全部 DAO 的删除 mutation。
type entityDeletePreparer interface {
	PrepareDelete(*corenest.RollbackTx) error
}

// remoteDeleteIntent 告诉 Remote 批次哪个实体是本事务明确请求删除的目标。
type remoteDeleteIntent struct{ entityID int64 }

func (intent remoteDeleteIntent) RemoteDeleteRequested(entityID int64) bool {
	return intent.entityID != 0 && intent.entityID == entityID
}

// admitEntityDelete 是 EntityManager 销毁实体前的持久删除准入，按调用位置分三路：
// 在 Nest 事务内则延后到事务准入（deferEntityDelete）；否则 Remote 实体走独立的远端
// 删除事务，本地实体走隔离的本地删除事务。
func (runtime *Runtime) admitEntityDelete(ctx context.Context, value entity.IThreadSafeEntity, reason entity.EntityDestroyReason) (admission entity.DeleteAdmission, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// 准入中途 panic 无法证明远端、WAL 或内存未被动过，只能按不确定结果处理：
			// fatal 并由 EntityManager 摘除实体。保留原错误链供 errors.Is 判断。
			if cause, ok := recovered.(error); ok {
				err = fmt.Errorf("dataengine delete: admission panic: %w", cause)
			} else {
				err = fmt.Errorf("dataengine delete: admission panic: %v", recovered)
			}
			admission = entity.DeleteAdmissionIndeterminate
			runtime.fail(err)
		}
	}()
	if runtime == nil || runtime.Projector == nil || runtime.access == nil || value == nil {
		return entity.DeleteAdmissionImmediate, errors.New("dataengine delete: runtime is not configured")
	}
	if !value.AutoPersist() {
		return entity.DeleteAdmissionImmediate, nil
	}
	if tx := corenest.CurrentRollbackTx(); tx != nil {
		return runtime.deferEntityDelete(tx, value, reason)
	}
	if entity.IsEntityKindRemoteManaged(value.GetEntityKind()) {
		return runtime.admitRemoteEntityDelete(ctx, value)
	}
	return runtime.admitLocalEntityDelete(ctx, value)
}

// deferEntityDelete 把删除并入当前 Nest 事务：同一实体只登记一次，本地实体立即准备删除
// mutation；事务准入成功后再销毁内存实体。Remote 删除目标必须已在消息中声明。
func (runtime *Runtime) deferEntityDelete(tx *corenest.RollbackTx, value entity.IThreadSafeEntity, reason entity.EntityDestroyReason) (entity.DeleteAdmission, error) {
	remote := entity.IsEntityKindRemoteManaged(value.GetEntityKind())
	if remote && !corenest.CurrentRemoteWriteBatchContains(value.ID()) {
		return entity.DeleteAdmissionImmediate, fmt.Errorf("%w: remote delete target %d was not declared in the Nest message", corenest.ErrDurableRemoteWriteUnsupported, value.ID())
	}
	first, err := tx.RequestEntityDelete(value.ID())
	if err != nil {
		return entity.DeleteAdmissionImmediate, err
	}
	if !first {
		return entity.DeleteAdmissionDeferred, nil
	}
	if !remote {
		preparer, ok := value.(entityDeletePreparer)
		if !ok {
			tx.CancelEntityDelete(value.ID())
			return entity.DeleteAdmissionImmediate, fmt.Errorf("dataengine delete: entity %d has no generated delete preparer", value.ID())
		}
		if err := preparer.PrepareDelete(tx); err != nil {
			tx.CancelEntityDelete(value.ID())
			return entity.DeleteAdmissionImmediate, err
		}
	}
	tx.AfterAdmission(func() {
		if err := runtime.access.Destroy(context.Background(), value, reason, false); err != nil {
			runtime.fail(fmt.Errorf("dataengine delete: finalize in-memory entity %d: %w", value.ID(), err))
		}
	})
	return entity.DeleteAdmissionDeferred, nil
}

// admitLocalEntityDelete 在隔离事务里准备并提交本地实体的删除；提交结果未知时 fatal。
func (runtime *Runtime) admitLocalEntityDelete(ctx context.Context, value entity.IThreadSafeEntity) (entity.DeleteAdmission, error) {
	preparer, ok := value.(entityDeletePreparer)
	if !ok {
		return entity.DeleteAdmissionImmediate, fmt.Errorf("dataengine delete: entity %d has no generated delete preparer", value.ID())
	}
	_, err := corenest.RunIsolatedTransaction(ctx, runtime.Projector, entityDeleteHandler, func() (any, error) {
		tx := corenest.CurrentRollbackTx()
		if tx == nil {
			return nil, corenest.ErrTransactionClosed
		}
		return nil, preparer.PrepareDelete(tx)
	})
	if errors.Is(err, corenest.ErrCommitIndeterminate) {
		runtime.fail(err)
		return entity.DeleteAdmissionIndeterminate, err
	}
	return entity.DeleteAdmissionImmediate, err
}

// admitRemoteEntityDelete 独立删除 Remote 实体：准备远端批次 → 冻结删除提交 → 系统事务
// 准入 WAL → 等待投影 → 远端确认。WAL 准入前失败则 Abort；准入后任何失败都是不确定结果，
// 交给批次的 Indeterminate 收尾并 fatal。
func (runtime *Runtime) admitRemoteEntityDelete(ctx context.Context, value entity.IThreadSafeEntity) (entity.DeleteAdmission, error) {
	if runtime.remoteManager == nil {
		return entity.DeleteAdmissionImmediate, entity.ErrRemoteWriteCapabilityDisabled
	}
	// 独立的 Remote 删除要准备远端批次、等待持久提交和确认，业务 worker 上不允许等待。
	// 在任何远端/WAL 副作用之前明确拒绝：实体保留在内存、不 fence，调用方可按
	// fctx.ErrBlockingInBusinessWorker 判别并改为声明目标后走正式事务。不能等到
	// PrepareRemoteWriteBatch 的入口断言 panic：那会被 admitEntityDelete 的 recover
	// 当成不确定结果，一次误用就 fence 全进程。历史：RR-20260926-27。
	if err := fctx.BlockingError("dataengine.admitRemoteEntityDelete"); err != nil {
		return entity.DeleteAdmissionImmediate, fmt.Errorf("dataengine delete: remote entity %d: %w", value.ID(), err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batch, err := runtime.remoteManager.PrepareRemoteWriteBatch(ctx, []int64{value.ID()})
	if err != nil {
		return entity.DeleteAdmissionImmediate, err
	}
	if batch == nil {
		return entity.DeleteAdmissionImmediate, entity.ErrRemoteWriteCapabilityDisabled
	}
	closeBatch := func() {
		if closeErr := batch.Close(context.WithoutCancel(ctx)); closeErr != nil {
			runtime.fail(fmt.Errorf("dataengine delete: close remote batch: %w", closeErr))
		}
	}
	// abort 用于 WAL 准入前的失败：远端没有任何持久副作用，实体保留。
	abort := func(err error) (entity.DeleteAdmission, error) {
		_ = batch.Abort(ctx, err)
		closeBatch()
		return entity.DeleteAdmissionImmediate, err
	}
	// indeterminate 用于准入后结果未知：交给批次收尾，并 fatal 由 EntityManager 摘除实体。
	indeterminate := func(err error) (entity.DeleteAdmission, error) {
		_ = batch.Indeterminate(ctx, err)
		closeBatch()
		runtime.fail(err)
		return entity.DeleteAdmissionIndeterminate, err
	}

	id, err := newSystemTransactionID()
	if err != nil {
		return abort(err)
	}
	outcome := entity.NewRemoteTransactionOutcome(entity.RemoteTransactionID(id), entityDeleteHandler, "", true, uint8(coredata.DurabilityStrict))
	outcome.DeleteIntents = remoteDeleteIntent{entityID: value.ID()}
	if err := batch.FinalizeLocked(outcome); err != nil {
		return abort(err)
	}
	commits := batch.Commits()
	if len(commits) != 1 || !commits[0].Delete {
		return abort(fmt.Errorf("dataengine delete: remote entity %d produced %d delete commits", value.ID(), len(commits)))
	}
	commit := commits[0].Clone()
	record := coredata.CommitRecord{
		ID: id, Handler: entityDeleteHandler, CreatedAt: time.Now().UTC().UnixNano(), Durability: coredata.DurabilityStrict,
		Mutations: []coredata.Mutation{{
			Key: coredata.DocumentKey{Resource: "remote_entity", ID: commit.EntityID}, Kind: coredata.MutationDelete,
			ExpectedVersion: commit.BaseVersion, NextVersion: commit.NextVersion, Schema: commit.Schema, Codec: "remote", Remote: &commit,
		}},
	}
	ticket, err := runtime.Projector.CommitSystem(ctx, record)
	if err != nil {
		if errors.Is(err, corenest.ErrCommitIndeterminate) {
			return indeterminate(err)
		}
		return abort(err)
	}
	if err := coredata.WaitProjection(ctx, ticket); err != nil {
		return indeterminate(err)
	}
	if _, err := batch.Commit(ctx); err != nil {
		return indeterminate(err)
	}
	closeBatch()
	return entity.DeleteAdmissionImmediate, nil
}

func (runtime *Runtime) fail(err error) {
	if err != nil && runtime != nil && runtime.onFatal != nil {
		runtime.onFatal(err)
	}
}

func newSystemTransactionID() (coredata.TransactionID, error) {
	var id coredata.TransactionID
	if _, err := rand.Read(id[:]); err != nil {
		return coredata.TransactionID{}, fmt.Errorf("dataengine: transaction id: %w", err)
	}
	return id, nil
}
