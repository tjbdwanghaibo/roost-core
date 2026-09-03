package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
)

type remoteSyncTransport interface {
	entity.IRemoteSnapshotPublisher
	PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error
}

// remoteEntityManager is the single transaction/snapshot implementation.
// Wrappers are private coordination cells and are not exposed as an alternate
// persistence API.
type remoteEntityManager struct {
	mu             sync.RWMutex
	wrappers       map[int64]*remoteEntityWrapper
	creating       map[int64]*remoteWrapperCreate
	lockFactory    redis.IVersionedLockFactory
	backend        entity.IRemoteEntityBackend
	ownershipStore entity.IRemoteEntityOwnershipStore
	syncer         remoteSyncTransport
	cfg            *Config
	localSid       int32
	remote         *remoteState
	sealed         bool
	fatalMu        sync.RWMutex
	fatalErr       error
	onFatal        func(error)
}

func (m *remoteEntityManager) setFatalHandler(handler func(error)) {
	if m == nil {
		return
	}
	m.fatalMu.Lock()
	m.onFatal = handler
	m.fatalMu.Unlock()
}

func (m *remoteEntityManager) recordReleaseFailure(err error) {
	if m == nil || err == nil {
		return
	}
	m.fatalMu.Lock()
	first := m.fatalErr == nil
	m.fatalErr = errors.Join(m.fatalErr, err)
	handler := m.onFatal
	m.fatalMu.Unlock()
	metrics.IncCounter("remote_entity.release_failure_total", nil, 1)
	if first && handler != nil {
		handler(err)
	}
}

func (m *remoteEntityManager) fatalError() error {
	if m == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	m.fatalMu.RLock()
	defer m.fatalMu.RUnlock()
	return m.fatalErr
}

func (m *remoteEntityManager) wrapperCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.wrappers)
}

type remoteWrapperCreate struct {
	done    chan struct{}
	wrapper *remoteEntityWrapper
}

var _ entity.IRemoteEntityManager = (*remoteEntityManager)(nil)

func newRemoteEntityManager(lockFactory redis.IVersionedLockFactory, cfg *Config, localSid int32, snapshotL2 ...cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]) *remoteEntityManager {
	mgr := &remoteEntityManager{
		wrappers:    make(map[int64]*remoteEntityWrapper),
		creating:    make(map[int64]*remoteWrapperCreate),
		lockFactory: lockFactory,
		cfg:         cfg,
		localSid:    localSid,
	}
	mgr.remote = newRemoteState(mgr, cfg, snapshotL2...)
	return mgr
}

func (m *remoteEntityManager) getOrCreate(id int64, category entity.EntityCategory, kind entity.EntityKind) *remoteEntityWrapper {
	meta := resolveRemoteWrapperID(id, category, kind)
	if meta.FullID == 0 || m == nil || m.lockFactory == nil || m.cfg == nil {
		return nil
	}

	m.mu.RLock()
	w := m.wrappers[meta.FullID]
	if w != nil {
		w.retain()
		m.mu.RUnlock()
		return w
	}
	m.mu.RUnlock()

	m.mu.Lock()
	if existing := m.wrappers[meta.FullID]; existing != nil {
		existing.retain()
		m.mu.Unlock()
		return existing
	}
	if call := m.creating[meta.FullID]; call != nil {
		m.mu.Unlock()
		<-call.done
		m.mu.RLock()
		created := m.wrappers[meta.FullID]
		if created != nil {
			created.retain()
		}
		m.mu.RUnlock()
		return created
	}
	m.pruneWrappersLocked(time.Now())
	if m.cfg.WrapperCapacity > 0 && len(m.wrappers)+len(m.creating) >= m.cfg.WrapperCapacity {
		m.mu.Unlock()
		return nil
	}
	call := &remoteWrapperCreate{done: make(chan struct{})}
	m.creating[meta.FullID] = call
	m.mu.Unlock()

	opts := redis.VersionedLockOptions{
		Key:                m.cfg.LockKey,
		TTL:                m.cfg.LockTTL,
		RetryInterval:      m.cfg.RetryDelay,
		RetryCount:         m.cfg.RetryCount,
		AutoAsyncTouch:     true,
		AsyncTouchInterval: m.cfg.LockTTL / 3,
		AsyncTouchExtend:   m.cfg.LockTTL / 2,
	}
	created := newRemoteEntityWrapper(meta.FullID, meta.Category, meta.Kind, m.lockFactory.NewVersionedLock(meta.FullID, opts), m)
	created.retain()
	if err := created.refreshMarked(fctx.BaseContext()); err != nil {
		slog.Warn("remote_entity: initial marker refresh failed; shared path remains fail-closed", "id", meta.FullID, "err", err)
	}

	m.mu.Lock()
	m.wrappers[meta.FullID] = created
	call.wrapper = created
	delete(m.creating, meta.FullID)
	close(call.done)
	m.mu.Unlock()
	return created
}

func (m *remoteEntityManager) pruneWrappersLocked(now time.Time) {
	if m == nil || m.cfg == nil || len(m.wrappers) == 0 {
		return
	}
	cutoff := now.Add(-m.cfg.WrapperIdleTTL).UnixNano()
	for id, wrapper := range m.wrappers {
		if wrapper != nil && wrapper.refs.Load() == 0 && len(wrapper.writeGate) == 0 && wrapper.lastUsed.Load() <= cutoff {
			delete(m.wrappers, id)
		}
	}
	if m.cfg.WrapperCapacity <= 0 || len(m.wrappers)+len(m.creating) < m.cfg.WrapperCapacity {
		return
	}
	var oldestID int64
	oldestAt := int64(^uint64(0) >> 1)
	for id, wrapper := range m.wrappers {
		if wrapper != nil && wrapper.refs.Load() == 0 && len(wrapper.writeGate) == 0 && wrapper.lastUsed.Load() < oldestAt {
			oldestID, oldestAt = id, wrapper.lastUsed.Load()
		}
	}
	if oldestID != 0 {
		delete(m.wrappers, oldestID)
	}
}

func resolveRemoteWrapperID(id int64, category entity.EntityCategory, kind entity.EntityKind) entity.EntityIDMeta {
	fullID, err := entity.NormalizeFullID(id, kind)
	if err != nil {
		return entity.EntityIDMeta{}
	}
	meta := entity.ResolveEntityID(fullID)
	if category != entity.EntityCategoryNone && meta.Category != category {
		return entity.EntityIDMeta{}
	}
	return meta
}

func (m *remoteEntityManager) get(id int64) (*remoteEntityWrapper, bool) {
	m.mu.RLock()
	w, ok := m.wrappers[id]
	m.mu.RUnlock()
	return w, ok
}

func (m *remoteEntityManager) SetBackend(backend entity.IRemoteEntityBackend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.backend = backend
}

func (m *remoteEntityManager) SetOwnershipStore(store entity.IRemoteEntityOwnershipStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.ownershipStore = store
}

func (m *remoteEntityManager) setSyncer(syncer remoteSyncTransport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.syncer = syncer
}

func (m *remoteEntityManager) sealDependencies() {
	m.mu.Lock()
	m.sealed = true
	m.mu.Unlock()
}

func (m *remoteEntityManager) validateDependencies() error {
	if m == nil || m.cfg == nil || m.lockFactory == nil {
		return fmt.Errorf("remote_entity: manager is not initialized")
	}
	if m.backend == nil {
		return fmt.Errorf("remote_entity: authoritative backend is required")
	}
	if m.ownershipStore == nil {
		return fmt.Errorf("remote_entity: ownership store is required")
	}
	return nil
}
