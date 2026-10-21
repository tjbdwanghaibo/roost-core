package dataengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod parses configuration, looks up the Mongo / JetStream / Remote Entity
// capabilities, hands them to core's engine.Assemble and forwards lifecycle
// calls. Construction order and failure rollback live in core.
type Mod struct {
	remoteEnabled bool
	access        *entity.ManagerAccess
	// resolver 是 WithDatabaseResolver 注册的映射回调，Init 时对每个声明的逻辑库各调用一次。
	resolver coredata.DatabaseResolver
	cfg      modConfig
	registry *app.Registry
	asm      *engine.Assembly

	fatalMu  sync.RWMutex
	fatalErr error

	// localExecutor 由 Nest 构造时绑定（mod 是 Nest 的 committer），转交给启动后才创建的 Projector。
	localExecutor atomic.Pointer[func(func()) error]
}

type ModOption func(*Mod)

func WithEntityAccess(access *entity.ManagerAccess) ModOption {
	return func(mod *Mod) { mod.access = access }
}

func WithRemoteProjection(enabled bool) ModOption {
	return func(mod *Mod) { mod.remoteEnabled = enabled }
}

// WithDatabaseResolver 注册逻辑库 → 物理库的回调（与配置 dataengine.databases 合并）：Init 时对代码声明的
// 每个逻辑库调用一次。同一逻辑库只能有一个来源：配置没写的逻辑库取回调返回的非空物理名；配置写了的，回调
// 返回空串或相同的值沿用配置，返回不同的值拒绝启动（报错点名逻辑库与两个值）。返回空串且配置也没写时用逻辑名
// 本身并告警；返回错误则拒绝启动。冲突只能在 Init 判断，--check-config 不调用回调、发现不了。运行中只查 Init
// 时冻结的表，不再调用它。
func WithDatabaseResolver(resolver func(logical string) (string, error)) ModOption {
	return func(mod *Mod) { mod.resolver = resolver }
}

type modConfig struct {
	mongo           engine.MongoStoreConfig // mongo.Databases 是 Init 时冻结的映射表
	wal             nestwal.Options
	projector       engine.ProjectorOptions
	outbox          engine.OutboxWorkerOptions
	effectPrefix    string
	effectStream    fnats.JetStreamConfig
	startupTimeout  time.Duration
	shutdownTimeout time.Duration
	pipelined       engine.PipelinedRuntimeConfig
}

func NewMod(options ...ModOption) *Mod {
	mod := &Mod{}
	for _, option := range options {
		if option != nil {
			option(mod)
		}
	}
	return mod
}

func (mod *Mod) Name() app.ModName { return mods.ModDataEngine }

// DependsOn names Mods only. The health registry this Mod publishes into is a
// built-in of every app.Registry, not a Mod, and app resolves dependencies by
// Mod NAME — so declaring mods.ModHealth here made every process that
// assembled the data engine fail at startup with
// `unknown mod dependency "health"` (found by the first generated project
// that was actually started).
func (mod *Mod) DependsOn() []app.ModName { return nil }

// OptionalDependsOn orders this Mod after the Mods whose capabilities it
// loads: Mongo, NATS (JetStream is the NATS Mod's capability, not a Mod of
// its own) and, when remote projection is on, Remote Entity.
func (mod *Mod) OptionalDependsOn() []app.ModName {
	// ModAccountServingGuard：合服后误启动的 game 在打开 WAL 与 outbox 之前被拒绝（wiring/account.ServingGuardMod）。
	deps := []app.ModName{mods.ModMongo, mods.ModNats, mods.ModAccountServingGuard}
	if mod != nil && mod.remoteEnabled {
		deps = append(deps, mods.ModRemoteEntity)
	}
	return deps
}

// EffectsConfig 是 dataengine.effects.* 的声明：效果流（ROOST_EFFECTS）的主题前缀、名字与保留期。
// saga Mod 与 SubscribeEffects 经 LoadEffectsConfig / EffectSettings 读同一份，两边对流的理解不会分叉。
type EffectsConfig struct {
	SubjectPrefix   string        `config:"subject_prefix" shared:"dataengine.effects.subject_prefix" default:"roost.effect" example:"{project}.effect"`
	Stream          string        `config:"stream" shared:"dataengine.effects.stream" default:"ROOST_EFFECTS" example:"{PROJECT}_EFFECTS"`
	MaxAge          time.Duration `config:"max_age" default:"168h" min:"1ns" example:"168h"`
	MaxBytes        int64         `config:"max_bytes" default:"8589934592" min:"1" example:"8589934592"`
	DuplicateWindow time.Duration `config:"duplicate_window" default:"10m" min:"1ns" example:"10m"`
	Replicas        int           `config:"replicas" default:"1" min:"1" example:"1"`
}

