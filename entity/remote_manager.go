package entity

import (
	"context"
	"errors"
)

var (
	// ErrRemotePersistenceIndeterminate means a remote save/delete returned an
	// error after the backend may already have accepted it. Callers must not
	// blindly replay the business command.
	ErrRemotePersistenceIndeterminate = errors.New("remote entity persistence outcome is indeterminate")
	// ErrRemoteReleaseIncomplete means state persistence completed but an
	// ownership/distributed guard could not be released normally.
	ErrRemoteReleaseIncomplete = errors.New("remote entity release is incomplete")
	// ErrRemoteUnloadUnsupported 表示 loader 不能把实例从本进程内存卸载（没有实现
	// IRemoteEntityUnloader）。被持久拒绝的实例只能保持隔离，直到业务自行重新加载。
	ErrRemoteUnloadUnsupported = errors.New("remote entity loader cannot unload a local instance")
)

// IRemoteEntityLoader materializes authoritative entities for the write path.
// Persistence is performed exclusively through IRemoteAtomicBatchCommitter;
// loaders must never expose an independent save/delete path.
type IRemoteEntityLoader interface {
	LoadRemoteEntity(context.Context, int64, EntityKind) (IThreadSafeRemoteEntity, error)
}

// IRemoteEntityLocalLookup is the zero-I/O fast path used while ownership or
// write gates are held. Implementations must never perform storage access.
type IRemoteEntityLocalLookup interface {
	LookupLocalRemoteEntity(int64, EntityKind) IThreadSafeRemoteEntity
}

// IRemoteEntityUnloader 由能够把 Remote 实例从本进程内存卸载的 loader 实现：只移除内存实例、
// 不删持久数据，下一次访问经同一 loader 从权威重新加载。Remote 事务被持久拒绝后，实例内存
// 仍留着被拒绝的修改（生成实体的 RollbackRemoteCommit 不恢复前像），框架据此换代（RR-20260926-39）。
// 与 DataEngine 驱逐（RR-20260926-30）同一语义，正式实现为 ManagerAccess.Unload。调用方在本地执行入口
// （Nest 快池，或未装配 Nest 时就地）调用；实现自行取得实体锁，不得阻塞等待 I/O。实例已不在内存或已被
// 别的实例替换时返回 nil。
type IRemoteEntityUnloader interface {
	UnloadRemoteEntity(context.Context, IThreadSafeRemoteEntity) error
}

// IRemoteEntityBackend is the complete authoritative capability set. Keeping
// it as one contract makes incomplete production wiring a compile-time error.
type IRemoteEntityBackend interface {
	IRemoteEntityLoader
	IRemoteAtomicBatchCommitter
	IRemoteSnapshotLoader
	IRemoteCommitOutbox
}

// IRemoteEntityOwnershipStore is the authoritative fenced ownership state.
// Absence is not local ownership: writers must first atomically claim it.
// Every state transition is a compare-and-swap so concurrent servers cannot
// create two owners or reuse an ownership generation.
type IRemoteEntityOwnershipStore interface {
	GetOwnership(ctx context.Context, id int64) (RemoteEntityMarkerLease, bool, error)
	ClaimOwnership(ctx context.Context, id int64, ownerSid int32) (RemoteEntityMarkerLease, error)
	EnterSharedExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease) (RemoteEntityMarkerLease, error)
	LeaveSharedExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease) (RemoteEntityMarkerLease, error)
	TransferExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease, newOwnerSid int32) (RemoteEntityMarkerLease, error)
}

// RemoteEntityMarkerLease identifies one ownership generation. Implementations
// should reject releases and writes made with an older fence.
type RemoteEntityMarkerLease struct {
	OwnerSid    int32
	MarkerEpoch uint64
	RouteEpoch  uint64
	Shared      bool
}

// IRemoteEntityManager is the single production Remote Entity contract. Writes
// are immutable transaction batches; reads are immutable scoped snapshots.
type IRemoteEntityManager interface {
	RemoteWriteBatchManager
	RemoteOwnershipManager
	RemoteSnapshotInterestManager
	RemoteSnapshotReader

	SetBackend(backend IRemoteEntityBackend)
	SetOwnershipStore(store IRemoteEntityOwnershipStore)
}
