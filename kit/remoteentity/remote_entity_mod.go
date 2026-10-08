package remoteentity

import (
	"context"
	"fmt"
	"log/slog"

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

// ConfigSchema 声明 remote_entity.*（含与 Mirror 共用的快照段）与 Cluster 判断用的 redis.cluster_addrs。
func (m *RemoteEntityMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(entityConfig{}) }

func (m *RemoteEntityMod) Init(cfg *viper.Viper) error {
	var settings entityConfig
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("remote_entity mod: %w", err)
	}
	if m.localSid == 0 {
		m.localSid = settings.Sid
	}
	if m.localSid == 0 {
		return fmt.Errorf("remote_entity mod: non-zero sid is required for ownership fencing")
	}
	re := settings.RemoteEntity
	m.cfg = coreremote.DefaultConfig()
	setPositive(&m.cfg.LockTTL, re.LockTTL)
	if re.LockKey != "" {
		m.cfg.LockKey = re.LockKey
	}
	setPositive(&m.cfg.RetryCount, re.RetryCount)
	setPositive(&m.cfg.RetryDelay, re.RetryDelay)
	setPositive(&m.cfg.OpTimeout, re.OpTimeout)
	setPositive(&m.cfg.VersionTTL, re.VersionTTL)
	setPositive(&m.cfg.UnlockRetryCount, re.UnlockRetryCount)
	setPositive(&m.cfg.UnlockRetryInterval, re.UnlockRetryInterval)
	setPositive(&m.cfg.FinalizeRetryInterval, re.FinalizeRetryInterval)
	setPositive(&m.cfg.FinalizeProjectionTimeout, re.FinalizeProjectionTimeout)
	setPositive(&m.cfg.MaxWriteBatch, re.MaxWriteBatch)
	re.snapshotConfig.apply(m.cfg)
	setPositive(&m.cfg.AsyncFinalizeCapacity, re.AsyncFinalizeCapacity)
	m.cfg.MaxConcurrentWrites = re.MaxConcurrentWrites
	setPositive(&m.cfg.AsyncFinalizeWorkers, re.AsyncFinalizeWorkers)
	setPositive(&m.cfg.OutboxPublishWorkers, re.OutboxPublishWorkers)
	setPositive(&m.cfg.TransactionTrackLimit, re.TransactionTrackLimit)
	setPositive(&m.cfg.TransactionTrackTTL, re.TransactionTrackTTL)
	setPositive(&m.cfg.WrapperCapacity, re.WrapperCapacity)
	setPositive(&m.cfg.WrapperIdleTTL, re.WrapperIdleTTL)
	m.mongoConfig = coreremote.MongoBackendConfig{Database: re.Mongo.Database, TransactionTTL: re.Mongo.TransactionTTL}
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
	// O-M6-6：持有 App 单实例锁时，把它的身份交给 Remote，同 sid 重启后第一次取锁即接管上一代进程留下的
	// 共享锁。只在 Remote 的 sid 就是单实例锁的 sid 时传（NewRemoteEntityMod 显式给了别的 sid 时没有这个保证）；
	// singleton.enabled=false 时没有这个能力，不接管，照旧按 lock_ttl 等待。
	if incarnation, ok := app.Lookup[app.SingletonIncarnation](r, mods.ModSingletonIncarnation); ok && incarnation.Sid == m.localSid {
		deps.Incarnation = &coreremote.ProcessIncarnation{Holder: incarnation.Key, Token: incarnation.Token}
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