// config 是 wiring/dataengine 读的键（维护者决定）。数字与时长写 0 的取 core 缺省（声明里 min:"0" 的键）；
// 其余键的缺省值就是声明的 default，写 0 或负数拒绝。
type config struct {
	app.ServiceIdentity
	mods.PersistenceConfig
	Database              string          `config:"dataengine.database" default:"game" example:"{project_db}" help:"Data Engine 自己的集合（receipts / transactions / outbox）所在的 Mongo 库。生成配置按工程名填写；共用一个 Mongo 的不同工程必须不同"`
	Databases             DatabasesConfig `config:"dataengine.databases" example:"{main: {project_db}}" help:"DAO 逻辑库（标记里的 db=）→ Mongo 物理库。没写的逻辑库物理名就是逻辑名本身；生成配置把骨架的 main 映射到按工程名派生的库。\n只认代码声明过的逻辑库；物理名不能是系统库，按 sid 分库时加上 _<sid> 不超过 63 字节；两个逻辑库不能映射到同一个物理库。\nWAL 里有未投影记录时改映射会拒绝启动：先停写排空 WAL、迁移数据再改（docs/framework/PLAYER-MIGRATION.md）"`
	StartupTimeout        time.Duration   `config:"dataengine.startup_timeout" default:"30s" min:"1ns" example:"30s"`
	ShutdownTimeout       time.Duration   `config:"dataengine.shutdown_timeout" default:"30s" min:"1ns" example:"30s" help:"Stop budget declared to the App for draining WAL and projection; granted from\nshutdown.total_timeout after a 3s floor for each other Mod (scaled down with a warning\nonly when the rest cannot cover it). The generated shutdown.total_timeout counts it:\nraise that too when raising this."`
	TransactionReceiptTTL time.Duration   `config:"dataengine.transaction_receipt_ttl" default:"720h" min:"1ns" example:"720h"`
	ReceiptTTL            time.Duration   `config:"dataengine.receipt_ttl" default:"720h" min:"1ns" example:"720h"`
	WAL                   struct {
		Dir                 string        `config:"dir" help:"不写取 data/wal/dataengine/<sid>；显式写时只给一个进程用（见 dataengine.wal）"`
		SegmentBytes        int64         `config:"segment_bytes" min:"0" example:"268435456"`
		MaxDiskBytes        int64         `config:"max_disk_bytes" min:"0" example:"8589934592"`
		MaxUnackedAge       time.Duration `config:"max_unacked_age" min:"0" example:"24h"`
		QueueCapacity       int           `config:"queue_capacity" min:"0" example:"8192"`
		GroupCommitInterval time.Duration `config:"group_commit_interval" min:"0" example:"2ms"`
	} `config:"dataengine.wal" help:"dir 不写取 data/wal/dataengine/<sid>（相对进程工作目录），每个 sid 一个目录；生成的开发配置因此不写 dir。\n显式写的 dir 只能给一个进程用（WAL 对目录加独占锁，第二个进程启动即失败），同一份配置用 --sid 起多个进程时不要写"`
	Projection struct {
		RemoteWorkers      int           `config:"remote_workers" min:"1" max:"64"`
		BatchRecords       int           `config:"batch_records" min:"0" example:"256"`
		BatchBytes         int           `config:"batch_bytes" min:"0"`
		ReadBytes          int           `config:"read_bytes" min:"0"`
		RetryMin           time.Duration `config:"retry_min" min:"0" example:"100ms"`
		RetryMax           time.Duration `config:"retry_max" min:"0" example:"5s"`
		CheckpointRecords  int           `config:"checkpoint_records" min:"0"`
		CheckpointInterval time.Duration `config:"checkpoint_interval" min:"0"`
		MaxUnackedRecords  int64         `config:"max_unacked_records" min:"0"`
		WarnUnackedRecords int64         `config:"warn_unacked_records" min:"0"`
	} `config:"dataengine.projection"`
	Outbox struct {
		Owner           string        `config:"owner" help:"不写取 dataengine-<sid>"`
		Workers         int           `config:"workers" default:"2" min:"1" example:"2"`
		BatchSize       int           `config:"batch_size" default:"64" min:"1" example:"64"`
		LeaseDuration   time.Duration `config:"lease_duration" default:"30s" min:"1ns" example:"30s"`
		PollInterval    time.Duration `config:"poll_interval" default:"100ms" min:"1ns" example:"100ms"`
		RetryMin        time.Duration `config:"retry_min" default:"1s" min:"1ns" example:"1s"`
		RetryMax        time.Duration `config:"retry_max" default:"1m" min:"1ns" example:"1m"`
		MaxPending      int64         `config:"max_pending" min:"0" example:"1000000"`
		MaxOldestAge    time.Duration `config:"max_oldest_age" min:"0" example:"30m"`
		BacklogInterval time.Duration `config:"backlog_interval" default:"1s" min:"1ns"`
	} `config:"dataengine.outbox"`
	Effects   EffectsConfig `config:"dataengine.effects"`
	Pipelined struct {
		Allowlist          []string `config:"allowlist" example:"[]"`
		Async              bool     `config:"async" example:"false"`
		AsyncWorkers       int      `config:"async_workers" min:"0" example:"8"`
		AsyncQueueCapacity int      `config:"async_queue_capacity" min:"0" example:"4096"`
	} `config:"nest.pipelined"`
}

