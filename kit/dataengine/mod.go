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
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"github.com/tjbdwanghaibo/roost-kit/mods"
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
	switch cfg.GetInt("dataengine.wal.writer_version") {
	case 0:
		wal.WriterVersion = nestwal.WriterVersionV2
	case 1:
		wal.WriterVersion = nestwal.WriterVersionV1
	case 2:
		wal.WriterVersion = nestwal.WriterVersionV2
	default:
		return errors.New("dataengine mod: wal.writer_version must be 1 or 2")
	}
	if value := cfg.GetInt64("dataengine.wal.segment_bytes"); value > 0 {
		wal.SegmentBytes = value
	}
	if value := cfg.GetInt("dataengine.wal.queue_capacity"); value > 0 {
		wal.QueueCapacity = value
	}
	if value := cfg.GetDuration("dataengine.wal.group_commit_interval"); value > 0 {
		wal.GroupCommitInterval = value
	}
	if value := cfg.GetInt64("dataengine.wal.max_disk_bytes"); value > 0 {
		wal.MaxDiskBytes = value
	}
	if value := cfg.GetDuration("dataengine.wal.max_unacked_age"); value > 0 {
		wal.MaxUnackedAge = value
	}
	wal.OnFatal = mod.onFatal
	projector := engine.DefaultProjectorOptions()
	if value := cfg.GetDuration("dataengine.projection.retry_min"); value > 0 {
		projector.RetryMin = value
	}
	if value := cfg.GetDuration("dataengine.projection.retry_max"); value > 0 {
		projector.RetryMax = value
	}
	if value := cfg.GetInt("dataengine.projection.batch_records"); value > 0 {
		projector.ReplayBatchRecords = value
	}
	if value := cfg.GetInt("dataengine.projection.batch_bytes"); value > 0 {
		projector.ReplayBatchBytes = value
	}
	projector.OnFatal = mod.onFatal
	owner := strings.TrimSpace(cfg.GetString("dataengine.outbox.owner"))
	if owner == "" {
		owner = fmt.Sprintf("dataengine-%d", sid)
	}
	outbox := engine.OutboxWorkerOptions{
		Owner: owner, Workers: positive(cfg.GetInt("dataengine.outbox.workers"), 2),
		BatchSize:     positive(cfg.GetInt("dataengine.outbox.batch_size"), 64),
		LeaseDuration: duration(cfg.GetDuration("dataengine.outbox.lease_duration"), 30*time.Second),
		PollInterval:  duration(cfg.GetDuration("dataengine.outbox.poll_interval"), 100*time.Millisecond),
		RetryMin:      duration(cfg.GetDuration("dataengine.outbox.retry_min"), time.Second),
		RetryMax:      duration(cfg.GetDuration("dataengine.outbox.retry_max"), time.Minute),
		MaxPending:    cfg.GetInt64("dataengine.outbox.max_pending"), MaxOldestAge: cfg.GetDuration("dataengine.outbox.max_oldest_age"),
		BacklogInterval: duration(cfg.GetDuration("dataengine.outbox.backlog_interval"), time.Second),
		OnHardLimit:     mod.onFatal,
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.GetString("dataengine.effects.subject_prefix")), ".")
	if prefix == "" {
		prefix = "roost.effect"
	}
	stream := strings.TrimSpace(cfg.GetString("dataengine.effects.stream"))
	if stream == "" {
		stream = "ROOST_EFFECTS"
	}
	mod.cfg = modConfig{
		mongo: engine.MongoStoreConfig{DefaultDatabase: database, ServerID: sid,
			TransactionReceiptTTL: duration(cfg.GetDuration("dataengine.transaction_receipt_ttl"), 30*24*time.Hour),
			ReceiptTTL:            duration(cfg.GetDuration("dataengine.receipt_ttl"), 30*24*time.Hour)},
		wal: wal, projector: projector, outbox: outbox, effectPrefix: prefix,
		effectStream: fnats.JetStreamConfig{
			Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge:     duration(cfg.GetDuration("dataengine.effects.max_age"), 7*24*time.Hour),
			Duplicates: duration(cfg.GetDuration("dataengine.effects.duplicate_window"), 10*time.Minute),
			Replicas:   positive(cfg.GetInt("dataengine.effects.replicas"), 1),
			MaxBytes:   positiveInt64(cfg.GetInt64("dataengine.effects.max_bytes"), 8<<30),
		},
		startupTimeout:  duration(cfg.GetDuration("dataengine.startup_timeout"), 30*time.Second),
		shutdownTimeout: duration(cfg.GetDuration("dataengine.shutdown_timeout"), 30*time.Second),
		pipelined: engine.PipelinedRuntimeConfig{
			Allowlist: cfg.GetStringSlice("nest.pipelined.allowlist"), Async: cfg.GetBool("nest.pipelined.async"),
			AsyncWorkers: cfg.GetInt("nest.pipelined.async_workers"), AsyncQueueCap: cfg.GetInt("nest.pipelined.async_queue_capacity"),
		},
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
	deps := engine.AssemblyDeps{Mongo: mongoClient, JetStream: jetStream, Access: mod.access, OnFatal: mod.onFatal}
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
	return health.Result{Status: health.StatusOK, Message: engine.HealthMessage(runtime.WAL.Stats(), runtime.Projector.Stats(), runtime.Outbox.Stats())}
}

// dataEngineHealthMessage is the one line an operator reads first. Both sides
// of the outbox are on it: publish failures (the bus) and store failures (the
// claim / ack / nack round-trips to Mongo) — a store that is down looks like a
// healthy worker with a growing backlog otherwise.

var _ corenest.PipelinedTransactionCommitter = (*Mod)(nil)
var _ corenest.TransactionReleaseNotifier = (*Mod)(nil)

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
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)
