package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// remoteEntityWrapper 是每个实体的内部协调单元：写门、所有权缓存、版本锁与当前实例。业务代码只能经 RemoteWriteBatch
// 与不可变快照 API 间接使用它。
type remoteEntityWrapper struct {
	id       int64
	category entity.EntityCategory
	kind     entity.EntityKind
	e        entity.IThreadSafeRemoteEntity
	rMu      redis.IVersionedLock
	entityMu sync.Mutex
	// rejectedStale 是因 Remote 写被持久拒绝而隔离、等待框架仅内存卸载并从权威重载的旧实例，
	// 与 e 同由 entityMu 保护。写准入见到它仍处于隔离时返回可重试的 entity.ErrRemoteEntityReloading，而不是通用的
	// ErrRemoteFenced；关联上别的实例或 loader 不支持卸载时清除。
	rejectedStale entity.IThreadSafeRemoteEntity
	// aligned / alignedLease 是独占持有的对齐记录，与 e 同由 entityMu 保护：aligned 这份实例在所有权租约 alignedLease
	// 之下与权威版本一致过（加载、取得归属、退出共享之后的第一次准入，或起服预加载 / 路由前置检查）。之后本进程是这个
	// 实体唯一的写者（App 单实例与 fail-stop 保证一个实体同一时刻只有一个 owner），只要实例与租约都没换，内存就是最新，
	// 写准入不再读写权威（heldExclusively）。所有权的任何变更都推进 MarkerEpoch，实例换代是新指针，两者都让记录失配。
	aligned      entity.IThreadSafeRemoteEntity
	alignedLease entity.RemoteEntityMarkerLease
	// writingTx 是当前持有写门、已定稿一笔提交的事务（与 e 同由 entityMu 保护），写门释放时清除。确认
	// （acknowledgeRemoteCommit）据此认出写者：独占写不再带每次递增的写许可 fence，不能再靠 fence 区分。
	writingTx   entity.RemoteTransactionID
	marker      atomic.Uint32
	markerAt    atomic.Int64
	markerLease entity.RemoteEntityMarkerLease
	markerMu    sync.RWMutex
	ownershipMu sync.RWMutex
	writeGate   chan struct{}
	mgr         *Manager
	refs        atomic.Int64
	lastUsed    atomic.Int64
}

const (
	markerUnknown uint32 = iota
	markerUnclaimed
	markerLocal
	markerShared
)

func newRemoteEntityWrapper(id int64, category entity.EntityCategory, kind entity.EntityKind, rMu redis.IVersionedLock, mgr *Manager) *remoteEntityWrapper {
	meta := resolveRemoteWrapperID(id, category, kind)
	w := &remoteEntityWrapper{
		id: meta.FullID, category: meta.Category, kind: meta.Kind,
		rMu: rMu, mgr: mgr, writeGate: make(chan struct{}, 1),
	}
	w.lastUsed.Store(time.Now().UnixNano())
	return w
}

func (w *remoteEntityWrapper) retain() {
	if w != nil {
		w.refs.Add(1)
		w.lastUsed.Store(time.Now().UnixNano())
	}
}

func (w *remoteEntityWrapper) release() {
	if w == nil {
		return
	}
	w.lastUsed.Store(time.Now().UnixNano())
	if w.refs.Add(-1) < 0 {
		panic("remote_entity: wrapper reference underflow")
	}
}

func (w *remoteEntityWrapper) isMarked() bool {
	return w != nil && w.marker.Load() == markerShared
}

func (w *remoteEntityWrapper) setOwnership(lease entity.RemoteEntityMarkerLease, found bool) {
	if w == nil {
		return
	}
	next := markerUnclaimed
	if found {
		next = markerLocal
	}
	if found && lease.Shared {
		next = markerShared
	}
	w.markerMu.Lock()
	w.markerLease = lease
	w.markerMu.Unlock()
	w.marker.Store(next)
	w.markerAt.Store(time.Now().UnixNano())
}

