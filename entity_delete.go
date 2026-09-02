package dataengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

const entityDeleteHandler = "__dataengine_entity_delete"

type entityDeletePreparer interface {
	PrepareDelete(*corenest.RollbackTx) error
}

type remoteDeleteIntent struct{ entityID int64 }

func (intent remoteDeleteIntent) RemoteDeleteRequested(entityID int64) bool {
	return intent.entityID != 0 && intent.entityID == entityID
}

func (runtime *Runtime) admitEntityDelete(ctx context.Context, value entity.IThreadSafeEntity, reason entity.EntityDestroyReason) (admission entity.DeleteAdmission, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("dataengine delete: admission panic: %v", recovered)
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

func (runtime *Runtime) admitRemoteEntityDelete(ctx context.Context, value entity.IThreadSafeEntity) (entity.DeleteAdmission, error) {
	if runtime.remoteManager == nil {
		return entity.DeleteAdmissionImmediate, entity.ErrRemoteWriteCapabilityDisabled
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

	id, err := newSystemTransactionID()
	if err != nil {
		_ = batch.Abort(ctx, err)
		closeBatch()
		return entity.DeleteAdmissionImmediate, err
	}
	outcome := entity.NewRemoteTransactionOutcome(entity.RemoteTransactionID(id), entityDeleteHandler, "", true, uint8(corenest.DurabilityStrict))
	outcome.DeleteIntents = remoteDeleteIntent{entityID: value.ID()}
	if err := batch.FinalizeLocked(outcome); err != nil {
		_ = batch.Abort(ctx, err)
		closeBatch()
		return entity.DeleteAdmissionImmediate, err
	}
	commits := batch.Commits()
	if len(commits) != 1 || !commits[0].Delete {
		err := fmt.Errorf("dataengine delete: remote entity %d produced %d delete commits", value.ID(), len(commits))
		_ = batch.Abort(ctx, err)
		closeBatch()
		return entity.DeleteAdmissionImmediate, err
	}
	commit := commits[0].Clone()
	record := coredata.CommitRecord{
		ID: id, Handler: entityDeleteHandler, CreatedAt: time.Now().UTC().UnixNano(), Durability: corenest.DurabilityStrict,
		Mutations: []coredata.Mutation{{
			Key: coredata.DocumentKey{Resource: "remote_entity", ID: commit.EntityID}, Kind: coredata.MutationDelete,
			ExpectedVersion: commit.BaseVersion, NextVersion: commit.NextVersion, Schema: commit.Schema, Codec: "remote", Remote: &commit,
		}},
	}
	ticket, err := runtime.Projector.CommitSystem(ctx, record)
	if err != nil {
		if errors.Is(err, corenest.ErrCommitIndeterminate) {
			_ = batch.Indeterminate(ctx, err)
			closeBatch()
			runtime.fail(err)
			return entity.DeleteAdmissionIndeterminate, err
		}
		_ = batch.Abort(ctx, err)
		closeBatch()
		return entity.DeleteAdmissionImmediate, err
	}
	if err := coredata.WaitProjection(ctx, ticket); err != nil {
		_ = batch.Indeterminate(ctx, err)
		closeBatch()
		runtime.fail(err)
		return entity.DeleteAdmissionIndeterminate, err
	}
	if _, err := batch.Commit(ctx); err != nil {
		_ = batch.Indeterminate(ctx, err)
		closeBatch()
		runtime.fail(err)
		return entity.DeleteAdmissionIndeterminate, err
	}
	closeBatch()
	return entity.DeleteAdmissionImmediate, nil
}

func (runtime *Runtime) fail(err error) {
	if err != nil && runtime != nil && runtime.onFatal != nil {
		runtime.onFatal(err)
	}
}
