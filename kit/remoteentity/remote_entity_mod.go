package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"

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
	// 严格读取（维护者决定 A4）：写错类型的值不再被读成 0 / 纳秒后取默认，返回前一并报出。
	read := app.NewConfigReader(cfg)
	if m.localSid == 0 {
		m.localSid = cfg.GetInt32("sid")
	}

	if ttl := read.Duration("remote_entity.lock_ttl"); ttl > 0 {
		m.cfg.LockTTL = ttl
	}
	if key := cfg.GetString("remote_entity.lock_key"); key != "" {
		m.cfg.LockKey = key
	}
	// 锁状态与 fence 在一个 Lua 中更新；Cluster 必须显式选择同槽前缀。
	// 不自动改 key，否则滚动发布时新旧节点会锁住不同身份。
	if len(mods.RedisClusterAddrs(cfg)) > 0 {
		start := strings.IndexByte(m.cfg.LockKey, '{')
		if start < 0 || strings.IndexByte(m.cfg.LockKey[start+1:], '}') <= 0 {
			return fmt.Errorf("remote_entity: Redis Cluster requires a non-empty hash tag in remote_entity.lock_key (for example {roost:remote}); got %q", m.cfg.LockKey)
		}
	}
	if retry := read.Int("remote_entity.retry_count"); retry > 0 {
		m.cfg.RetryCount = retry
	}
	if delay := read.Duration("remote_entity.retry_delay"); delay > 0 {
		m.cfg.RetryDelay = delay
	}
	if timeout := read.Duration("remote_entity.op_timeout"); timeout > 0 {
		m.cfg.OpTimeout = timeout
	}
	if vttl := read.Duration("remote_entity.version_ttl"); vttl > 0 {
		m.cfg.VersionTTL = vttl
	}
	if uRetry := read.Int("remote_entity.unlock_retry_count"); uRetry > 0 {
		m.cfg.UnlockRetryCount = uRetry
	}
	if uInterval := read.Duration("remote_entity.unlock_retry_interval"); uInterval > 0 {
		m.cfg.UnlockRetryInterval = uInterval
	}
	if interval := read.Duration("remote_entity.finalize_retry_interval"); interval > 0 {
		m.cfg.FinalizeRetryInterval = interval
	}
	if wait := read.Duration("remote_entity.finalize_projection_timeout"); wait > 0 {
		m.cfg.FinalizeProjectionTimeout = wait
	}
	if limit := read.Int("remote_entity.max_write_batch"); limit > 0 {
		m.cfg.MaxWriteBatch = limit
	}
	if err := readSnapshotConfig(cfg, read, m.cfg); err != nil {
		return err
	}
	if ttl := read.Duration("remote_entity.marker_cache_ttl"); ttl > 0 {
		m.cfg.MarkerCacheTTL = ttl
	}
	if capacity := read.Int("remote_entity.async_finalize_capacity"); capacity > 0 {
		m.cfg.AsyncFinalizeCapacity = capacity
	}
	if cfg.IsSet("remote_entity.max_concurrent_writes") {
		limit := read.Int("remote_entity.max_concurrent_writes")
		if limit < 0 {
			return errors.Join(read.Err(), errors.New("remote_entity.max_concurrent_writes must not be negative"))
		}
		m.cfg.MaxConcurrentWrites = limit
	}
	if workers := read.Int("remote_entity.async_finalize_workers"); workers > 0 {
		m.cfg.AsyncFinalizeWorkers = workers
	}
	if limit := read.Int("remote_entity.transaction_track_limit"); limit > 0 {
		m.cfg.TransactionTrackLimit = limit
	}
	if ttl := read.Duration("remote_entity.transaction_track_ttl"); ttl > 0 {
		m.cfg.TransactionTrackTTL = ttl
	}
	if limit := read.Int("remote_entity.wrapper_capacity"); limit > 0 {
		m.cfg.WrapperCapacity = limit
	}
	if ttl := read.Duration("remote_entity.wrapper_idle_ttl"); ttl > 0 {
		m.cfg.WrapperIdleTTL = ttl
	}
	if m.localSid == 0 {
		return fmt.Errorf("remote_entity mod: non-zero sid is required for ownership fencing")
	}
	m.mongoConfig = coreremote.MongoBackendConfig{
		Database:       cfg.GetString("remote_entity.mongo.database"),
		TransactionTTL: read.Duration("remote_entity.mongo.transaction_ttl"),
	}
	if m.mongoConfig.Database == "" {
		m.mongoConfig.Database = "remote_entity"
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("remote_entity mod: %w", err)
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
		// 同进程的只读读取与只读服务用同一个能力名（Mirror 第 5 步）：业务读 DTO 的代码在两处一样。
		// 只读的 RemoteMirrorMod 再装进同一进程会撞这个名字、启动即失败——同一 sid 不开第二个客户端。
		mods.Capability{Name: mods.ModRemoteMirror, Value: entity.RemoteSnapshotReadOnly(asm.Manager.SnapshotClient())},
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
	dependencies := []app.ModName{mods.ModRedis, mods.ModSyncBus}
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
	if stats.WriteLimit > 0 && stats.WritesInFlight >= stats.WriteLimit {
		return health.Result{Status: health.StatusDegraded, Message: fmt.Sprintf("write capacity exhausted writes_in_flight=%d write_limit=%d write_rejected=%d", stats.WritesInFlight, stats.WriteLimit, stats.WriteRejected)}
	}
	// snapshot_push=false 是显式退化（总线不能确认订阅，读取按需回源），interest_refused 是 O4 配额 / 表满
	// 拒绝的累计数（这些 key 按需读取）；两者都不影响健康状态。
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("wrappers=%d capacity=%d local_interests=%d transactions=%d active_transactions=%d writes_in_flight=%d write_limit=%d write_rejected=%d snapshot_push=%v interest_refused=%d", stats.Wrappers, m.cfg.WrapperCapacity, localInterests, transactions, activeTransactions, stats.WritesInFlight, stats.WriteLimit, stats.WriteRejected, stats.SnapshotPush, stats.InterestRefused)}
}

