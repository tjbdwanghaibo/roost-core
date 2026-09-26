package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
)

// remoteEntityWrapper is an internal per-entity coordination cell. Business
// code can only reach it through RemoteWriteBatch and immutable snapshot APIs.
type remoteEntityWrapper struct {
	id       int64
	category entity.EntityCategory
	kind     entity.EntityKind
	e        entity.IThreadSafeRemoteEntity
	rMu      redis.IVersionedLock
	entityMu sync.Mutex
	// rejectedStale 是因 Remote 写被持久拒绝而隔离、等待框架仅内存卸载并从权威重载的旧实例（RR-20260926-62），
	// 与 e 同由 entityMu 保护。写准入见到它仍处于隔离时返回可重试的 entity.ErrRemoteEntityReloading，而不是通用的
	// ErrRemoteFenced；关联上别的实例或 loader 不支持卸载时清除。
	rejectedStale entity.IThreadSafeRemoteEntity
	marker        atomic.Uint32
	markerAt      atomic.Int64
	markerLease   entity.RemoteEntityMarkerLease
	markerMu      sync.RWMutex
	ownershipMu   sync.RWMutex
	writeGate     chan struct{}
	mgr           *Manager
	refs          atomic.Int64
	lastUsed      atomic.Int64
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

// invalidateMarker forgets the cached lease so the next admission must ask
// the authority (ensureMarker treats markerUnknown as "refresh now"). Used
// when an ownership CAS ended with an unknown outcome (U-0188): a hot marker
// that predates the CAS is exactly the thing that must not be trusted.
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

// markRejectedReload 记录 e 因持久拒绝被隔离、正等待框架卸载重载（RR-20260926-62）。
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

// detachEntity 在实例被仅内存卸载后解除关联；已换成新实例时不动（RR-20260926-39）。
func (w *remoteEntityWrapper) detachEntity(e entity.IThreadSafeRemoteEntity) {
	w.entityMu.Lock()
	if w.e == e {
		w.e = nil
	}
	if w.rejectedStale == e {
		// 卸载已完成，下一次访问直接从权威重载，不再需要按旧实例判断重载窗口。
		w.rejectedStale = nil
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
	ttl := w.mgr.cfg.MarkerCacheTTL
	if ttl <= 0 {
		ttl = time.Second
	}
	if w.marker.Load() != markerUnknown && time.Since(time.Unix(0, w.markerAt.Load())) < ttl {
		return nil
	}
	return w.refreshMarked(ctx)
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

func hasEntityDirty(e entity.IThreadSafeRemoteEntity) bool {
	guardable, ok := e.(entity.Guardable)
	if !ok || e == nil {
		return false
	}
	dirty := false
	guardable.RangeDao(func(dao entity.DaoInterface) {
		if !dirty {
			dirty = dao.Dirty() != nil && dao.Dirty().Dirty()
		}
	})
	return dirty
}
