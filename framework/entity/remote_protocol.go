package entity

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"time"
)

// Remote 实体写路径的错误。
var (
	ErrRemoteRejected                = errors.New("remote entity: write rejected before commit")
	ErrRemoteOverloaded              = errors.New("remote entity: capacity exceeded")
	ErrRemoteFenced                  = errors.New("remote entity: writer fenced")
	ErrRemoteOwnerTransition         = errors.New("remote entity: owner transition in progress")
	ErrRemoteVersionConflict         = errors.New("remote entity: state version conflict")
	ErrRemoteCommitTimeout           = errors.New("remote entity: commit timed out")
	ErrRemoteAtomicBatchUnsupported  = errors.New("remote entity: atomic batch unsupported")
	ErrRemoteWriteCapabilityDisabled = errors.New("remote entity: write capability disabled")
	ErrRemoteInvalidStateTransition  = errors.New("remote entity: invalid ownership transition")
	ErrRemoteCommitNotFinalized      = errors.New("remote entity: commit not finalized")
	// ErrRemoteEntityReloading 表示实例因 Remote 写被持久拒绝而隔离、框架正在把它仅内存卸载并从权威重新加载：
	// gate 释放到卸载完成之间的下一写者、卸载（Destroy）过程中发起的重载都得到它。
	// 这是可重试错误：卸载完成后的下一次访问得到从权威重载的新实例。它包裹 ErrRemoteFenced，
	// 既有 errors.Is(err, ErrRemoteFenced) 判断保持成立；需要区分“可重试的重载窗口”时先判断本哨兵。
	ErrRemoteEntityReloading = fmt.Errorf("%w: rejected instance is being unloaded and reloaded from the authority (retryable)", ErrRemoteFenced)
)

// RemoteOwnershipState 是实例的 Remote 所有权状态，按 ValidRemoteOwnershipTransition 转换。
type RemoteOwnershipState uint8

const (
	RemoteOwnershipUnknown RemoteOwnershipState = iota
	RemoteOwnershipLocalOwned
	RemoteOwnershipSharing
	RemoteOwnershipShared
	RemoteOwnershipDraining
	RemoteOwnershipFenced
	RemoteOwnershipRecovering
	RemoteOwnershipQuarantined
)

