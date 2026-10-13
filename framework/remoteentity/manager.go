package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

type remoteSyncTransport interface {
	entity.IRemoteSnapshotPublisher
	PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error
}

// Manager 是唯一的 Remote 事务 / 快照实现。wrapper 是私有的协调单元，不作为另一套持久化 API 暴露。
type Manager struct {
	mu          sync.RWMutex
	wrappers    map[int64]*remoteEntityWrapper
	creating    map[int64]*remoteWrapperCreate
	lockFactory redis.IVersionedLockFactory
	// lockFactoryErr 在锁工厂发出的锁不带 fence 时设置（见 NewManager）；带着它的 Manager 不创建任何 wrapper。
	lockFactoryErr  error
	lockFactoryWarn sync.Once
	backend         entity.IRemoteEntityBackend
	ownershipStore  entity.IRemoteEntityOwnershipStore
	cfg             *Config
	localSid        int32
	remote          *remoteState
	// snapshots 是快照读、兴趣、复制接收与它们的生命周期。Manager 只组合并委托它，
	// owner 的提交后发布经它的包内入口 publishCommitted。
	snapshots *SnapshotClient
	sealed    bool
	fatalMu   sync.RWMutex
	fatalErr  error
	onFatal   func(error)
	// localExecutor 是 Nest 注入的本地执行入口（BindLocalExecutor → NestMgr.RunLocal），后台收尾据此回到业务池。
	localExecutor atomic.Pointer[func(func()) error]
}

// SetFatalHandler 设置首次释放失败时的致命错误回调（通常让进程 fail-stop）。
func (m *Manager) SetFatalHandler(handler func(error)) {
	if m == nil {
		return
	}
	m.fatalMu.Lock()
	m.onFatal = handler
	m.fatalMu.Unlock()
}

// recordReleaseFailure 累积释放失败：首次失败时调用致命回调，此后 FatalError 非 nil，Prepare 一律按 fence 拒绝。
func (m *Manager) recordReleaseFailure(err error) {
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

// FatalError 返回累积的释放失败；非 nil 时不再准入新的写批次。
func (m *Manager) FatalError() error {
	if m == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	m.fatalMu.RLock()
	defer m.fatalMu.RUnlock()
	return m.fatalErr
}

// WrapperCount 返回当前的 wrapper 数。
func (m *Manager) WrapperCount() int {
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

var _ entity.IRemoteEntityManager = (*Manager)(nil)

// NewManager 构造 Manager；snapshotL2 可选（最多取第一个）。锁工厂发出的锁不带 fence 时记下 LockFactoryError，
// 不创建任何 wrapper。后端与所有权存储另经 Set* 注入，并在启动前 SealDependencies。
func NewManager(lockFactory redis.IVersionedLockFactory, cfg *Config, localSid int32, snapshotL2 ...cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]) *Manager {
	mgr := &Manager{
		wrappers:    make(map[int64]*remoteEntityWrapper),
		creating:    make(map[int64]*remoteWrapperCreate),
		lockFactory: lockFactory,
		cfg:         cfg,
		localSid:    localSid,
	}
	// 在装配处、构造时一次性按失败关闭：Remote Entity 的写门需要锁在取得时分配的 fence（redis.IFencedVersionedLock）。
	// 普通版本锁的工厂会让每个共享操作在运行期被 ErrRemoteFenced 拒绝——结果正确，但发现得晚、噪声大，指标上也与
	// 真正的 fence 冲突无法区分。构造探测锁不做 I/O。
	if lockFactory != nil {
		probeKey, probeTTL := "e", time.Hour
		if cfg != nil {
			probeKey, probeTTL = cfg.LockKey, cfg.LockTTL
		}
		if _, ok := lockFactory.NewVersionedLock(1, redis.VersionedLockOptions{Key: probeKey, TTL: probeTTL}).(redis.IFencedVersionedLock); !ok {
			mgr.lockFactoryErr = fmt.Errorf("remote_entity: lock factory %T does not provide fenced locks (redis.IFencedVersionedLock); shared entities cannot be served", lockFactory)
		}
	}
	mgr.remote = newRemoteState(cfg)
	var l2 cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
	if len(snapshotL2) > 0 {
		l2 = snapshotL2[0]
	}
	// 权威 loader 晚绑定：backend 在 SetBackend 之后才有。Manager 的 backend 是写 owner 的权威存储，
	// 沿用它一直提供的 Linearizable 读。
	mgr.snapshots = newSnapshotClient(cfg, SnapshotClientDeps{
		L2: l2, ConsumerSID: localSid, LinearizableLoader: true,
		Loader: func(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			if mgr.backend == nil {
				return entity.RemoteSnapshotEnvelope{}, false, nil
			}
			return mgr.backend.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
		},
	})
	return mgr
}

// LockFactoryError 返回本 Manager 不会创建 wrapper 的原因，没有时为 nil。Mod 把它转成 Provide 失败，配置错误的部署
// 在启动时停下，而不是拒绝每一个共享操作。
func (m *Manager) LockFactoryError() error {
	if m == nil {
		return nil
	}
	return m.lockFactoryErr
}

func (m *Manager) getOrCreate(id int64, category entity.EntityCategory, kind entity.EntityKind) *remoteEntityWrapper {
	meta := resolveRemoteWrapperID(id, category, kind)
	if meta.FullID == 0 || m == nil || m.lockFactory == nil || m.cfg == nil {
		return nil
	}
	if m.lockFactoryErr != nil {
		m.lockFactoryWarn.Do(func() { slog.Error("remote_entity: refusing to create wrappers", "err", m.lockFactoryErr) })
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
	// 构造只建立本地协调状态；权威读取留给写入/所有权入口，使用本次调用的预算。
	// markerUnknown 让首次准入必须查询 Redis，不能把未读取误当成本地所有权。

	m.mu.Lock()
	m.wrappers[meta.FullID] = created
	call.wrapper = created
	delete(m.creating, meta.FullID)
	close(call.done)
	m.mu.Unlock()
	return created
}

func (m *Manager) pruneWrappersLocked(now time.Time) {
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

func (m *Manager) get(id int64) (*remoteEntityWrapper, bool) {
	m.mu.RLock()
	w, ok := m.wrappers[id]
	m.mu.RUnlock()
	return w, ok
}

// SetBackend 注入权威后端；依赖封存后调用 panic。
func (m *Manager) SetBackend(backend entity.IRemoteEntityBackend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.backend = backend
}

// SetOwnershipStore 注入所有权存储；依赖封存后调用 panic。
func (m *Manager) SetOwnershipStore(store entity.IRemoteEntityOwnershipStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.ownershipStore = store
}

// SetSyncer 注入快照与兴趣的发布通道；依赖封存后调用 panic。
func (m *Manager) SetSyncer(syncer remoteSyncTransport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed {
		panic("remote_entity: dependencies are sealed")
	}
	m.snapshots.transport = syncer
}

// SealDependencies 封存依赖，此后 Set* 调用 panic。
func (m *Manager) SealDependencies() {
	m.mu.Lock()
	m.sealed = true
	m.mu.Unlock()
}

// ValidateDependencies 确认锁工厂、权威后端与所有权存储都已注入。
func (m *Manager) ValidateDependencies() error {
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
