package remoteentity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/mirror"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

// RemoteEntityMod implements app.Mod for remote entity lifecycle management.
// Depends on: redis mod (for versioned locks and marker storage).
type RemoteEntityMod struct {
	mgr         *remoteEntityManager
	cfg         *Config
	localSid    int32
	registry    *app.Registry
	snapshotRep *mirror.Replicator
	interestRep *mirror.Replicator
	backend     entity.IRemoteEntityBackend
	atomicStore AtomicCommitStore
	mongoLoader entity.IRemoteEntityLoader
	mongoConfig mongoBackendConfig
}

type mongoBackendConfig struct {
	database       string
	transactionTTL time.Duration
}

type ModOption func(*RemoteEntityMod)

func WithBackend(backend entity.IRemoteEntityBackend) ModOption {
	return func(mod *RemoteEntityMod) { mod.backend = backend }
}

// WithMongoStorage supplies the only application-specific boundary (entity
// loading) and lets the mod build the fenced transactional storage backend
// from the registered Mongo capability.
func WithMongoStorage(loader entity.IRemoteEntityLoader) ModOption {
	return func(mod *RemoteEntityMod) { mod.mongoLoader = loader }
}

func NewRemoteEntityMod(localSid int32, opts ...ModOption) *RemoteEntityMod {
	mod := &RemoteEntityMod{localSid: localSid}
	for _, opt := range opts {
		if opt != nil {
			opt(mod)
		}
	}
	return mod
}

func (m *RemoteEntityMod) Name() app.ModName { return mods.ModRemoteEntity }

func (m *RemoteEntityMod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	m.cfg = DefaultConfig()
	if m.localSid == 0 {
		m.localSid = cfg.GetInt32("sid")
	}

	if ttl := cfg.GetDuration("remote_entity.lock_ttl"); ttl > 0 {
		m.cfg.LockTTL = ttl
	}
	if key := cfg.GetString("remote_entity.lock_key"); key != "" {
		m.cfg.LockKey = key
	}
	if retry := cfg.GetInt("remote_entity.retry_count"); retry > 0 {
		m.cfg.RetryCount = retry
	}
	if delay := cfg.GetDuration("remote_entity.retry_delay"); delay > 0 {
		m.cfg.RetryDelay = delay
	}
	if timeout := cfg.GetDuration("remote_entity.op_timeout"); timeout > 0 {
		m.cfg.OpTimeout = timeout
	}
	if vttl := cfg.GetDuration("remote_entity.version_ttl"); vttl > 0 {
		m.cfg.VersionTTL = vttl
	}
	if uRetry := cfg.GetInt("remote_entity.unlock_retry_count"); uRetry > 0 {
		m.cfg.UnlockRetryCount = uRetry
	}
	if uInterval := cfg.GetDuration("remote_entity.unlock_retry_interval"); uInterval > 0 {
		m.cfg.UnlockRetryInterval = uInterval
	}
	if interval := cfg.GetDuration("remote_entity.finalize_retry_interval"); interval > 0 {
		m.cfg.FinalizeRetryInterval = interval
	}
	if limit := cfg.GetInt("remote_entity.max_write_batch"); limit > 0 {
		m.cfg.MaxWriteBatch = limit
	}
	if shards := cfg.GetInt("remote_entity.snapshot_cache_shards"); shards > 0 {
		m.cfg.SnapshotCacheShards = shards
	}
	if entries := cfg.GetInt("remote_entity.snapshot_cache_entries"); entries > 0 {
		m.cfg.SnapshotCacheEntries = entries
	}
	if bytes := cfg.GetInt64("remote_entity.snapshot_cache_bytes"); bytes > 0 {
		m.cfg.SnapshotCacheBytes = bytes
	}
	if ttl := cfg.GetDuration("remote_entity.snapshot_cache_ttl"); ttl > 0 {
		m.cfg.SnapshotCacheTTL = ttl
	}
	if ttl := cfg.GetDuration("remote_entity.snapshot_l2_ttl"); ttl > 0 {
		m.cfg.SnapshotL2TTL = ttl
	}
	if ttl := cfg.GetDuration("remote_entity.snapshot_interest_ttl"); ttl > 0 {
		m.cfg.SnapshotInterestTTL = ttl
	}
	if limit := cfg.GetInt("remote_entity.snapshot_interest_keys"); limit > 0 {
		m.cfg.SnapshotInterestKeys = limit
	}
	if limit := cfg.GetInt("remote_entity.snapshot_interest_subs"); limit > 0 {
		m.cfg.SnapshotInterestSubs = limit
	}
	if ttl := cfg.GetDuration("remote_entity.marker_cache_ttl"); ttl > 0 {
		m.cfg.MarkerCacheTTL = ttl
	}
	if timeout := cfg.GetDuration("remote_entity.snapshot_load_timeout"); timeout > 0 {
		m.cfg.SnapshotLoadTimeout = timeout
	}
	if limit := cfg.GetInt("remote_entity.snapshot_max_waiters"); limit > 0 {
		m.cfg.SnapshotMaxWaiters = limit
	}
	if capacity := cfg.GetInt("remote_entity.async_finalize_capacity"); capacity > 0 {
		m.cfg.AsyncFinalizeCapacity = capacity
	}
	if workers := cfg.GetInt("remote_entity.async_finalize_workers"); workers > 0 {
		m.cfg.AsyncFinalizeWorkers = workers
	}
	if limit := cfg.GetInt("remote_entity.transaction_track_limit"); limit > 0 {
		m.cfg.TransactionTrackLimit = limit
	}
	if ttl := cfg.GetDuration("remote_entity.transaction_track_ttl"); ttl > 0 {
		m.cfg.TransactionTrackTTL = ttl
	}
	if limit := cfg.GetInt("remote_entity.wrapper_capacity"); limit > 0 {
		m.cfg.WrapperCapacity = limit
	}
	if ttl := cfg.GetDuration("remote_entity.wrapper_idle_ttl"); ttl > 0 {
		m.cfg.WrapperIdleTTL = ttl
	}
	if m.localSid == 0 {
		return fmt.Errorf("remote_entity mod: non-zero sid is required for ownership fencing")
	}
	m.mongoConfig = mongoBackendConfig{
		database:       cfg.GetString("remote_entity.mongo.database"),
		transactionTTL: cfg.GetDuration("remote_entity.mongo.transaction_ttl"),
	}
	if m.mongoConfig.database == "" {
		m.mongoConfig.database = "remote_entity"
	}

	return nil
}