func (s RemoteOwnershipState) String() string {
	switch s {
	case RemoteOwnershipLocalOwned:
		return "local_owned"
	case RemoteOwnershipSharing:
		return "sharing"
	case RemoteOwnershipShared:
		return "shared"
	case RemoteOwnershipDraining:
		return "draining"
	case RemoteOwnershipFenced:
		return "fenced"
	case RemoteOwnershipRecovering:
		return "recovering"
	case RemoteOwnershipQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}

// ValidRemoteOwnershipTransition 报告所有权状态从 from 到 to 是否合法（相同状态总是合法）。
func ValidRemoteOwnershipTransition(from, to RemoteOwnershipState) bool {
	if from == to {
		return true
	}
	switch from {
	case RemoteOwnershipUnknown:
		return to == RemoteOwnershipRecovering || to == RemoteOwnershipLocalOwned || to == RemoteOwnershipShared
	case RemoteOwnershipLocalOwned:
		return to == RemoteOwnershipSharing || to == RemoteOwnershipDraining || to == RemoteOwnershipFenced
	case RemoteOwnershipSharing:
		return to == RemoteOwnershipShared || to == RemoteOwnershipLocalOwned || to == RemoteOwnershipFenced
	case RemoteOwnershipShared:
		return to == RemoteOwnershipDraining || to == RemoteOwnershipFenced
	case RemoteOwnershipDraining:
		return to == RemoteOwnershipLocalOwned || to == RemoteOwnershipShared || to == RemoteOwnershipFenced || to == RemoteOwnershipRecovering
	case RemoteOwnershipFenced:
		return to == RemoteOwnershipRecovering
	case RemoteOwnershipRecovering:
		return to == RemoteOwnershipLocalOwned || to == RemoteOwnershipShared || to == RemoteOwnershipQuarantined || to == RemoteOwnershipFenced
	case RemoteOwnershipQuarantined:
		return to == RemoteOwnershipRecovering
	default:
		return false
	}
}

// RemoteVersionVector 是 Remote 实体的四个独立版本维度：状态版本、marker 代际、锁 fence、路由代际。
type RemoteVersionVector struct {
	StateVersion uint64
	MarkerEpoch  uint64
	LockFence    uint64
	RouteEpoch   uint64
}

// RemoteWriteMode 是取得写权限的方式：经所有者路由，或共享模式下的分布式锁。
type RemoteWriteMode uint8

const (
	RemoteWriteOwnerRouted RemoteWriteMode = iota + 1
	RemoteWriteSharedLock
)

// RemoteWriteLease 是一次 Remote 写入取得的写权限。
type RemoteWriteLease struct {
	EntityID    int64
	OwnerSID    int32
	Mode        RemoteWriteMode
	State       RemoteOwnershipState
	BaseVersion uint64
	MarkerEpoch uint64
	LockFence   uint64
	RouteEpoch  uint64
	AcquiredAt  int64
	ExpiresAt   int64
}

// Valid 报告租约是否完整可用：有实体与代际，共享锁模式带 fence，状态为本地拥有或共享。
func (l RemoteWriteLease) Valid() bool {
	if l.EntityID == 0 || l.MarkerEpoch == 0 || l.RouteEpoch == 0 {
		return false
	}
	if l.Mode == RemoteWriteSharedLock && l.LockFence == 0 {
		return false
	}
	return l.State == RemoteOwnershipLocalOwned || l.State == RemoteOwnershipShared
}

type RemoteTransactionID [16]byte

func (id RemoteTransactionID) IsZero() bool   { return id == RemoteTransactionID{} }
func (id RemoteTransactionID) String() string { return fmt.Sprintf("%x", id[:]) }

// RemotePersistChange 是一个 DAO 变化中与持久化相关的部分。具体 patch 仍归 Nest；Remote 聚合有意在租约与实体版本
// fence 之下冻结完整文档。
type RemotePersistChange struct {
	Mask   uint64
	Delete bool
}

// RemotePersistChangeSource 只暴露正在 finalize 的事务所记录的变化。查询某个参与方即把它认领给 Remote 提交，
// 不再作为普通 mutation 输出。
type RemotePersistChangeSource interface {
	RemotePersistChangeFor(any) (RemotePersistChange, bool)
}

// RemoteDeleteIntentSource 显式暴露聚合删除意图。意图是事务内的，不依赖在持久 Remote 提交准入之前修改 IsRemoved。
type RemoteDeleteIntentSource interface {
	RemoteDeleteRequested(int64) bool
}

type RemoteTransactionOutcome struct {
	TransactionID  RemoteTransactionID
	Handler        string
	RequestID      string
	Succeeded      bool
	Durability     uint8
	FinalizedAt    int64
	PersistChanges RemotePersistChangeSource
	DeleteIntents  RemoteDeleteIntentSource
}

type RemoteSnapshotRecord struct {
	Key          RemoteSnapshotKey
	BaseVersion  uint64
	StateVersion uint64
	MarkerEpoch  uint64
	RouteEpoch   uint64
	Schema       uint32
	Codec        uint16
	Full         bool
	Data         []byte
	Checksum     RemoteChecksum
}

func (r RemoteSnapshotRecord) Clone() RemoteSnapshotRecord {
	r.Data = append([]byte(nil), r.Data...)
	return r
}

// RemoteDataMutation 是冻结的整文档写入。它有意不携带活的 DAO 指针与接口类型的 patch 值，WAL 记录因此是确定的，
// 可以由另一个进程或实现重放。
type RemoteDataMutation struct {
	Database      string
	DatabaseScope uint8
	Collection    string
	ID            int64
	Version       uint64
	Mask          uint64
	Data          []byte
}

type RemoteDataDelete struct {
	Database      string
	DatabaseScope uint8
	Collection    string
	ID            int64
}

func (d RemoteDataDelete) Validate() error {
	if d.Collection == "" || d.ID == 0 {
		return fmt.Errorf("%w: invalid data delete", ErrRemoteRejected)
	}
	return nil
}

func (m RemoteDataMutation) Clone() RemoteDataMutation {
	m.Data = append([]byte(nil), m.Data...)
	return m
}

func (m RemoteDataMutation) Validate() error {
	if m.Collection == "" || m.ID == 0 || m.Version == 0 || len(m.Data) == 0 {
		return fmt.Errorf("%w: invalid data mutation", ErrRemoteRejected)
	}
	return nil
}

// RemoteCommit 在 FinalizeLocked 返回后不可变。实现不得保留指向活的实体 / 组件 / DAO 内存的别名。
type RemoteCommit struct {
	TransactionID RemoteTransactionID
	EntityID      int64
	Kind          EntityKind
	Delete        bool
	BaseVersion   uint64
	NextVersion   uint64
	MarkerEpoch   uint64
	LockFence     uint64
	RouteEpoch    uint64
	Schema        uint32
	Codec         uint16
	Mutations     []RemoteDataMutation
	Deletes       []RemoteDataDelete
	Snapshots     []RemoteSnapshotRecord
	Invalidations []RemoteSnapshotKey
	Checksum      RemoteChecksum
}

func (c RemoteCommit) Clone() RemoteCommit {
	c.Mutations = slices.Clone(c.Mutations)
	for i := range c.Mutations {
		c.Mutations[i] = c.Mutations[i].Clone()
	}
	c.Snapshots = slices.Clone(c.Snapshots)
	c.Deletes = slices.Clone(c.Deletes)
	for i := range c.Snapshots {
		c.Snapshots[i] = c.Snapshots[i].Clone()
	}
	c.Invalidations = slices.Clone(c.Invalidations)
	return c
}

func (c RemoteCommit) Validate() error {
	if c.TransactionID.IsZero() || c.EntityID == 0 || c.Kind == EntityKindNone {
		return fmt.Errorf("%w: invalid identity", ErrRemoteRejected)
	}
	if c.NextVersion != c.BaseVersion+1 {
		return fmt.Errorf("%w: base=%d next=%d", ErrRemoteVersionConflict, c.BaseVersion, c.NextVersion)
	}
	if c.MarkerEpoch == 0 || c.RouteEpoch == 0 {
		return fmt.Errorf("%w: missing ownership epoch", ErrRemoteFenced)
	}
	if len(c.Mutations) == 0 && !c.Delete {
		return fmt.Errorf("%w: empty mutation set", ErrRemoteRejected)
	}
	if c.Delete && (len(c.Mutations) != 0 || len(c.Deletes) == 0) {
		return fmt.Errorf("%w: delete commit contains data mutations", ErrRemoteRejected)
	}
	if !c.Delete && len(c.Deletes) != 0 {
		return fmt.Errorf("%w: update commit contains data deletes", ErrRemoteRejected)
	}
	mutationKeys := make(map[string]struct{}, len(c.Mutations))
	for i := range c.Mutations {
		if err := c.Mutations[i].Validate(); err != nil {
			return fmt.Errorf("remote entity: mutation %d: %w", i, err)
		}
		if c.Mutations[i].ID != c.EntityID {
			return fmt.Errorf("%w: mutation identity mismatch", ErrRemoteRejected)
		}
		if c.Mutations[i].Version != c.NextVersion {
			return fmt.Errorf("%w: mutation version mismatch", ErrRemoteVersionConflict)
		}
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%d", c.Mutations[i].Database, c.Mutations[i].DatabaseScope, c.Mutations[i].Collection, c.Mutations[i].ID)
		if _, exists := mutationKeys[key]; exists {
			return fmt.Errorf("%w: duplicate data mutation", ErrRemoteRejected)
		}
		mutationKeys[key] = struct{}{}
	}
	for i := range c.Deletes {
		if err := c.Deletes[i].Validate(); err != nil || c.Deletes[i].ID != c.EntityID {
			return fmt.Errorf("remote entity: delete %d: %w", i, errors.Join(err, ErrRemoteRejected))
		}
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%d", c.Deletes[i].Database, c.Deletes[i].DatabaseScope, c.Deletes[i].Collection, c.Deletes[i].ID)
		if _, exists := mutationKeys[key]; exists {
			return fmt.Errorf("%w: duplicate data delete", ErrRemoteRejected)
		}
		mutationKeys[key] = struct{}{}
	}
	snapshotKeys := make(map[RemoteSnapshotKey]struct{}, len(c.Snapshots)+len(c.Invalidations))
	for i := range c.Snapshots {
		snapshot := &c.Snapshots[i]
		if !snapshot.Key.Valid() || snapshot.Key.EntityID != c.EntityID || snapshot.Key.Kind != c.Kind || snapshot.BaseVersion != c.BaseVersion || snapshot.StateVersion != c.NextVersion || snapshot.MarkerEpoch != c.MarkerEpoch || snapshot.RouteEpoch != c.RouteEpoch || snapshot.Schema == 0 || len(snapshot.Data) == 0 {
			return fmt.Errorf("%w: invalid snapshot %d", ErrRemoteRejected, i)
		}
		if snapshot.Checksum != 0 && RemoteSnapshotChecksum(snapshot.Data) != snapshot.Checksum {
			return fmt.Errorf("%w: snapshot checksum mismatch", ErrRemoteRejected)
		}
		if _, exists := snapshotKeys[snapshot.Key]; exists {
			return fmt.Errorf("%w: duplicate snapshot", ErrRemoteRejected)
		}
		snapshotKeys[snapshot.Key] = struct{}{}
	}
	for _, key := range c.Invalidations {
		if !key.Valid() || key.EntityID != c.EntityID || key.Kind != c.Kind {
			return fmt.Errorf("%w: invalid snapshot invalidation", ErrRemoteRejected)
		}
		if _, exists := snapshotKeys[key]; exists {
			return fmt.Errorf("%w: duplicate snapshot invalidation", ErrRemoteRejected)
		}
		snapshotKeys[key] = struct{}{}
	}
	return nil
}

type RemoteCommitReceipt struct {
	TransactionID RemoteTransactionID
	EntityID      int64
	StateVersion  uint64
	MarkerEpoch   uint64
	LockFence     uint64
	RouteEpoch    uint64
	CommittedAt   int64
}

type RemoteCommitState uint8

const (
	RemoteCommitUnknown RemoteCommitState = iota
	RemoteCommitAdmitted
	RemoteCommitApplied
	RemoteCommitPublished
	RemoteCommitCommitted
	RemoteCommitRejected
	RemoteCommitIndeterminate
)

type RemoteCommitStatus struct {
	TransactionID RemoteTransactionID
	State         RemoteCommitState
	Receipts      []RemoteCommitReceipt
	Commits       []RemoteCommit
	Cause         string
}

func (s RemoteCommitStatus) Clone() RemoteCommitStatus {
	s.Receipts = append([]RemoteCommitReceipt(nil), s.Receipts...)
	if len(s.Commits) > 0 {
		commits := make([]RemoteCommit, len(s.Commits))
		for i := range s.Commits {
			commits[i] = s.Commits[i].Clone()
		}
		s.Commits = commits
	}
	return s
}

// IRemoteCommitParticipant 由 Remote 托管实体的生成代码实现。Build 在持实体锁时调用，冻结事务内的变化；Acknowledge
// 只在权威 Remote 结论已知后执行；Rollback 不得要求业务代码管理实体锁。
//
// AcknowledgeRemoteCommit 必须幂等且并发安全：同一提交可能被确认多次，且可能并发——WAL 投影器报告
// “结果未知”后 finalizer 回源 Applied 自行发布，投影器对同一事务的重试同时也会确认；outbox 重放、Committed 收尾的对账
// 也会再次确认已确认过的提交。它不持有实体锁被调用，不能假设与 Build / Rollback 或另一次 Acknowledge 互斥。
// 生成实现经 dataengine.Tracker.AdvanceVersion（原子 CAS，版本已相等时直接成功）满足该契约；手写实现与测试替身须自行
// 保证（重复确认同一版本返回 nil、共享状态加锁或原子化）。
type IRemoteCommitParticipant interface {
	// HasRemoteCommitLocked 只读取本次事务的持久变更，不能使用跨事务的 Sync 脏位。
	HasRemoteCommitLocked(RemoteTransactionOutcome) bool
	BuildRemoteCommitLocked(RemoteWriteLease, RemoteTransactionOutcome) (RemoteCommit, error)
	AcknowledgeRemoteCommit(RemoteCommit) error
	RollbackRemoteCommit(RemoteCommit)
}

// IRemoteCommitter 以存储侧的版本与 fence 检查应用不可变提交。它必须按 TransactionID 幂等，并拒绝以不同提交内容复用
// TransactionID。Applied 状态必须包含在线 outbox 重放所需的不可变提交与回执。
type IRemoteCommitter interface {
	CommitRemote(context.Context, RemoteCommit) (RemoteCommitReceipt, error)
	CommitStatus(context.Context, RemoteTransactionID) (RemoteCommitStatus, error)
}

type IRemoteAtomicBatchCommitter interface {
	IRemoteCommitter
	CommitRemoteBatch(context.Context, []RemoteCommit) ([]RemoteCommitReceipt, error)
}

// IRemoteCommitOutbox 暴露已在存储中应用、但快照 / 事件发布被中断的提交。实现把这一状态与实体数据原子地持久化，
// 并保证 MarkRemoteCommitPublished 幂等。
type IRemoteCommitOutbox interface {
	PendingRemoteCommits(context.Context, int) ([]RemoteCommitStatus, error)
	MarkRemoteCommitPublished(context.Context, RemoteTransactionID) error
}

// RemoteOutboxCursor 是有序 outbox 的扫描位置；零值从头开始，不代表已发布水位。
type RemoteOutboxCursor struct {
	CreatedAt     time.Time
	TransactionID string
}

// IRemoteCommitOutboxPager 让恢复跨过本轮失败项继续扫描后续页。游标只在一次扫描内使用，
// 下次仍从头重试；不能以游标推进代替持久发布确认。正式 Mongo Backend 实现此接口。
type IRemoteCommitOutboxPager interface {
	PendingRemoteCommitPage(context.Context, RemoteOutboxCursor, int) ([]RemoteCommitStatus, RemoteOutboxCursor, error)
}

type IRemoteStorageInitializer interface {
	EnsureRemoteStorage(context.Context) error
}

// IRemoteSnapshotPublisher 发布 / 删除 Remote 快照（删除带版本）。
type IRemoteSnapshotPublisher interface {
	PublishRemoteSnapshot(context.Context, RemoteSnapshotRecord) error
	DeleteRemoteSnapshot(context.Context, RemoteSnapshotKey, uint64) error
}

// RemoteSnapshotScope 为生成的 DAO 视图给出稳定、非零的 scope ID。公开 API 仍应优先使用显式的协议 scope 常量。
func RemoteSnapshotScope(collection string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(collection))
	scope := h.Sum32()
	if scope == 0 {
		return 1
	}
	return scope
}

