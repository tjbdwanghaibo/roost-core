package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// defaultMirrorShutdownTimeout 是 remote_entity.mirror.shutdown_timeout 的缺省值：停止时在途的权威加载已被
// 取消，剩下的是 L2 调用（受 snapshot_load_timeout 约束，缺省 2s）与已准入的复制 handler。
const defaultMirrorShutdownTimeout = 5 * time.Second

// RemoteMirrorMod 是只读服务的 Remote 快照装配（Mirror 第 5 步，docs/feature/MIRROR-STEP-5-2026-10-06.md）。
//
// 它只装 remoteentity.SnapshotClient：读、兴趣续租、复制接收、权威回填与三步停机。没有写 Manager、Mongo 原子
// backend、分布式锁或 finalizer，注册表里唯一的能力是 entity.RemoteSnapshotReadOnly（mods.ModRemoteMirror），
// 业务用生成的 New<DTO>Reader(MirrorSource(registry)) 读 DTO。
//
// 依赖：redis（共享 L2，水位权威）、syncbus（JetStream 模式才有推送，普通 NATS 退化为按需读取）、
// mongo（缺省的只读权威 loader；用 WithMirrorLoader 换掉时不需要）。
type RemoteMirrorMod struct {
	localSid int32
	cfg      *coreremote.Config
	database string
	// shutdownTimeout 是声明给 App 的停机预算（remote_entity.mirror.shutdown_timeout）。
	shutdownTimeout time.Duration

	loader       entity.RemoteSnapshotLoader
	linearizable bool

	client   *coreremote.SnapshotClient
	registry *app.Registry
	// stopping 在第一次 StopWithContext 时置位（健康检查用；停没停完以 StopWithContext 的返回值为准）。
	stopping atomic.Bool
}

// MirrorOption 调整 RemoteMirrorMod 的装配。
type MirrorOption func(*RemoteMirrorMod)

// WithMirrorLoader 换掉缺省的只读 Mongo loader，例如同进程 owner 的 Backend.LoadRemoteSnapshot。
// linearizable 只在 loader 确实提供线性化读时为 true，否则 Linearizable 读返回 entity.ErrRemoteReadUnsupported。
func WithMirrorLoader(loader entity.RemoteSnapshotLoader, linearizable bool) MirrorOption {
	return func(mod *RemoteMirrorMod) { mod.loader, mod.linearizable = loader, linearizable }
}

// NewRemoteMirrorMod 返回只读装配。localSid 为 0 时取配置的 sid；它是兴趣的 consumer 身份，必须与 owner
// 以及其他只读服务都不同。
func NewRemoteMirrorMod(localSid int32, opts ...MirrorOption) *RemoteMirrorMod {
	mod := &RemoteMirrorMod{localSid: localSid}
	for _, opt := range opts {
		if opt != nil {
			opt(mod)
		}
	}
	return mod
}

// MirrorSource 返回注册表里的只读快照能力（RemoteMirrorMod 或同进程的 RemoteEntityMod 提供）。
// 在 Provide 之后（Mod 的 Start、服务的 Init）调用。
func MirrorSource(r *app.Registry) (entity.RemoteSnapshotReadOnly, error) {
	source, ok := app.Lookup[entity.RemoteSnapshotReadOnly](r, mods.ModRemoteMirror)
	if !ok || source == nil {
		return nil, fmt.Errorf("remote mirror: capability %q not found: add kit/remoteentity.NewRemoteMirrorMod (read-only service) or RemoteEntityMod (owner process)", mods.ModRemoteMirror)
	}
	return source, nil
}

func (m *RemoteMirrorMod) Name() app.ModName { return mods.ModRemoteMirror }

// Init 只读 remote_entity.* 的快照段（与 RemoteEntityMod 同一个读取函数，A4 严格读取）、
// remote_entity.mongo.database 与 remote_entity.mirror.shutdown_timeout。锁、提交、finalizer 的键不读。
func (m *RemoteMirrorMod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	if m.localSid == 0 {
		m.localSid = cfg.GetInt32("sid")
	}
	if m.localSid == 0 {
		return errors.New("remote_entity mirror mod: non-zero sid is required (it is the consumer identity of snapshot interests)")
	}
	read := app.NewConfigReader(cfg)
	m.cfg = coreremote.DefaultConfig()
	if err := readSnapshotConfig(cfg, read, m.cfg); err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	m.shutdownTimeout = defaultMirrorShutdownTimeout
	if cfg.IsSet("remote_entity.mirror.shutdown_timeout") {
		timeout := read.Duration("remote_entity.mirror.shutdown_timeout")
		if err := read.Err(); err != nil {
			return fmt.Errorf("remote_entity mirror mod: %w", err)
		}
		if timeout <= 0 {
			return fmt.Errorf("remote_entity mirror mod: remote_entity.mirror.shutdown_timeout must be positive, got %v", cfg.Get("remote_entity.mirror.shutdown_timeout"))
		}
		m.shutdownTimeout = timeout
	}
	m.database = cfg.GetString("remote_entity.mongo.database")
	if m.database == "" {
		m.database = "remote_entity"
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	return nil
}

func (m *RemoteMirrorMod) DependsOn() []app.ModName {
	dependencies := []app.ModName{mods.ModRedis, mods.ModSyncBus}
	if m != nil && m.loader == nil {
		dependencies = append(dependencies, mods.ModMongo)
	}
	return dependencies
}

// Provide 建客户端（未启动）并登记只读能力与健康项。
func (m *RemoteMirrorMod) Provide(r *app.Registry) error {
	if m == nil || m.cfg == nil {
		return errors.New("remote_entity mirror mod: not initialized")
	}
	redis, ok := app.Lookup[fredis.IRedis](r, mods.ModRedis)
	if !ok || redis == nil {
		return fmt.Errorf("remote_entity mirror mod: required capability %q not found", mods.ModRedis)
	}
	l2, err := coreremote.NewSnapshotL2StoreFromConfig(redis, m.cfg)
	if err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	loader, linearizable := m.loader, m.linearizable
	if loader == nil {
		mongoClient, ok := app.Lookup[fmongo.IMongo](r, mods.ModMongo)
		if !ok || mongoClient == nil {
			return fmt.Errorf("remote_entity mirror mod: required capability %q not found", mods.ModMongo)
		}
		loader, linearizable = coreremote.NewMongoSnapshotLoader(mongoClient, m.database), false
	}
	client, err := coreremote.NewSnapshotClient(m.cfg, coreremote.SnapshotClientDeps{
		L2: l2, Loader: loader, LinearizableLoader: linearizable, ConsumerSID: m.localSid,
	})
	if err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	healthRegistry, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("remote_entity mirror mod: required capability %q not found", mods.ModHealth)
	}
	// 登记的是只读接口：类型断言拿不到发布 / 提交能力（SnapshotClient 本身也没有导出的写方法）。
	if err := r.Register(mods.ModRemoteMirror, entity.RemoteSnapshotReadOnly(client)); err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	healthRegistry.Register("remote_mirror", health.CheckerFunc(m.checkHealth))
	m.client, m.registry, m.linearizable = client, r, linearizable
	return nil
}

