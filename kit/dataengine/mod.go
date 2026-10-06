package dataengine

import (
	"context"
	"errors"
	"fmt"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// Mod parses configuration, looks up the Mongo / JetStream / Remote Entity
// capabilities, hands them to core's engine.Assemble and forwards lifecycle
// calls. Construction order and failure rollback live in core (P3b).
type Mod struct {
	remoteEnabled bool
	access        *entity.ManagerAccess
	cfg           modConfig
	registry      *app.Registry
	asm           *engine.Assembly

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

type modConfig struct {
	mongo           engine.MongoStoreConfig
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
// that was actually started; roost-codegen U-0025).
func (mod *Mod) DependsOn() []app.ModName { return nil }

// OptionalDependsOn orders this Mod after the Mods whose capabilities it
// loads: Mongo, NATS (JetStream is the NATS Mod's capability, not a Mod of
// its own) and, when remote projection is on, Remote Entity.
func (mod *Mod) OptionalDependsOn() []app.ModName {
	deps := []app.ModName{mods.ModMongo, mods.ModNats}
	if mod != nil && mod.remoteEnabled {
		deps = append(deps, mods.ModRemoteEntity)
	}
	return deps
}

// EffectsConfig 是 dataengine.effects.* 的声明：效果流（ROOST_EFFECTS）的主题前缀、名字与保留期。
// saga Mod 经 EffectStreamRetention 读同一份，两边对流的理解不会分叉。
type EffectsConfig struct {
	SubjectPrefix   string        `config:"subject_prefix" default:"roost.effect" example:"roost.effect"`
	Stream          string        `config:"stream" default:"ROOST_EFFECTS" example:"ROOST_EFFECTS"`
	MaxAge          time.Duration `config:"max_age" default:"168h" min:"1ns" example:"168h"`
	MaxBytes        int64         `config:"max_bytes" default:"8589934592" min:"1" example:"8589934592"`
	DuplicateWindow time.Duration `config:"duplicate_window" default:"10m" min:"1ns" example:"10m"`
	Replicas        int           `config:"replicas" default:"1" min:"1" example:"1"`
}

// config 是 kit/dataengine 读的键（维护者决定 A4 ①）。数字与时长写 0 的取 core 缺省（声明里 min:"0" 的键）；
// 其余键的缺省值就是声明的 default，写 0 或负数拒绝。
type config struct {
	app.ServiceIdentity
	mods.PersistenceConfig
	Database              string        `config:"dataengine.database" default:"game" example:"game"`
	StartupTimeout        time.Duration `config:"dataengine.startup_timeout" default:"30s" min:"1ns" example:"30s"`
	ShutdownTimeout       time.Duration `config:"dataengine.shutdown_timeout" default:"30s" min:"1ns" example:"30s" help:"Stop budget declared to the App for draining WAL and projection; granted from\nshutdown.total_timeout after a 3s floor for each other Mod (scaled down with a warning\nonly when the rest cannot cover it). The generated shutdown.total_timeout counts it:\nraise that too when raising this."`
	TransactionReceiptTTL time.Duration `config:"dataengine.transaction_receipt_ttl" default:"720h" min:"1ns" example:"720h"`
	ReceiptTTL            time.Duration `config:"dataengine.receipt_ttl" default:"720h" min:"1ns" example:"720h"`
	WAL                   struct {
		Dir                 string        `config:"dir" example:"data/wal/dataengine" help:"不写取 data/wal/dataengine/<sid>"`
		WriterVersion       int           `config:"writer_version" default:"2" min:"1" max:"2" example:"2"`
		SegmentBytes        int64         `config:"segment_bytes" min:"0" example:"268435456"`
		MaxDiskBytes        int64         `config:"max_disk_bytes" min:"0" example:"8589934592"`
		MaxUnackedAge       time.Duration `config:"max_unacked_age" min:"0" example:"24h"`
		QueueCapacity       int           `config:"queue_capacity" min:"0" example:"8192"`
		GroupCommitInterval time.Duration `config:"group_commit_interval" min:"0" example:"2ms"`
	} `config:"dataengine.wal"`
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

// ValidateConfig：告警水位不能高于硬上限；嵌入的持久化选择也要检查（自己定义了 ValidateConfig，提升的那个被遮住）。
func (c *config) ValidateConfig(production bool) error {
	if err := c.PersistenceConfig.ValidateConfig(production); err != nil {
		return err
	}
	if c.Projection.MaxUnackedRecords > 0 && c.Projection.WarnUnackedRecords > c.Projection.MaxUnackedRecords {
		return errors.New("dataengine mod: invalid projection checkpoint or backlog limits: warn_unacked_records exceeds max_unacked_records")
	}
	return nil
}

// ConfigSchema 声明 dataengine.*、nest.pipelined.* 与持久化引擎的选择。
func (mod *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (mod *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	sid := settings.Sid
	dir := settings.WAL.Dir
	if dir == "" {
		dir = filepath.Join("data", "wal", "dataengine", fmt.Sprintf("%d", sid))
	}
	wal := nestwal.DefaultOptions(dir)
	wal.WriterVersion = nestwal.WriterVersionV2
	if settings.WAL.WriterVersion == 1 {
		wal.WriterVersion = nestwal.WriterVersionV1
	}
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
	effects := settings.Effects
	prefix := effects.subjectPrefix()
	mod.cfg = modConfig{
		mongo: engine.MongoStoreConfig{DefaultDatabase: settings.Database, ServerID: sid,
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
	asm, err := engine.Assemble(deps, engine.AssemblyConfig{
		Mongo: mod.cfg.mongo, WAL: mod.cfg.wal, Projector: mod.cfg.projector, Outbox: mod.cfg.outbox,
		EffectPrefix: mod.cfg.effectPrefix, EffectStream: mod.cfg.effectStream, Pipelined: mod.cfg.pipelined,
	})
	if err != nil {
		return err
	}
	mod.registry, mod.asm = registry, asm
	if err := registry.Register(mods.ModDataEngine, mod); err != nil {
		return err
	}
	healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("dataengine mod: capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("dataengine", health.CheckerFunc(mod.checkHealth))
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
// 排空 WAL 与投影，不够时按比例缩放并告警（RR-20260926-42、RR-20260926-51）。未 Init 时为 0，即不声明。
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
// 是跨进程交出实体所有权前的屏障（RR-20260926-31）：另一进程接手时只从 Mongo
// 读取，本进程已确认但未投影的事务不会被它看到。只跟踪本进程的在途记录；
// 投影失败、Runtime 未就绪或 WAL 不健康都返回错误，调用方应保留所有权并重试。
// 会阻塞，只能在慢路径或独立 goroutine 调用，快 worker 上调用会按
// RR-20260926-06 fail-fast。
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

// BindLocalExecutor 实现 corenest.LocalExecutorBinder：Nest 在构造时交来快池执行入口，
// DataEngine 用它在快池持锁驱逐被跳过的原生步骤留下的实体（RR-20260926-30）。
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

// 效果流（ROOST_EFFECTS）的缺省名字与保留期。saga Mod 用 EffectStreamRetention 读同一份，
// 校验完成回执比流里的结果活得久（O-S5-2，维护者第十二轮决定）。
const (
	DefaultEffectStream = "ROOST_EFFECTS"
	DefaultEffectMaxAge = 7 * 24 * time.Hour
)

// EffectStreamRetention 按 DataEngine Mod 的声明返回效果流的名字与保留期
// （dataengine.effects.stream / dataengine.effects.max_age，未配置取缺省）。
func EffectStreamRetention(cfg *viper.Viper) (stream string, maxAge time.Duration, err error) {
	var settings struct {
		Effects EffectsConfig `config:"dataengine.effects"`
	}
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", 0, err
	}
	return settings.Effects.Stream, settings.Effects.MaxAge, nil
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModStopperWithContext = (*Mod)(nil)
var _ app.ModStopBudgetProvider = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)