// invalidateMarker 丢弃缓存的租约，下一次准入必须询问权威（ensureMarker 把 markerUnknown 当作“立即刷新”）。所有权 CAS
// 结果未知时使用：早于这次 CAS 的热 marker 恰恰不能被信任。
func (w *remoteEntityWrapper) invalidateMarker() {
	if w == nil {
		return
	}
	w.marker.Store(markerUnknown)
	w.markerAt.Store(0)
}

func (w *remoteEntityWrapper) attachEntity(e entity.IThreadSafeRemoteEntity) {
	w.entityMu.Lock()
	w.e = e
	if w.rejectedStale != nil && w.rejectedStale != e {
		// 已换成从权威重载的新实例：重载窗口结束。
		w.rejectedStale = nil
	}
	w.entityMu.Unlock()
}

// markRejectedReload 记录 e 因持久拒绝被隔离、正等待框架卸载重载。
func (w *remoteEntityWrapper) markRejectedReload(e entity.IThreadSafeRemoteEntity) {
	if w == nil || e == nil {
		return
	}
	w.entityMu.Lock()
	w.rejectedStale = e
	w.entityMu.Unlock()
}

// clearRejectedReload 在不会由框架重载时（loader 不支持卸载）撤销标记，写准入回到通用的 ErrRemoteFenced。
func (w *remoteEntityWrapper) clearRejectedReload(e entity.IThreadSafeRemoteEntity) {
	if w == nil {
		return
	}
	w.entityMu.Lock()
	if w.rejectedStale == e {
		w.rejectedStale = nil
	}
	w.entityMu.Unlock()
}

// rejectedReloadPending 报告 e 是否是等待框架卸载重载的被拒绝实例。
func (w *remoteEntityWrapper) rejectedReloadPending(e entity.IThreadSafeRemoteEntity) bool {
	if w == nil || e == nil {
		return false
	}
	w.entityMu.Lock()
	pending := w.rejectedStale == e
	w.entityMu.Unlock()
	return pending
}

// heldExclusively 报告实体是否处于稳定的独占持有：持久权威（durable）下缓存的所有权是本 sid 独占，且当前托管的实例
// 就是在这份租约下对齐过的那份。是则返回它与租约，写准入不做任何 I/O。
//
// 缓存的租约在这里不按 MarkerCacheTTL 过期：独占实体的所有权只由 owner 进程自己改（认领、转移、进出共享都要求
// expected.OwnerSid == localSid，并持写门与所有权写锁串行），本进程的改动都同步写进缓存，结果未知时置为未知；单实例
// 前提下没有别的进程能改它。前提被打破时由投影的版本 / 代际条件兜底（REMOTE_ENTITY“独占写准入”）。
func (w *remoteEntityWrapper) heldExclusively(durable bool) (entity.IThreadSafeRemoteEntity, entity.RemoteEntityMarkerLease, bool) {
	if !durable || w.marker.Load() != markerLocal {
		return nil, entity.RemoteEntityMarkerLease{}, false
	}
	lease, found := w.cachedOwnership()
	if !found || lease.Shared || lease.OwnerSid != w.mgr.localSid || !validOwnershipLease(lease) {
		return nil, entity.RemoteEntityMarkerLease{}, false
	}
	w.entityMu.Lock()
	aligned, alignedLease := w.aligned, w.alignedLease
	w.entityMu.Unlock()
	if aligned == nil || alignedLease != lease || aligned.IsRemoved() || w.lookupLocalEntity() != aligned {
		return nil, entity.RemoteEntityMarkerLease{}, false
	}
	return aligned, lease, true
}

// markAligned 记下 e 在独占租约 lease 下已与权威对齐（调用方持写门）。
func (w *remoteEntityWrapper) markAligned(e entity.IThreadSafeRemoteEntity, lease entity.RemoteEntityMarkerLease) {
	w.entityMu.Lock()
	w.aligned, w.alignedLease = e, lease
	w.entityMu.Unlock()
}

// setWriting 记下持有写门的这次准入定稿了事务 tx；clearWriting 在写门释放（或定稿回滚）时清除。
func (w *remoteEntityWrapper) setWriting(tx entity.RemoteTransactionID) {
	w.entityMu.Lock()
	w.writingTx = tx
	w.entityMu.Unlock()
}