// Start 用 kit/syncbus 的总线启动客户端：JetStream 上开推送，普通 NATS 上不订阅快照推送、记 Warn（按需读取）。
func (m *RemoteMirrorMod) Start() error {
	if m == nil || m.client == nil || m.registry == nil {
		return errors.New("remote_entity mirror mod: not provided")
	}
	bus, ok := app.Lookup[fsyncbus.ISyncBus](m.registry, mods.ModSyncBus)
	if !ok || bus == nil {
		return fmt.Errorf("remote_entity mirror mod: required capability %q not found", mods.ModSyncBus)
	}
	if err := m.client.Start(bus); err != nil {
		return fmt.Errorf("remote_entity mirror mod: %w", err)
	}
	stats := m.client.Stats()
	slog.Info("remote_entity mirror mod: started", "sid", m.localSid, "snapshot_push", stats.PushEnabled,
		"cached_max_staleness", m.cfg.CachedMaxStaleness, "linearizable", m.linearizable)
	return nil
}

// StopBudget 声明 remote_entity.mirror.shutdown_timeout 为停机预算（app.ModStopBudgetProvider）。手写 Mod 的
// 预算不计入生成器算出的 shutdown.total_timeout（RR-20260926-66），装这个 Mod 的服务要手工留出这段时间。
func (m *RemoteMirrorMod) StopBudget() time.Duration {
	if m == nil {
		return 0
	}
	return m.shutdownTimeout
}

func (m *RemoteMirrorMod) Stop() {
	timeout := defaultMirrorShutdownTimeout
	if m != nil && m.shutdownTimeout > 0 {
		timeout = m.shutdownTimeout
	}
	ctx, cancel := context.WithTimeout(fctx.BaseContext(), timeout)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Warn("remote_entity mirror mod: stop failed", "err", err)
	}
}

// StopWithContext 是客户端的三步停机：关读准入、取消在途的权威加载、退订；在 ctx 内等已准入的工作返回；
// 返回 nil 之后 App 才停 redis / mongo / syncbus。ctx 到期返回 ctx 错误并保留客户端，重试会再等；
// 已停完之后再调用返回 nil。
func (m *RemoteMirrorMod) StopWithContext(ctx context.Context) error {
	if m == nil || m.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	m.stopping.Store(true)
	if err := m.client.Stop(ctx); err != nil {
		slog.Warn("remote_entity mirror mod: stop incomplete", "err", err)
		return err
	}
	slog.Info("remote_entity mirror mod: stopped")
	return nil
}

// checkHealth 报告推送模式与拒绝计数。推送关闭（普通 NATS）与兴趣被拒都是显式的按需读取，不算故障；
// 本机兴趣表满时新 key 不再有推送，报 Degraded。停止之后报 Fail。
func (m *RemoteMirrorMod) checkHealth(context.Context) health.Result {
	if m == nil || m.client == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	stats := m.client.Stats()
	message := fmt.Sprintf("snapshot_push=%v interest_refused=%d local_interests=%d interest_capacity=%d bootstrap_overflows=%d",
		stats.PushEnabled, stats.InterestRejected, stats.LocalInterests, m.cfg.SnapshotInterestKeys, stats.Bootstrap.Overflows)
	if m.stopping.Load() {
		return health.Result{Status: health.StatusFail, Message: "stopped " + message}
	}
	if m.cfg.SnapshotInterestKeys > 0 && stats.LocalInterests >= m.cfg.SnapshotInterestKeys {
		return health.Result{Status: health.StatusDegraded, Message: "local interest table full " + message}
	}
	return health.Result{Status: health.StatusOK, Message: message}
}

var (
	_ app.Mod                   = (*RemoteMirrorMod)(nil)
	_ app.ModStopperWithContext = (*RemoteMirrorMod)(nil)
	_ app.ModStopBudgetProvider = (*RemoteMirrorMod)(nil)
)
