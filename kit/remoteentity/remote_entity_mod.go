package remoteentity

import (
	"context"
	"fmt"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

// RemoteEntityMod implements app.Mod for remote entity lifecycle management.
// Depends on: redis mod (for versioned locks and marker storage). Core
// assembles the manager, backend and ownership store and owns the start /
// stop sequence; the Mod parses configuration, publishes the capabilities and
// forwards lifecycle calls (P3b).
type RemoteEntityMod struct {
	asm         *coreremote.Assembly
	cfg         *coreremote.Config
	localSid    int32
	registry    *app.Registry
	backend     entity.IRemoteEntityBackend
	mongoLoader entity.IRemoteEntityLoader
	mongoConfig coreremote.MongoBackendConfig
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
	m.cfg = coreremote.DefaultConfig()
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
	m.mongoConfig = coreremote.MongoBackendConfig{
		Database:       cfg.GetString("remote_entity.mongo.database"),
		TransactionTTL: cfg.GetDuration("remote_entity.mongo.transaction_ttl"),
	}
	if m.mongoConfig.Database == "" {
		m.mongoConfig.Database = "remote_entity"
	}

	return nil
}

func (m *RemoteEntityMod) Provide(r *app.Registry) error {
	redis, ok := app.Lookup[fredis.IRedis](r, mods.ModRedis)
	if !ok {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModRedis)
	}
	deps := coreremote.AssemblyDeps{Redis: redis, Backend: m.backend, Loader: m.mongoLoader}
	if m.backend == nil && m.mongoLoader != nil {
		mongoClient, ok := app.Lookup[fmongo.IMongo](r, mods.ModMongo)
		if !ok || mongoClient == nil {
			return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModMongo)
		}
		deps.Mongo = mongoClient
	}
	if failure, ok := app.Lookup[*app.RuntimeFailure](r, app.ModRuntimeFailure); ok && failure != nil {
		deps.OnFatal = func(err error) { failure.Fail(fmt.Errorf("remote_entity fatal release failure: %w", err)) }
	}
	asm, err := coreremote.Assemble(deps, m.cfg, m.localSid, m.mongoConfig)
	if err != nil {
		return err
	}
	m.asm = asm

	// Register into app registry
	if err := mods.RegisterAll(r,
		mods.Capability{Name: mods.ModRemoteEntity, Value: entity.IRemoteEntityManager(asm.Manager)},
		mods.Capability{Name: mods.ModRemoteEntityAtomicStore, Value: asm.AtomicStore},
		mods.Capability{Name: mods.ModRedisVLock, Value: asm.LockFactory},
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
	// Mods only: health is a registry built-in, not a Mod (roost-codegen U-0025).
	dependencies := []app.ModName{mods.ModRedis, mods.ModRoom}
	if m != nil && m.mongoLoader != nil && m.backend == nil {
		dependencies = append(dependencies, mods.ModMongo)
	}
	return dependencies
}

func (m *RemoteEntityMod) checkHealth(context.Context) health.Result {
	if m == nil || m.asm == nil || m.asm.Manager == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	mgr := m.asm.Manager
	if err := mgr.FatalError(); err != nil {
		return health.Result{Status: health.StatusFail, Message: "fatal release failure", Err: err}
	}
	stats := mgr.Stats()
	localInterests, transactions, activeTransactions := stats.LocalInterests, stats.Transactions, stats.ActiveTransactions
	if (m.cfg.SnapshotInterestKeys > 0 && localInterests >= m.cfg.SnapshotInterestKeys) || (m.cfg.TransactionTrackLimit > 0 && activeTransactions >= m.cfg.TransactionTrackLimit) {
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("capacity exhausted wrappers=%d local_interests=%d transactions=%d active_transactions=%d", stats.Wrappers, localInterests, transactions, activeTransactions)}
	}
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("wrappers=%d capacity=%d local_interests=%d transactions=%d active_transactions=%d", stats.Wrappers, m.cfg.WrapperCapacity, localInterests, transactions, activeTransactions)}
}

func (m *RemoteEntityMod) Start() error {
	if m == nil || m.asm == nil {
		return fmt.Errorf("remote_entity mod: not provided")
	}
	if m.registry == nil {
		return fmt.Errorf("remote_entity mod: registry is not configured")
	}
	bus, ok := app.Lookup[fsyncbus.ISyncBus](m.registry, mods.ModRoom)
	if !ok {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModRoom)
	}
	if err := m.asm.Start(fctx.BaseContext(), bus); err != nil {
		return fmt.Errorf("remote_entity mod: %w", err)
	}
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
	if m == nil || m.asm == nil {
		return nil
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	err := m.asm.Stop(ctx)
	slog.Info("remote_entity mod: stopped")
	return err
}