func (w *remoteEntityWrapper) clearWriting() {
	w.entityMu.Lock()
	w.writingTx = entity.RemoteTransactionID{}
	w.entityMu.Unlock()
}

// writing 报告 tx 是不是当前持有写门、已定稿的那笔事务。
func (w *remoteEntityWrapper) writing(tx entity.RemoteTransactionID) bool {
	w.entityMu.Lock()
	defer w.entityMu.Unlock()
	return !tx.IsZero() && w.writingTx == tx
}

// detachEntity 在实例被仅内存卸载后解除关联；已换成新实例时不动。
func (w *remoteEntityWrapper) detachEntity(e entity.IThreadSafeRemoteEntity) {
	w.entityMu.Lock()
	if w.e == e {
		w.e = nil
	}
	if w.rejectedStale == e {
		// 卸载已完成，下一次访问直接从权威重载，不再需要按旧实例判断重载窗口。
		w.rejectedStale = nil
	}
	if w.aligned == e {
		w.aligned, w.alignedLease = nil, entity.RemoteEntityMarkerLease{}
	}
	w.entityMu.Unlock()
}

func (w *remoteEntityWrapper) attachedEntity() entity.IThreadSafeRemoteEntity {
	w.entityMu.Lock()
	e := w.e
	w.entityMu.Unlock()
	return e
}

func (w *remoteEntityWrapper) lookupLocalEntity() entity.IThreadSafeRemoteEntity {
	if w == nil || w.mgr == nil {
		return nil
	}
	if local, ok := w.mgr.backend.(entity.IRemoteEntityLocalLookup); ok && local != nil {
		return local.LookupLocalRemoteEntity(w.id, w.kind)
	}
	return w.attachedEntity()
}

func (w *remoteEntityWrapper) loadEntity(ctx context.Context) (entity.IThreadSafeRemoteEntity, error) {
	if w == nil || w.mgr == nil {
		return nil, entity.ErrRemoteRejected
	}
	if w.mgr.backend == nil {
		return nil, nil
	}
	if ctx == nil {
		return nil, entity.ErrRemoteRejected
	}
	return w.mgr.backend.LoadRemoteEntity(ctx, w.id, w.kind)
}

func (w *remoteEntityWrapper) unlockObserved(ctx context.Context, version int64) error {
	if w == nil || w.mgr == nil || w.rMu == nil {
		return entity.ErrRemoteReleaseIncomplete
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.mgr.cfg.OpTimeout)
		defer cancel()
	}
	err := w.rMu.UnlockWithRetry(ctx, version, w.mgr.cfg.VersionTTL, w.mgr.cfg.UnlockRetryCount, w.mgr.cfg.UnlockRetryInterval)
	if err != nil {
		w.mgr.recordReleaseFailure(errors.Join(entity.ErrRemoteReleaseIncomplete, err))
	}
	return err
}

func (w *remoteEntityWrapper) refreshMarked(ctx context.Context) error {
	if w.mgr.ownershipStore == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.mgr.cfg.OpTimeout)
		defer cancel()
	}
	lease, found, err := w.mgr.ownershipStore.GetOwnership(ctx, w.id)
	if err != nil {
		return err
	}
	w.setOwnership(lease, found)
	return nil
}

func (w *remoteEntityWrapper) ensureMarker(ctx context.Context) error {
	if !w.markerExpired() {
		return nil
	}
	return w.refreshMarked(ctx)
}

// markerExpired 报告缓存的所有权是否未知或超过 MarkerCacheTTL（非正值按 1s）。
func (w *remoteEntityWrapper) markerExpired() bool {
	ttl := w.mgr.cfg.MarkerCacheTTL
	if ttl <= 0 {
		ttl = time.Second
	}
	return w.marker.Load() == markerUnknown || time.Since(time.Unix(0, w.markerAt.Load())) >= ttl
}

func (w *remoteEntityWrapper) leaseOwner() int32 {
	w.markerMu.RLock()
	owner := w.markerLease.OwnerSid
	w.markerMu.RUnlock()
	return owner
}

func (w *remoteEntityWrapper) isLocalOwner() bool {
	owner := w.leaseOwner()
	return owner != 0 && owner == w.mgr.localSid
}