// DatabasesConfig 是 dataengine.databases：DAO 逻辑库名 → Mongo 物理库名。
type DatabasesConfig map[string]string

// ValidateConfig：告警水位不能高于硬上限；嵌入的持久化选择也要检查（自己定义了 ValidateConfig，提升的那个被遮住）；
// dataengine.databases 按本进程声明的逻辑库与 sid 校验（--check-config 与 doctor 走同一份规则，回调只在 Init 时调用）。
func (c *config) ValidateConfig(production bool) error {
	if err := c.PersistenceConfig.ValidateConfig(production); err != nil {
		return err
	}
	if err := coredata.ValidateDatabaseConfig(coredata.DeclaredDatabases(), c.Databases, c.Sid); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	if c.Projection.MaxUnackedRecords > 0 && c.Projection.WarnUnackedRecords > c.Projection.MaxUnackedRecords {
		return errors.New("dataengine mod: invalid projection checkpoint or backlog limits: warn_unacked_records exceeds max_unacked_records")
	}
	return (engine.AssemblyConfig{
		Mongo: engine.MongoStoreConfig{TransactionReceiptTTL: c.TransactionReceiptTTL},
		WAL:   nestwal.Options{MaxUnackedAge: c.WAL.MaxUnackedAge},
	}).ValidateReceiptRetention()
}

