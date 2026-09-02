package remoteentity

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/cube-core/entity"
)

type StorageBackend interface {
	entity.IRemoteAtomicBatchCommitter
	entity.IRemoteSnapshotLoader
	entity.IRemoteCommitOutbox
	entity.IRemoteStorageInitializer
}

// Backend combines the application-owned entity loader with the framework's
// transactional storage implementation. This is the only business-specific
// boundary required by RemoteEntityMod.
type Backend struct {
	loader  entity.IRemoteEntityLoader
	storage StorageBackend
}

func NewBackend(loader entity.IRemoteEntityLoader, storage StorageBackend) (*Backend, error) {
	if loader == nil || storage == nil {
		return nil, fmt.Errorf("remote_entity: loader and storage backend are required")
	}
	return &Backend{loader: loader, storage: storage}, nil
}

func (b *Backend) LoadRemoteEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	return b.loader.LoadRemoteEntity(ctx, id, kind)
}

func (b *Backend) LookupLocalRemoteEntity(id int64, kind entity.EntityKind) entity.IThreadSafeRemoteEntity {
	if local, ok := b.loader.(entity.IRemoteEntityLocalLookup); ok {
		return local.LookupLocalRemoteEntity(id, kind)
	}
	return nil
}

func (b *Backend) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	return b.storage.CommitRemote(ctx, commit)
}

func (b *Backend) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return b.storage.CommitRemoteBatch(ctx, commits)
}

func (b *Backend) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	return b.storage.CommitStatus(ctx, id)
}

func (b *Backend) LoadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return b.storage.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
}

func (b *Backend) PendingRemoteCommits(ctx context.Context, limit int) ([]entity.RemoteCommitStatus, error) {
	return b.storage.PendingRemoteCommits(ctx, limit)
}

func (b *Backend) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	return b.storage.MarkRemoteCommitPublished(ctx, id)
}

func (b *Backend) EnsureRemoteStorage(ctx context.Context) error {
	return b.storage.EnsureRemoteStorage(ctx)
}

func (b *Backend) ApplyRemoteCommitsInTransaction(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	store, ok := b.storage.(AtomicCommitStore)
	if !ok || store == nil {
		return nil, entity.ErrRemoteAtomicBatchUnsupported
	}
	return store.ApplyRemoteCommitsInTransaction(ctx, commits)
}

var _ entity.IRemoteEntityBackend = (*Backend)(nil)
var _ AtomicCommitStore = (*Backend)(nil)