// RemoteSnapshotSchema 按实体 kind 与 scope 划分解码器命名空间。
func RemoteSnapshotSchema(kind EntityKind, scope uint32) uint32 {
	value := uint32(kind)*0x9e3779b1 ^ scope
	if value == 0 {
		return 1
	}
	return value
}

type IRemoteSnapshotLoader interface {
	LoadRemoteSnapshot(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error)
}

// RemoteWriteBatch 在 Nest 锁定实体之前准备。FinalizeLocked 在 handler 成功后、仍持实体锁时调用；Commit 在实体锁释放
// 之后、写门释放之前调用。
type RemoteWriteBatch interface {
	EntityIDs() []int64
	FinalizeLocked(RemoteTransactionOutcome) error
	Commits() []RemoteCommit
	Commit(context.Context) ([]RemoteCommitReceipt, error)
	Abort(context.Context, error) error
	Indeterminate(context.Context, error) error
	Close(context.Context) error
}

// RemoteOutcomeDeferrer 由能在后台收尾里拿到持久结论的 RemoteWriteBatch 实现。
// 本地事务已持久提交、而 Remote Commit 返回错误（确认超时 / 结果未知 / 明确拒绝）时，Nest 在 Close 之前
// 把提交后工作（Sync Confirm、AfterCommit）交给批次：返回 true 表示批次接手，fn 在拿到持久结论之后
// 至多调用一次——committed=true 为 Applied/Committed，false 为持久拒绝（此时批次已回滚、隔离、释放并
// 卸载了持有被拒绝修改的实例）。调用只经 NestMgr.RunLocal 在快池进行；Nest 未启动、停机中或已 fence 而拒绝投递时
// 不调用（不在后台 goroutine 上执行业务回调，Sync 门保持冻结，计数告警）。停机排空拿不到结论时同样不调用。
// 返回 false 表示批次不会再有持久结论（调用方保持原状，不得自行 Confirm）。
type RemoteOutcomeDeferrer interface {
	DeferUntilDurableOutcome(fn func(committed bool)) bool
}