// ConfigSchema 声明 dataengine.*、nest.pipelined.* 与持久化引擎的选择。
func (mod *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (mod *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	sid := settings.Sid
	dir := walDir(settings.WAL.Dir, sid)
	wal := nestwal.DefaultOptions(dir)
	if value := settings.WAL.SegmentBytes; value > 0 {
		wal.SegmentBytes = value
	}
	if value := settings.WAL.QueueCapacity; value > 0 {
		wal.QueueCapacity = value
	}
	if value := settings.WAL.GroupCommitInterval; value > 0 {
		wal.GroupCommitInterval = value
	}
	if value := settings.WAL.MaxDiskBytes; value > 0 {
		wal.MaxDiskBytes = value
	}
	if value := settings.WAL.MaxUnackedAge; value > 0 {
		wal.MaxUnackedAge = value
	}
	wal.OnFatal = mod.onFatal
	projection := settings.Projection
	projector := engine.DefaultProjectorOptions()
	if projection.RemoteWorkers > 0 {
		projector.RemoteProjectionWorkers = projection.RemoteWorkers
	}
	if projection.RetryMin > 0 {
		projector.RetryMin = projection.RetryMin
	}
	if projection.RetryMax > 0 {
		projector.RetryMax = projection.RetryMax
	}
	if projection.BatchRecords > 0 {
		projector.ReplayBatchRecords = projection.BatchRecords
	}
	if projection.BatchBytes > 0 {
		projector.ReplayBatchBytes = projection.BatchBytes
	}
	if projection.ReadBytes > 0 {
		projector.ReplayReadBytes = projection.ReadBytes
	}
	if projection.CheckpointRecords > 0 {
		projector.CheckpointRecords = projection.CheckpointRecords
	}
	if projection.CheckpointInterval > 0 {
		projector.CheckpointInterval = projection.CheckpointInterval
	}
	projector.MaxUnackedRecords = uint64(projection.MaxUnackedRecords)
	projector.WarnUnackedRecords = uint64(projection.WarnUnackedRecords)
	projector.OnFatal = mod.onFatal
	owner := settings.Outbox.Owner
	if owner == "" {
		owner = fmt.Sprintf("dataengine-%d", sid)
	}
	outbox := engine.OutboxWorkerOptions{
		Owner: owner, Workers: settings.Outbox.Workers, BatchSize: settings.Outbox.BatchSize,
		LeaseDuration: settings.Outbox.LeaseDuration, PollInterval: settings.Outbox.PollInterval,
		RetryMin: settings.Outbox.RetryMin, RetryMax: settings.Outbox.RetryMax,
		MaxPending: settings.Outbox.MaxPending, MaxOldestAge: settings.Outbox.MaxOldestAge,
		BacklogInterval: settings.Outbox.BacklogInterval,
		OnHardLimit:     mod.onFatal,
	}
	databases, err := NewDatabaseMap(settings.Databases, sid, mod.resolver)
	if err != nil {
		return err
	}
	effects := settings.Effects
	prefix := effects.subjectPrefix()
	mod.cfg = modConfig{
		mongo: engine.MongoStoreConfig{DefaultDatabase: settings.Database, Databases: databases, ServerID: sid,
			TransactionReceiptTTL: settings.TransactionReceiptTTL, ReceiptTTL: settings.ReceiptTTL},
		wal: wal, projector: projector, outbox: outbox, effectPrefix: prefix,
		effectStream: fnats.JetStreamConfig{
			Name: effects.Stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge: effects.MaxAge, Duplicates: effects.DuplicateWindow, Replicas: effects.Replicas, MaxBytes: effects.MaxBytes,
		},
		startupTimeout:  settings.StartupTimeout,
		shutdownTimeout: settings.ShutdownTimeout,
		pipelined: engine.PipelinedRuntimeConfig{
			Allowlist: settings.Pipelined.Allowlist, Async: settings.Pipelined.Async,
			AsyncWorkers: settings.Pipelined.AsyncWorkers, AsyncQueueCap: settings.Pipelined.AsyncQueueCapacity,
		},
	}
	return nil
}

// subjectPrefix 是去掉首尾点的主题前缀；只写了点时取缺省。
func (c EffectsConfig) subjectPrefix() string {
	if prefix := strings.Trim(c.SubjectPrefix, "."); prefix != "" {
		return prefix
	}
	return "roost.effect"
}

func (mod *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return errors.New("dataengine mod: nil registry")
	}
	if mod.access == nil || mod.access.Manager() == nil {
		return errors.New("dataengine mod: service-scoped entity access is required")
	}
	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return fmt.Errorf("dataengine mod: capability %q not found", mods.ModMongo)
	}
	jetStream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetStream == nil {
		return fmt.Errorf("dataengine mod: capability %q not found", mods.ModNatsJetStream)
	}
	deps := engine.AssemblyDeps{Mongo: mongoClient, JetStream: jetStream, Access: mod.access, OnFatal: mod.onFatal, LocalExecutor: mod.runLocal}
	if mod.remoteEnabled {
		manager, ok := app.Lookup[entity.IRemoteEntityManager](registry, mods.ModRemoteEntity)
		if !ok || manager == nil {
			return fmt.Errorf("dataengine mod: capability %q not found", mods.ModRemoteEntity)
		}
		remoteStore, ok := app.Lookup[engine.RemoteProjectionStore](registry, mods.ModRemoteEntityAtomicStore)
		if !ok || remoteStore == nil {
			return fmt.Errorf("dataengine mod: capability %q not found", mods.ModRemoteEntityAtomicStore)
		}
		deps.RemoteManager, deps.RemoteStore = manager, remoteStore
	}
	// Remote 写 DAO 文档与投影用同一张冻结的表：engine.Assemble 经 MongoStore.SetRemoteProjection 绑给 Remote 存储
	// （Remote 存储先于本 Mod Provide，写入只在 Start 之后发生）；Remote 存储已绑另一张表时这里报错。
	asm, err := engine.Assemble(deps, engine.AssemblyConfig{
		Mongo: mod.cfg.mongo, WAL: mod.cfg.wal, Projector: mod.cfg.projector, Outbox: mod.cfg.outbox,
		EffectPrefix: mod.cfg.effectPrefix, EffectStream: mod.cfg.effectStream, Pipelined: mod.cfg.pipelined,
	})
	if err != nil {
		return err
	}
	mod.registry, mod.asm = registry, asm
	if err := mods.RegisterAll(registry,
		mods.Capability{Name: mods.ModDataEngine, Value: mod},
		mods.Capability{Name: mods.ModDataEngineDatabases, Value: mod.cfg.mongo.Databases},
	); err != nil {
		return err
	}
	healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("dataengine mod: capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("dataengine", health.CheckerFunc(mod.checkHealth))
	// entity.unload（D16）是运维动作，由框架挂到 App 的 admin 注册表（与 configdata.* 相同），经 ops 的 /admin/execute
	// 调用；Runtime 在 Start 之后才存在，命令执行时再取。
	commands, ok := app.Lookup[*admin.Registry](registry, mods.ModAdmin)
	if !ok || commands == nil {
		return fmt.Errorf("dataengine mod: capability %q not found", mods.ModAdmin)
	}
	if err := engine.RegisterAdminCommands(commands, mod.Runtime); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	return nil
}