func (m *RemoteEntityMod) Provide(r *app.Registry) error {
	redis, ok := app.Lookup[fredis.IRedis](r, mods.ModRedis)
	if !ok {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModRedis)
	}
	lockFactory := newVersionedLockFactory(redis)

	m.mgr = newRemoteEntityManager(lockFactory, m.cfg, m.localSid, newRemoteSnapshotL2Store(redis, m.cfg.SnapshotL2TTL))
	if failure, ok := app.Lookup[*app.RuntimeFailure](r, app.ModRuntimeFailure); ok && failure != nil {
		m.mgr.setFatalHandler(func(err error) { failure.Fail(fmt.Errorf("remote_entity fatal release failure: %w", err)) })
	}
	if m.backend == nil && m.mongoLoader != nil {
		mongoClient, ok := app.Lookup[fmongo.IMongo](r, mods.ModMongo)
		if !ok || mongoClient == nil {
			return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModMongo)
		}
		storage := NewMongoCommitter(mongoClient, m.mongoConfig.database, m.localSid, m.mongoConfig.transactionTTL)
		backend, err := NewBackend(m.mongoLoader, storage)
		if err != nil {
			return err
		}
		m.backend = backend
	}
	if m.backend != nil {
		m.mgr.SetBackend(m.backend)
		m.atomicStore, _ = m.backend.(AtomicCommitStore)
		if m.atomicStore == nil {
			return fmt.Errorf("remote_entity mod: backend must support caller-owned atomic transactions")
		}
	}
	if m.atomicStore == nil {
		return fmt.Errorf("remote_entity mod: atomic storage backend is required")
	}

	// Set up the authoritative fenced ownership store (Redis-based).
	ownership := newRedisMarker(redis, "")
	m.mgr.SetOwnershipStore(ownership)

	// Register into app registry
	if err := mods.RegisterAll(r,
		mods.Capability{Name: mods.ModRemoteEntity, Value: entity.IRemoteEntityManager(m.mgr)},
		mods.Capability{Name: mods.ModRemoteEntityAtomicStore, Value: m.atomicStore},
		mods.Capability{Name: mods.ModRedisVLock, Value: fredis.IVersionedLockFactory(lockFactory)},
	); err != nil {
		return err
	}
	healthRegistry, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("remote_entity", health.CheckerFunc(m.checkHealth))
	m.registry = r
	return nil
}

func (m *RemoteEntityMod) DependsOn() []app.ModName {
	dependencies := []app.ModName{mods.ModRedis, mods.ModRoom, mods.ModHealth}
	if m != nil && m.mongoLoader != nil && m.backend == nil {
		dependencies = append(dependencies, mods.ModMongo)
	}
	return dependencies
}

