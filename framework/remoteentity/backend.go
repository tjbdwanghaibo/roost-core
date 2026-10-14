package remoteentity

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// StorageBackend 是框架的事务存储能力：原子批量提交、权威快照读取、outbox 与存储初始化。
type StorageBackend interface {
	entity.IRemoteAtomicBatchCommitter
	entity.IRemoteSnapshotLoader
	entity.IRemoteCommitOutbox
	entity.IRemoteStorageInitializer
}

// Backend 把应用持有的实体加载器与框架的事务存储实现组合在一起，是 RemoteEntityMod 唯一需要业务提供的边界。
type Backend struct {
	loader  entity.IRemoteEntityLoader
	storage StorageBackend
}

// NewBackend 组合加载器与存储，二者都必需。
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

// UnloadRemoteEntity 转发给 loader；loader 不支持卸载时返回 entity.ErrRemoteUnloadUnsupported。
func (b *Backend) UnloadRemoteEntity(ctx context.Context, e entity.IThreadSafeRemoteEntity) error {
	if unloader, ok := b.loader.(entity.IRemoteEntityUnloader); ok {
		return unloader.UnloadRemoteEntity(ctx, e)
	}
	return entity.ErrRemoteUnloadUnsupported
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

// SupportsConcurrentRemoteCommits 保留存储实现的并发能力；未知实现仍串行。
func (b *Backend) SupportsConcurrentRemoteCommits() bool {
	store, ok := b.storage.(interface{ SupportsConcurrentRemoteCommits() bool })
	return ok && store.SupportsConcurrentRemoteCommits()
}

// WriteAuthority 返回存储提供的持久写授权；存储不提供时为 nil。
func (b *Backend) WriteAuthority() WriteAuthority {
	if provider, ok := b.storage.(WriteAuthorityProvider); ok {
		return provider.WriteAuthority()
	}
	return nil
}

// RejectUnresolvedRemoteCommits 转发 Durability 0 未知结果的持久拒绝；存储不支持时
// 返回 ErrRemoteAtomicBatchUnsupported，finalizer 保持隔离并继续回源。
func (b *Backend) RejectUnresolvedRemoteCommits(ctx context.Context, commits []entity.RemoteCommit, cause string) (entity.RemoteCommitStatus, error) {
	store, ok := b.storage.(remoteUnresolvedRejecter)
	if !ok {
		return entity.RemoteCommitStatus{}, entity.ErrRemoteAtomicBatchUnsupported
	}
	return store.RejectUnresolvedRemoteCommits(ctx, commits, cause)
}

// PendingRemoteCommitPage 转发正式存储的分页能力；旧自定义存储仍按原接口逐页排空。
func (b *Backend) PendingRemoteCommitPage(ctx context.Context, after entity.RemoteOutboxCursor, limit int) ([]entity.RemoteCommitStatus, entity.RemoteOutboxCursor, error) {
	if pager, ok := b.storage.(entity.IRemoteCommitOutboxPager); ok {
		return pager.PendingRemoteCommitPage(ctx, after, limit)
	}
	pending, err := b.storage.PendingRemoteCommits(ctx, limit)
	return pending, entity.RemoteOutboxCursor{}, err
}