func (m *RemoteEntityMod) Start() error {
	if m == nil || m.asm == nil {
		return fmt.Errorf("remote_entity mod: not provided")
	}
	if m.registry == nil {
		return fmt.Errorf("remote_entity mod: registry is not configured")
	}
	bus, ok := app.Lookup[fsyncbus.ISyncBus](m.registry, mods.ModSyncBus)
	if !ok {
		return fmt.Errorf("remote_entity mod: required capability %q not found", mods.ModSyncBus)
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
	// 停止失败（ctx 到期时 finalizer / 复制 handler 还没排空）保留 Assembly，之后的 Stop 再等；只有真的
	// 停完才记 “stopped”（RR-20261005-NC-234）。
	if err := m.asm.Stop(ctx); err != nil {
		slog.Warn("remote_entity mod: stop incomplete", "err", err)
		return err
	}
	slog.Info("remote_entity mod: stopped")
	return nil
}

// readSnapshotConfig 读 remote_entity.* 的快照段（RemoteEntityMod 与只读的 RemoteMirrorMod 共用，A4 严格读取）：
// 缓存、共享 L2、陈旧上限、兴趣表、加载超时与等待者。类型错误攒在 read 里由调用方统一报出；值域错误直接返回。
func readSnapshotConfig(cfg *viper.Viper, read *app.ConfigReader, out *coreremote.Config) error {
	if shards := read.Int("remote_entity.snapshot_cache_shards"); shards > 0 {
		out.SnapshotCacheShards = shards
	}
	if entries := read.Int("remote_entity.snapshot_cache_entries"); entries > 0 {
		out.SnapshotCacheEntries = entries
	}
	if bytes := read.Int64("remote_entity.snapshot_cache_bytes"); bytes > 0 {
		out.SnapshotCacheBytes = bytes
	}
	if ttl := read.Duration("remote_entity.snapshot_cache_ttl"); ttl > 0 {
		out.SnapshotCacheTTL = ttl
	}
	if ttl := read.Duration("remote_entity.snapshot_l2_ttl"); ttl > 0 {
		out.SnapshotL2TTL = ttl
	}
	// B2：Cached / Monotonic 读能交出的快照距最近一次被共享 L2 或权威确认的最长时间。严格读取（A4）：
	// 不带单位的数字报错；设置了就必须为正；不配置时取 snapshot_cache_ttl。
	if cfg.IsSet("remote_entity.cached_max_staleness") {
		staleness := read.Duration("remote_entity.cached_max_staleness")
		if err := read.Err(); err != nil {
			return err
		}
		if staleness <= 0 {
			return fmt.Errorf("remote_entity.cached_max_staleness must be positive, got %v", cfg.Get("remote_entity.cached_max_staleness"))
		}
		out.CachedMaxStaleness = staleness
	}
	// 共享 L2 快照键的部署前缀（RR-20260927-17）。kit 里没有部署级的 Redis 前缀：服务各自用 <service>.key_prefix，
	// nats.prefix 只管 NATS 且缺省即 "roost"，remote_entity.lock_key 是锁身份（Cluster 下必须带 hash tag）。
	// 从后两者派生都会让已配置它们的部署升级后换键，所以单列一项、缺省为空：不配置时键与旧版本逐字相同。
	if prefix := cfg.GetString("remote_entity.snapshot_l2_key_prefix"); prefix != "" {
		if err := coreremote.ValidateSnapshotL2KeyPrefix(prefix); err != nil {
			return fmt.Errorf("remote_entity.snapshot_l2_key_prefix: %w", err)
		}
		out.SnapshotL2KeyPrefix = prefix
	}
	// O-M6-3：L2 写墓碑之后等几个副本确认、最多等多久（缺省 1 个、50ms；0 个关闭）。严格读取（A4）：写错类型
	// 报错；副本数不能为负；超时必须为正（Redis 的 WAIT … 0 是永久阻塞）且不超过 1s（调用方最多多等这么久）。
	if cfg.IsSet("remote_entity.snapshot_l2_tombstone_wait_replicas") {
		replicas := read.Int("remote_entity.snapshot_l2_tombstone_wait_replicas")
		if err := read.Err(); err != nil {
			return err
		}
		if replicas < 0 {
			return fmt.Errorf("remote_entity.snapshot_l2_tombstone_wait_replicas must not be negative, got %v", cfg.Get("remote_entity.snapshot_l2_tombstone_wait_replicas"))
		}
		out.SnapshotL2TombstoneWaitReplicas = replicas
	}
	if cfg.IsSet("remote_entity.snapshot_l2_tombstone_wait_timeout") {
		timeout := read.Duration("remote_entity.snapshot_l2_tombstone_wait_timeout")
		if err := read.Err(); err != nil {
			return err
		}
		if timeout <= 0 || timeout > coreremote.MaxSnapshotL2TombstoneWaitTimeout {
			return fmt.Errorf("remote_entity.snapshot_l2_tombstone_wait_timeout must be positive and at most %v, got %v", coreremote.MaxSnapshotL2TombstoneWaitTimeout, cfg.Get("remote_entity.snapshot_l2_tombstone_wait_timeout"))
		}
		out.SnapshotL2TombstoneWaitTimeout = timeout
	}
	if ttl := read.Duration("remote_entity.snapshot_interest_ttl"); ttl > 0 {
		out.SnapshotInterestTTL = ttl
	}
	if limit := read.Int("remote_entity.snapshot_interest_keys"); limit > 0 {
		out.SnapshotInterestKeys = limit
	}
	if limit := read.Int("remote_entity.snapshot_interest_subs"); limit > 0 {
		out.SnapshotInterestSubs = limit
	}
	// O4：每个 consumer 节点在兴趣表里的配额（0 = snapshot_interest_subs / 16），不能超过每节点上限。
	if cfg.IsSet("remote_entity.snapshot_interest_per_consumer") {
		quota := read.Int("remote_entity.snapshot_interest_per_consumer")
		if quota < 0 || quota > out.SnapshotInterestSubs {
			return errors.Join(read.Err(), fmt.Errorf("remote_entity.snapshot_interest_per_consumer must be between 0 and remote_entity.snapshot_interest_subs (%d), got %v", out.SnapshotInterestSubs, cfg.Get("remote_entity.snapshot_interest_per_consumer")))
		}
		out.SnapshotInterestPerConsumer = quota
	}
	if timeout := read.Duration("remote_entity.snapshot_load_timeout"); timeout > 0 {
		out.SnapshotLoadTimeout = timeout
	}
	if limit := read.Int("remote_entity.snapshot_max_waiters"); limit > 0 {
		out.SnapshotMaxWaiters = limit
	}
	return nil
}