func (m *RemoteEntityMod) checkHealth(context.Context) health.Result {
	if m == nil || m.mgr == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	if err := m.mgr.fatalError(); err != nil {
		return health.Result{Status: health.StatusFail, Message: "fatal release failure", Err: err}
	}
	m.mgr.remote.localInterestMu.Lock()
	localInterests := len(m.mgr.remote.localInterests)
	m.mgr.remote.localInterestMu.Unlock()
	m.mgr.remote.txMu.Lock()
	transactions := len(m.mgr.remote.txs)
	activeTransactions := 0
	for _, tracker := range m.mgr.remote.txs {
		if !tracker.closed {
			activeTransactions++
		}
	}
	m.mgr.remote.txMu.Unlock()
	if (m.cfg.SnapshotInterestKeys > 0 && localInterests >= m.cfg.SnapshotInterestKeys) || (m.cfg.TransactionTrackLimit > 0 && activeTransactions >= m.cfg.TransactionTrackLimit) {
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("capacity exhausted wrappers=%d local_interests=%d transactions=%d active_transactions=%d", m.mgr.wrapperCount(), localInterests, transactions, activeTransactions)}
	}
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("wrappers=%d capacity=%d local_interests=%d transactions=%d active_transactions=%d", m.mgr.wrapperCount(), m.cfg.WrapperCapacity, localInterests, transactions, activeTransactions)}
}

func (m *RemoteEntityMod) Start() error {
	if m == nil || m.mgr == nil {
		return fmt.Errorf("remote_entity mod: not provided")
	}
	if err := m.mgr.validateDependencies(); err != nil {
		return fmt.Errorf("remote_entity mod: %w", err)
	}
	if err := m.bindSyncer(); err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			m.stopReplicators()
		}
	}()
	m.mgr.sealDependencies()
	if initializer, ok := m.mgr.backend.(entity.IRemoteStorageInitializer); ok {
		storageCtx, cancel := context.WithTimeout(fctx.BaseContext(), m.cfg.OpTimeout)
		err := initializer.EnsureRemoteStorage(storageCtx)
		cancel()
		if err != nil {
			return err
		}
	}
	recoverCtx, cancel := context.WithTimeout(fctx.BaseContext(), m.cfg.OpTimeout)
	err := m.mgr.recoverRemoteOutbox(recoverCtx)
	cancel()
	if err != nil {
		return err
	}
	m.mgr.startRemoteFinalizer()
	started = true
	slog.Info("remote_entity mod: started",
		"sid", m.localSid,
		"lock_key", m.cfg.LockKey,
		"lock_ttl", m.cfg.LockTTL,
		"op_timeout", m.cfg.OpTimeout,
	)
	return nil
}

func (m *RemoteEntityMod) Stop() {
	if err := m.StopWithContext(fctx.BaseContext()); err != nil {
		slog.Warn("remote_entity mod: stop failed", "err", err)
	}
}

func (m *RemoteEntityMod) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	var err error
	if m.mgr != nil {
		err = m.mgr.stopRemoteFinalizer(ctx)
	}
	m.stopReplicators()
	slog.Info("remote_entity mod: stopped")
	return err
}

func (m *RemoteEntityMod) bindSyncer() error {
	if m.registry == nil {
		return fmt.Errorf("remote_entity mod: registry is not configured")
	}
	bus, ok := app.Lookup[fsyncbus.ISyncBus](m.registry, mods.ModRoom)
	if !ok {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModRoom)
	}
	snapshotRep := mirror.New(bus, syncTopicRemoteSnapshot, remoteSnapshotReplicaStore{mgr: m.mgr})
	interestRep := mirror.New(bus, syncTopicRemoteInterest, remoteInterestReplicaStore{mgr: m.mgr})
	syncer := newRemoteSyncer(snapshotRep)
	syncer.mgr = m.mgr
	syncer.interestRep = interestRep
	m.mgr.setSyncer(syncer)

	if err := snapshotRep.Start(); err != nil {
		return fmt.Errorf("remote_entity mod: start snapshot replica: %w", err)
	}
	if err := interestRep.Start(); err != nil {
		snapshotRep.Stop()
		return fmt.Errorf("remote_entity mod: start interest replica: %w", err)
	}
	m.snapshotRep = snapshotRep
	m.interestRep = interestRep
	return nil
}

func (m *RemoteEntityMod) stopReplicators() {
	if m.snapshotRep != nil {
		m.snapshotRep.Stop()
		m.snapshotRep = nil
	}
	if m.interestRep != nil {
		m.interestRep.Stop()
		m.interestRep = nil
	}
}
