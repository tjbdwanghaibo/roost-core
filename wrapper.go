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
	id          int64
	category    entity.EntityCategory
	kind        entity.EntityKind
	e           entity.IThreadSafeRemoteEntity
	rMu         redis.IVersionedLock
	entityMu    sync.Mutex
	marker      atomic.Uint32
	markerAt    atomic.Int64
	markerLease entity.RemoteEntityMarkerLease
	markerMu    sync.RWMutex
	ownershipMu sync.RWMutex
	writeGate   chan struct{}
	mgr         *remoteEntityManager
	refs        atomic.Int64
	lastUsed    atomic.Int64
}

const (
	markerUnknown uint32 = iota
	markerUnclaimed
	markerLocal
	markerShared
)

func newRemoteEntityWrapper(id int64, category entity.EntityCategory, kind entity.EntityKind, rMu redis.IVersionedLock, mgr *remoteEntityManager) *remoteEntityWrapper {
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

func (w *remoteEntityWrapper) attachEntity(e entity.IThreadSafeRemoteEntity) {
	w.entityMu.Lock()
	w.e = e
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
