package nestwal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	corecheckpoint "github.com/tjbdwanghaibo/cube-core/checkpoint"
	"github.com/tjbdwanghaibo/cube-core/entity"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	kitcheckpoint "github.com/tjbdwanghaibo/cube-kit/checkpoint"
	kitremote "github.com/tjbdwanghaibo/cube-kit/remote_entity"
)

// MongoAtomicApplier is the only production Nest mutation applier. It commits
// ordinary checkpoint after-images and Remote Entity commits in one MongoDB
// session transaction, then idempotently finalizes Remote Entity publication.
type MongoAtomicApplier struct {
	Backend     *kitcheckpoint.MongoBackend
	RemoteStore kitremote.AtomicCommitStore
	Remote      entity.RemoteCommitApplier
}

func (a *MongoAtomicApplier) ApplyMutations(ctx context.Context, txID corenest.TransactionID, mutations []corenest.EntityMutation) error {
	if a == nil || a.Backend == nil {
		return ErrMutationApplierRequired
	}
	if len(mutations) == 0 {
		return nil
	}
	raw, err := json.Marshal(mutations)
	if err != nil {
		return fmt.Errorf("nestwal: encode atomic mutation digest: %w", err)
	}
	digest := sha256.Sum256(raw)
	ordinary := make([]corecheckpoint.SaveOp, 0, len(mutations))
	remote := make([]entity.RemoteCommit, 0, len(mutations))
	remoteID := entity.RemoteTransactionID(txID)
	for i := range mutations {
		mutation := &mutations[i]
		if mutation.Remote != nil {
			if mutation.Remote.TransactionID != remoteID {
				return fmt.Errorf("nestwal: remote transaction identity mismatch at mutation %d", i)
			}
			remote = append(remote, mutation.Remote.Clone())
			continue
		}
		if mutation.Database == "" || mutation.Resource == "" || mutation.EntityID == 0 || mutation.Version == 0 || len(mutation.Data) == 0 {
			return fmt.Errorf("nestwal: incomplete ordinary mutation %d", i)
		}
		ordinary = append(ordinary, corecheckpoint.SaveOp{
			Db: mutation.Database, DbScope: corecheckpoint.DatabaseScope(mutation.DatabaseScope),
			Collection: mutation.Resource, ID: mutation.EntityID, Version: mutation.Version,
			Mask: mutation.Mask, Mode: corecheckpoint.SaveModeFull, Data: append([]byte(nil), mutation.Data...),
		})
	}
	if len(remote) != 0 && (a.RemoteStore == nil || a.Remote == nil) {
		return fmt.Errorf("%w: remote atomic store is not configured", ErrMutationApplierRequired)
	}
	err = a.Backend.ApplyAtomicTransaction(ctx, txID.String(), digest[:], ordinary, func(txCtx context.Context) error {
		if len(remote) == 0 {
			return nil
		}
		_, err := a.RemoteStore.ApplyRemoteCommitsInTransaction(txCtx, remote)
		return err
	})
	if err != nil {
		return err
	}
	if len(remote) != 0 {
		// The authoritative transaction is already durable. This second call is
		// an idempotent status lookup followed by snapshot/outbox publication and
		// live-entity acknowledgement.
		if _, err := a.Remote.ApplyRemoteCommits(ctx, remoteID, remote); err != nil {
			return err
		}
	}
	return nil
}

func (a *MongoAtomicApplier) AcknowledgeTransactions(ctx context.Context, ids []corenest.TransactionID) error {
	if a == nil || a.Backend == nil {
		return nil
	}
	for _, id := range ids {
		if err := a.Backend.AcknowledgeAtomicTransaction(ctx, id.String()); err != nil {
			return err
		}
	}
	return nil
}

var _ MutationApplier = (*MongoAtomicApplier)(nil)
var _ TransactionReceiptCleaner = (*MongoAtomicApplier)(nil)