func (mod *Mod) Start() error {
	if mod == nil || mod.asm == nil {
		return errors.New("dataengine mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), mod.cfg.startupTimeout)
	defer cancel()
	return mod.asm.Start(ctx)
}

func (mod *Mod) Stop() {
	if mod == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mod.cfg.shutdownTimeout)
	defer cancel()
	_ = mod.StopWithContext(ctx)
}

// StopBudget 声明 dataengine.shutdown_timeout 为 App 停机预算（app.ModStopBudgetProvider）：
// App 在 shutdown.total_timeout 扣除其他未声明 Mod 的固定保底后优先把这段时间给 StopWithContext
// 排空 WAL 与投影，不够时按比例缩放并告警。未 Init 时为 0，即不声明。
func (mod *Mod) StopBudget() time.Duration {
	if mod == nil {
		return 0
	}
	return mod.cfg.shutdownTimeout
}

func (mod *Mod) StopWithContext(ctx context.Context) error {
	if mod == nil {
		return nil
	}
	return mod.asm.Shutdown(ctx)
}

func (mod *Mod) Runtime() *engine.Runtime {
	if mod == nil {
		return nil
	}
	return mod.asm.Runtime()
}

// WaitEntityProjection 等待本进程 WAL 中该实体（完整 Entity ID）的在途投影落库，
// 是跨进程交出实体所有权前的屏障：另一进程接手时只从 Mongo
// 读取，本进程已确认但未投影的事务不会被它看到。只跟踪本进程的在途记录；
// 投影失败、Runtime 未就绪或 WAL 不健康都返回错误，调用方应保留所有权并重试。
// 会阻塞，只能在 io 池或独立 goroutine 调用，业务 worker 上调用会 fail-fast。
func (mod *Mod) WaitEntityProjection(ctx context.Context, entityID int64) error {
	runtime := mod.Runtime()
	if runtime == nil {
		return errors.New("dataengine mod: not provided")
	}
	return runtime.WaitEntityProjection(ctx, entityID)
}

func (mod *Mod) Repository() *engine.EntityRepository {
	runtime := mod.Runtime()
	if runtime == nil {
		return nil
	}
	return runtime.Repository
}
func (mod *Mod) NestOptions() []corenest.NestOption {
	if mod == nil {
		return nil
	}
	options := []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(mod)}
	if len(mod.cfg.pipelined.Allowlist) > 0 {
		options = append(options, corenest.NestOptionWithPipelinedAllowlist(mod.cfg.pipelined.Allowlist...))
	}
	if mod.cfg.pipelined.Async {
		options = append(options, corenest.NestOptionWithPipelinedAsyncCompletion(mod.cfg.pipelined.AsyncWorkers, mod.cfg.pipelined.AsyncQueueCap))
	}
	return options
}
func (mod *Mod) Flush(ctx context.Context) error {
	runtime := mod.Runtime()
	if runtime == nil {
		return nil
	}
	return runtime.Flush(ctx)
}