type RemoteWriteBatchManager interface {
	PrepareRemoteWriteBatch(context.Context, []int64) (RemoteWriteBatch, error)
	FlushRemoteEntity(context.Context, int64, uint64) error
	FlushRemoteTransaction(context.Context, RemoteTransactionID) error
	FlushRemoteAll(context.Context) error
	RemoteCommitStatus(context.Context, RemoteTransactionID) (RemoteCommitStatus, error)
}

type RemoteOwnershipManager interface {
	GetRemoteOwnership(context.Context, int64) (RemoteEntityMarkerLease, bool, error)
	ClaimRemoteOwnership(context.Context, int64) (RemoteEntityMarkerLease, error)
	EnterRemoteSharedMode(context.Context, int64) (RemoteEntityMarkerLease, error)
	LeaveRemoteSharedMode(context.Context, int64) (RemoteEntityMarkerLease, error)
	TransferRemoteOwnership(context.Context, int64, int32) (RemoteEntityMarkerLease, error)
}

// RemoteSnapshotInterest 是可续约的软状态订阅。scope 与 policy 是键的一部分，支持生成的 LOD / 投影变体。
type RemoteSnapshotInterest struct {
	ConsumerSID int32
	Key         RemoteSnapshotKey
	ExpiresAt   int64
	// Generation 为同一消费者对同一键的续约与释放排序：由发布者分配、只增不减，释放只在其代际不旧于租约代际时取消
	// 该租约。ExpiresAt 担不了这个角色：合法的释放在它撤回的租约过期之前就会发布。
	Generation uint64
}

