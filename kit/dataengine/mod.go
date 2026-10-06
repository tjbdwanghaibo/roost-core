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

func (mod *Mod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	if _, err := mods.ResolvePersistenceEngine(cfg); err != nil {
		return err
	}
	// 严格读取（维护者决定 A4）：写错类型的值不再被读成 0 / 纳秒后取默认，返回前一并报出。
	read := app.NewConfigReader(cfg)
	sid := cfg.GetInt32("sid")
	database := strings.TrimSpace(cfg.GetString("dataengine.database"))
	if database == "" {
		database = "game"
	}
	dir := strings.TrimSpace(cfg.GetString("dataengine.wal.dir"))
	if dir == "" {
		dir = filepath.Join("data", "wal", "dataengine", fmt.Sprintf("%d", sid))
	}
	wal := nestwal.DefaultOptions(dir)
	switch read.Int("dataengine.wal.writer_version") {
	case 0:
		wal.WriterVersion = nestwal.WriterVersionV2
	case 1:
		wal.WriterVersion = nestwal.WriterVersionV1
	case 2:
		wal.WriterVersion = nestwal.WriterVersionV2
	default:
		return errors.Join(read.Err(), errors.New("dataengine mod: wal.writer_version must be 1 or 2"))
	}
	if value := read.Int64("dataengine.wal.segment_bytes"); value > 0 {
		wal.SegmentBytes = value
	}
	if value := read.Int("dataengine.wal.queue_capacity"); value > 0 {
		wal.QueueCapacity = value
	}
	if value := read.Duration("dataengine.wal.group_commit_interval"); value > 0 {
		wal.GroupCommitInterval = value
	}
	if value := read.Int64("dataengine.wal.max_disk_bytes"); value > 0 {
		wal.MaxDiskBytes = value
	}
	if value := read.Duration("dataengine.wal.max_unacked_age"); value > 0 {
		wal.MaxUnackedAge = value
	}
	wal.OnFatal = mod.onFatal
	projector := engine.DefaultProjectorOptions()
	if cfg.IsSet("dataengine.projection.remote_workers") {
		value := read.Int("dataengine.projection.remote_workers")
		if value < 1 || value > 64 {
			return errors.Join(read.Err(), errors.New("dataengine mod: projection.remote_workers must be between 1 and 64"))
		}
		projector.RemoteProjectionWorkers = value
	}
	if value := read.Duration("dataengine.projection.retry_min"); value > 0 {
		projector.RetryMin = value
	}
	if value := read.Duration("dataengine.projection.retry_max"); value > 0 {
		projector.RetryMax = value
	}
	if value := read.Int("dataengine.projection.batch_records"); value > 0 {
		projector.ReplayBatchRecords = value
	}
	if value := read.Int("dataengine.projection.batch_bytes"); value > 0 {
		projector.ReplayBatchBytes = value
	}
	if value := read.Int("dataengine.projection.read_bytes"); value < 0 {
		return errors.Join(read.Err(), errors.New("dataengine mod: projection.read_bytes must not be negative"))
	} else if value > 0 {
		projector.ReplayReadBytes = value
	}
	checkpointRecords := read.Int("dataengine.projection.checkpoint_records")
	checkpointInterval := read.Duration("dataengine.projection.checkpoint_interval")
	maxUnacked := read.Int64("dataengine.projection.max_unacked_records")
	warnUnacked := read.Int64("dataengine.projection.warn_unacked_records")
	if checkpointRecords < 0 || checkpointInterval < 0 || maxUnacked < 0 || warnUnacked < 0 || (maxUnacked > 0 && warnUnacked > maxUnacked) {
		return errors.Join(read.Err(), errors.New("dataengine mod: invalid projection checkpoint or backlog limits"))
	}
	if checkpointRecords > 0 {
		projector.CheckpointRecords = checkpointRecords
	}
	if checkpointInterval > 0 {
		projector.CheckpointInterval = checkpointInterval
	}
	projector.MaxUnackedRecords = uint64(maxUnacked)
	projector.WarnUnackedRecords = uint64(warnUnacked)
	projector.OnFatal = mod.onFatal
	owner := strings.TrimSpace(cfg.GetString("dataengine.outbox.owner"))
	if owner == "" {
		owner = fmt.Sprintf("dataengine-%d", sid)
	}
	outbox := engine.OutboxWorkerOptions{
		Owner: owner, Workers: positive(read.Int("dataengine.outbox.workers"), 2),
		BatchSize:     positive(read.Int("dataengine.outbox.batch_size"), 64),
		LeaseDuration: duration(read.Duration("dataengine.outbox.lease_duration"), 30*time.Second),
		PollInterval:  duration(read.Duration("dataengine.outbox.poll_interval"), 100*time.Millisecond),
		RetryMin:      duration(read.Duration("dataengine.outbox.retry_min"), time.Second),
		RetryMax:      duration(read.Duration("dataengine.outbox.retry_max"), time.Minute),
		MaxPending:    read.Int64("dataengine.outbox.max_pending"), MaxOldestAge: read.Duration("dataengine.outbox.max_oldest_age"),
		BacklogInterval: duration(read.Duration("dataengine.outbox.backlog_interval"), time.Second),
		OnHardLimit:     mod.onFatal,
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.GetString("dataengine.effects.subject_prefix")), ".")
	if prefix == "" {
		prefix = "roost.effect"
	}
	stream := effectStreamName(cfg)
	mod.cfg = modConfig{
		mongo: engine.MongoStoreConfig{DefaultDatabase: database, ServerID: sid,
			TransactionReceiptTTL: duration(read.Duration("dataengine.transaction_receipt_ttl"), 30*24*time.Hour),
			ReceiptTTL:            duration(read.Duration("dataengine.receipt_ttl"), 30*24*time.Hour)},
		wal: wal, projector: projector, outbox: outbox, effectPrefix: prefix,
		effectStream: fnats.JetStreamConfig{
			Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge:     duration(read.Duration("dataengine.effects.max_age"), DefaultEffectMaxAge),
			Duplicates: duration(read.Duration("dataengine.effects.duplicate_window"), 10*time.Minute),
			Replicas:   positive(read.Int("dataengine.effects.replicas"), 1),
			MaxBytes:   positiveInt64(read.Int64("dataengine.effects.max_bytes"), 8<<30),
		},
		startupTimeout:  duration(read.Duration("dataengine.startup_timeout"), 30*time.Second),
		shutdownTimeout: duration(read.Duration("dataengine.shutdown_timeout"), 30*time.Second),
		pipelined: engine.PipelinedRuntimeConfig{
			Allowlist: cfg.GetStringSlice("nest.pipelined.allowlist"), Async: read.Bool("nest.pipelined.async"),
			AsyncWorkers: read.Int("nest.pipelined.async_workers"), AsyncQueueCap: read.Int("nest.pipelined.async_queue_capacity"),
		},
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("dataengine mod: %w", err)
	}
	return nil
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

func effectStreamName(cfg *viper.Viper) string {
	if stream := strings.TrimSpace(cfg.GetString("dataengine.effects.stream")); stream != "" {
		return stream
	}
	return DefaultEffectStream
}

// EffectStreamRetention 按 DataEngine Mod 的读法返回效果流的名字与保留期
// （dataengine.effects.stream / dataengine.effects.max_age，未配置取缺省）。
func EffectStreamRetention(cfg *viper.Viper) (stream string, maxAge time.Duration, err error) {
	if cfg == nil {
		return DefaultEffectStream, DefaultEffectMaxAge, nil
	}
	read := app.NewConfigReader(cfg)
	maxAge = duration(read.Duration("dataengine.effects.max_age"), DefaultEffectMaxAge)
	if err := read.Err(); err != nil {
		return "", 0, err
	}
	return effectStreamName(cfg), maxAge, nil
}

func duration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
func positiveInt64(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModStopperWithContext = (*Mod)(nil)
var _ app.ModStopBudgetProvider = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)