func (mod *Mod) Commit(ctx context.Context, record corenest.CommitRecord) error {
	runtime := mod.Runtime()
	if runtime == nil || !runtime.Ready() || runtime.Projector == nil {
		return corenest.ErrCommitterRequired
	}
	return runtime.Projector.Commit(ctx, record)
}

func (mod *Mod) Enqueue(ctx context.Context, record corenest.CommitRecord) (corenest.CommitTicket, error) {
	runtime := mod.Runtime()
	if runtime == nil || !runtime.Ready() || runtime.Projector == nil {
		return nil, corenest.ErrCommitterRequired
	}
	return runtime.Projector.Enqueue(ctx, record)
}

func (mod *Mod) DurableLSN() uint64 {
	runtime := mod.Runtime()
	if runtime == nil || runtime.Projector == nil {
		return 0
	}
	return runtime.Projector.DurableLSN()
}

func (mod *Mod) TransactionReleased(id corenest.TransactionID) {
	runtime := mod.Runtime()
	if runtime != nil && runtime.Projector != nil {
		runtime.Projector.TransactionReleased(id)
	}
}

// BindLocalExecutor 实现 corenest.LocalExecutorBinder：Nest 在构造时交来业务池执行入口，
// DataEngine 用它在业务池持锁驱逐被跳过的原生步骤留下的实体。
func (mod *Mod) BindLocalExecutor(run func(func()) error) {
	if mod != nil && run != nil {
		mod.localExecutor.Store(&run)
	}
}

// runLocal 是交给 Assembly 的转发：Nest 尚未绑定时（启动恢复阶段，还没有常驻实体）就地执行。
func (mod *Mod) runLocal(fn func()) error {
	if run := mod.localExecutor.Load(); run != nil {
		return (*run)(fn)
	}
	fn()
	return nil
}

// OnEntityLoaded 转发 engine.EntityRepository.OnEntityLoaded；Runtime 在 Start 之后才存在。
func (mod *Mod) OnEntityLoaded(hook func(entity.IThreadSafeEntity)) (func(), error) {
	repository := mod.Repository()
	if repository == nil {
		return nil, errors.New("dataengine mod: runtime is not started")
	}
	return repository.OnEntityLoaded(hook), nil
}

func (mod *Mod) onFatal(err error) {
	if err == nil {
		return
	}
	mod.fatalMu.Lock()
	if mod.fatalErr == nil {
		mod.fatalErr = err
	}
	mod.fatalMu.Unlock()
	slog.Error("dataengine: fatal storage outcome; process is fenced", "err", err)
	if mod.registry == nil {
		return
	}
	if manager, ok := app.Lookup[*corenest.NestMgr](mod.registry, mods.ModNest); ok && manager != nil {
		manager.Fence(err)
	}
	if failure, ok := app.Lookup[*app.RuntimeFailure](mod.registry, app.ModRuntimeFailure); ok && failure != nil {
		failure.Fail(fmt.Errorf("dataengine fatal storage outcome: %w", err))
	}
}

func (mod *Mod) checkHealth(ctx context.Context) health.Result {
	if mod == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	runtime := mod.Runtime()
	if runtime == nil || !runtime.Ready() {
		return health.Result{Status: health.StatusFail, Message: "not ready"}
	}
	mod.fatalMu.RLock()
	fatal := mod.fatalErr
	mod.fatalMu.RUnlock()
	if fatal != nil {
		return health.Result{Status: health.StatusFail, Message: "fenced", Err: fatal}
	}
	if err := errors.Join(runtime.Projector.Healthy(), runtime.Outbox.Healthy()); err != nil {
		return health.Result{Status: health.StatusFail, Message: "unhealthy", Err: err}
	}
	// The backlog gauge is sampled on an interval by the claim loop, so a
	// health probe takes its own reading rather than reporting one that may be
	// a whole interval stale. A probe failure is not a health failure on its
	// own — the last known values still get reported.
	if err := runtime.Outbox.RefreshBacklog(ctx); err != nil {
		slog.Warn("dataengine: outbox backlog probe failed during health check", "err", err)
	}
	stats := runtime.Projector.Stats()
	status := health.StatusOK
	if stats.BacklogWarning {
		status = health.StatusDegraded
	}
	return health.Result{Status: status, Message: engine.HealthMessage(runtime.WAL.Stats(), stats, runtime.Outbox.Stats())}
}