type RemoteSnapshotInterestManager interface {
	RenewRemoteSnapshotInterest(context.Context, RemoteSnapshotKey) error
	ReleaseRemoteSnapshotInterest(context.Context, RemoteSnapshotKey) error
}

// RemoteCommitApplier 提交并返回持久回执；成功不代表 outbox 已完成发布。
// 需要发布完成的写者继续通过 RemoteWriteBatch 的提交/确认契约等待。
type RemoteCommitApplier interface {
	ApplyRemoteCommits(context.Context, RemoteTransactionID, []RemoteCommit) ([]RemoteCommitReceipt, error)
}

type RemoteSnapshotReader interface {
	ReadRemoteSnapshot(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error)
}

// ValidateRemoteWriteBatchIDs 校验批次目标都是 Remote 托管 kind 的合法 ID，返回去重、升序的完整 ID。
func ValidateRemoteWriteBatchIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	normalized := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		meta := ResolveEntityID(id)
		if meta.FullID == 0 || meta.Kind == EntityKindNone || !IsEntityKindRemoteManaged(meta.Kind) {
			return nil, fmt.Errorf("%w: invalid remote entity %d", ErrRemoteRejected, id)
		}
		if _, ok := seen[meta.FullID]; ok {
			continue
		}
		seen[meta.FullID] = struct{}{}
		normalized = append(normalized, meta.FullID)
	}
	slices.Sort(normalized)
	return normalized, nil
}

// NewRemoteTransactionOutcome 构造事务结论，FinalizedAt 取当前时间。
func NewRemoteTransactionOutcome(id RemoteTransactionID, handler, requestID string, succeeded bool, durability uint8) RemoteTransactionOutcome {
	return RemoteTransactionOutcome{TransactionID: id, Handler: handler, RequestID: requestID, Succeeded: succeeded, Durability: durability, FinalizedAt: time.Now().UnixNano()}
}
