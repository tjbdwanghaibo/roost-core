package entity

import (
	"context"
	"errors"
)

var (
	// ErrRemotePersistenceIndeterminate 表示 Remote 保存 / 删除在后端可能已接受之后返回了错误，调用方不能盲目重放业务命令。
	// RemoteWriteBatch.Commit 在本地事务已提交之后返回的错误，只有“Remote 确定没有写入”的明确拒绝可以不带它；Nest 按
	// “不带它 = 明确拒绝”给回复加 nest.ErrRemotePartRejected。等待 WAL 投影器结论的 Commit（strict）没等到
	// 结论就失败（等待截止、tracker 重新登记失败等）不说明 Remote 没写入，必须带它。
	ErrRemotePersistenceIndeterminate = errors.New("remote entity persistence outcome is indeterminate")
	// ErrRemotePublicationPending 与 ErrRemotePersistenceIndeterminate 一起返回：Remote 提交已在权威存储里
	// 持久（Applied），只是提交后的发布（缓存、同步总线、标记已发布）失败了。发布由 owner 的补发循环与
	// finalizer 按 outbox 补上，WAL 投影器据此把这条记为已投影，不再按它退避重试、挡住后面的记录。
	ErrRemotePublicationPending = errors.New("remote entity commit is persisted; publication is pending")
	// ErrRemoteReleaseIncomplete 表示状态已持久，但所有权 / 分布式保护未能正常释放。
	ErrRemoteReleaseIncomplete = errors.New("remote entity release is incomplete")
	// ErrRemoteUnloadUnsupported 表示 loader 不能把实例从本进程内存卸载（没有实现
	// IRemoteEntityUnloader）。被持久拒绝的实例只能保持隔离，直到业务自行重新加载。
	ErrRemoteUnloadUnsupported = errors.New("remote entity loader cannot unload a local instance")
)

// IRemoteEntityLoader 为写路径加载权威实体。持久化只经 IRemoteAtomicBatchCommitter 进行，加载器不得暴露独立的保存 / 删除路径。
type IRemoteEntityLoader interface {
	LoadRemoteEntity(context.Context, int64, EntityKind) (IThreadSafeRemoteEntity, error)
}

// IRemoteEntityLocalLookup 是持有所有权或写门时使用的零 I/O 快路径，实现绝不能访问存储。
type IRemoteEntityLocalLookup interface {
	LookupLocalRemoteEntity(int64, EntityKind) IThreadSafeRemoteEntity
}

// IRemoteEntityUnloader 由能够把 Remote 实例从本进程内存卸载的 loader 实现：只移除内存实例、
// 不删持久数据，下一次访问经同一 loader 从权威重新加载。Remote 事务被持久拒绝后，实例内存
// 仍留着被拒绝的修改（生成实体的 RollbackRemoteCommit 不恢复前像），框架据此换代。
// 与 DataEngine 驱逐同一语义，正式实现为 ManagerAccess.Unload。调用方在本地执行入口
// （Nest 业务池，或未装配 Nest 时就地）调用；实现自行取得实体锁，不得阻塞等待 I/O。实例已不在内存或已被
// 别的实例替换时返回 nil。
type IRemoteEntityUnloader interface {
	UnloadRemoteEntity(context.Context, IThreadSafeRemoteEntity) error
}

// IRemoteEntityBackend 是完整的权威能力集合。合成一个契约，让不完整的生产装配成为编译期错误。
type IRemoteEntityBackend interface {
	IRemoteEntityLoader
	IRemoteAtomicBatchCommitter
	IRemoteSnapshotLoader
	IRemoteCommitOutbox
}

// IRemoteEntityOwnershipStore 是带 fence 的权威所有权状态。没有记录不等于本地拥有：写者必须先原子地认领。每次状态
// 转换都是 CAS，并发的服务不会产生两个所有者，也不会复用所有权代际。
type IRemoteEntityOwnershipStore interface {
	GetOwnership(ctx context.Context, id int64) (RemoteEntityMarkerLease, bool, error)
	ClaimOwnership(ctx context.Context, id int64, ownerSid int32) (RemoteEntityMarkerLease, error)
	EnterSharedExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease) (RemoteEntityMarkerLease, error)
	LeaveSharedExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease) (RemoteEntityMarkerLease, error)
	TransferExpected(ctx context.Context, id int64, expected RemoteEntityMarkerLease, newOwnerSid int32) (RemoteEntityMarkerLease, error)
}

// RemoteEntityMarkerLease 标识一个所有权代际。实现应拒绝以更旧 fence 发起的释放与写入。
type RemoteEntityMarkerLease struct {
	OwnerSid    int32
	MarkerEpoch uint64
	RouteEpoch  uint64
	Shared      bool
}

// IRemoteEntityManager 是唯一的生产 Remote Entity 契约：写入是不可变的事务批次，读取是不可变的按 scope 快照。
type IRemoteEntityManager interface {
	RemoteWriteBatchManager
	RemoteOwnershipManager
	RemoteSnapshotInterestManager
	RemoteSnapshotReader

	SetBackend(backend IRemoteEntityBackend)
	SetOwnershipStore(store IRemoteEntityOwnershipStore)
}