// dataEngineHealthMessage is the one line an operator reads first. Both sides
// of the outbox are on it: publish failures (the bus) and store failures (the
// claim / ack / nack round-trips to Mongo) — a store that is down looks like a
// healthy worker with a growing backlog otherwise.

var _ corenest.PipelinedTransactionCommitter = (*Mod)(nil)
var _ corenest.TransactionReleaseNotifier = (*Mod)(nil)
var _ corenest.LocalExecutorBinder = (*Mod)(nil)

// 效果流（ROOST_EFFECTS）的缺省名字与保留期，与 EffectsConfig 的声明相同（TestEffectStreamDefaultsMatchTheDeclaration）。
// saga Mod 的配置检查用 LoadEffectsConfig 读同一份，校验完成回执比流里的结果活得久（维护者决定）。
const (
	DefaultEffectStream = "ROOST_EFFECTS"
	DefaultEffectMaxAge = 7 * 24 * time.Hour
)

// LoadEffectsConfig 按 DataEngine Mod 的声明读 dataengine.effects.*（未配置取缺省）。saga Mod 用它取
// 效果流的名字与主题前缀：Nest 发起的 saga start 与原生步骤的结果都发布在这条流上，效果流只在这里写一次。
func LoadEffectsConfig(cfg *viper.Viper) (EffectsConfig, error) {
	var settings struct {
		Effects EffectsConfig `config:"dataengine.effects"`
	}
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return EffectsConfig{}, err
	}
	return settings.Effects, nil
}

// Prefix 是效果主题前缀去掉首尾点之后的值，与 Mod 发布效果时用的相同。
func (c EffectsConfig) Prefix() string { return c.subjectPrefix() }

// walDir 是 WAL 目录的规则：dataengine.wal.dir 写了就用它；不写时缺省按 sid 分目录
// data/wal/dataengine/<sid>（相对进程工作目录），同一工作目录、同一份配置用 --sid 起的多个进程各用各的 WAL。
// 生成的开发配置不写 dataengine.wal.dir，靠的就是这条缺省；生产示例显式写每实例独占的持久卷。
func walDir(configured string, sid int32) string {
	if configured != "" {
		return configured
	}
	return filepath.Join("data", "wal", "dataengine", fmt.Sprintf("%d", sid))
}

// WALDir 按 DataEngine Mod 的声明与同一条规则（walDir）返回 cfg 对应进程的 WAL 目录；sid 不为 0 时覆盖
// 配置里的 sid（与进程 --sid 一致）。运维工具（cmd/sidmove、cmd/sidmerge）检查一个已停进程的 WAL 时用它，
// 不另抄缺省。相对路径相对那个进程的工作目录。
func WALDir(cfg *viper.Viper, sid int32) (string, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", fmt.Errorf("dataengine: %w", err)
	}
	if sid == 0 {
		sid = settings.Sid
	}
	return walDir(settings.WAL.Dir, sid), nil
}

// EffectSettings 按 DataEngine Mod 的声明（整份 dataengine.*，与 Mod Init 读的同一个结构体）返回业务订阅效果流
// 要用的三样：效果收件箱所在的库（dataengine.database）、主题前缀（去掉首尾点，与 Mod 发布时相同）与流名。
// SubscribeEffects 与 wiring/saga 的 Steps（原生收件箱的库）用它；业务自己组装消费者时（game-demo 的活动阶段消费者）
// 同样经它，不直接读 viper、不另抄一份缺省值（生成工程同样守住“读配置只经声明”）。
func EffectSettings(cfg *viper.Viper) (database, subjectPrefix, stream string, err error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", "", "", fmt.Errorf("dataengine: %w", err)
	}
	return settings.Database, settings.Effects.subjectPrefix(), settings.Effects.Stream, nil
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModStopperWithContext = (*Mod)(nil)
var _ app.ModStopBudgetProvider = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)
