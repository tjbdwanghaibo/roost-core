package nestwal

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/cube-core/app"
	"github.com/tjbdwanghaibo/cube-core/entity"
	"github.com/tjbdwanghaibo/cube-core/health"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	kitcheckpoint "github.com/tjbdwanghaibo/cube-kit/checkpoint"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	kitremote "github.com/tjbdwanghaibo/cube-kit/remote_entity"
)

type Mod struct {
	remoteEnabled bool
	runtime       *Runtime
	config        modConfig
	registry      *app.Registry
	fatalMu       sync.RWMutex
	fatalErr      error
}

type modConfig struct {
	wal            Options
	committer      CommitterOptions
	effectPrefix   string
	effectStream   fnats.JetStreamConfig
	startupTimeout time.Duration
}

func NewMod(remoteEnabled bool) *Mod { return &Mod{remoteEnabled: remoteEnabled} }

func (m *Mod) Name() app.ModName { return mods.ModNestWAL }

func (m *Mod) DependsOn() []app.ModName {
	deps := []app.ModName{mods.ModCheckpoint, mods.ModNatsJetStream, mods.ModHealth}
	if m != nil && m.remoteEnabled {
		deps = append(deps, mods.ModRemoteEntity)
	}
	return deps
}

func (m *Mod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	dir := cfg.GetString("nest.wal.dir")
	if dir == "" {
		dir = filepath.Join("data", "wal", "nest", fmt.Sprintf("%d", cfg.GetInt32("sid")))
	}
	wal := DefaultOptions(dir)
	if value := cfg.GetInt64("nest.wal.segment_bytes"); value > 0 {
		wal.SegmentBytes = value
	}
	if value := cfg.GetInt("nest.wal.max_record_bytes"); value > 0 {
		wal.MaxRecordBytes = value
	}
	if value := cfg.GetInt("nest.wal.queue_capacity"); value > 0 {
		wal.QueueCapacity = value
	}
	if value := cfg.GetInt("nest.wal.batch_max_records"); value > 0 {
		wal.BatchMaxRecords = value
	}
	if value := cfg.GetInt("nest.wal.batch_max_bytes"); value > 0 {
		wal.BatchMaxBytes = value
	}
	if value := cfg.GetDuration("nest.wal.batch_delay"); value > 0 {
		wal.BatchDelay = value
	}
	if value := cfg.GetDuration("nest.wal.group_commit_interval"); value > 0 {
		wal.GroupCommitInterval = value
	}
	if cfg.IsSet("nest.wal.retain_segments") {
		wal.RetainSegments = cfg.GetInt("nest.wal.retain_segments")
	}
	if value := cfg.GetInt64("nest.wal.max_disk_bytes"); value > 0 {
		wal.MaxDiskBytes = value
	}
	if value := cfg.GetDuration("nest.wal.max_unacked_age"); value > 0 {
		wal.MaxUnackedAge = value
	}
	wal.OnFatal = m.onFatal

	committer := DefaultCommitterOptions()
	if value := cfg.GetDuration("nest.wal.replay_retry_min"); value > 0 {
		committer.RetryMin = value
	}
	if value := cfg.GetDuration("nest.wal.replay_retry_max"); value > 0 {
		committer.RetryMax = value
	}
	if value := cfg.GetDuration("nest.wal.replay_idle_poll"); value > 0 {
		committer.IdlePoll = value
	}
	if value := cfg.GetInt("nest.wal.replay_batch_records"); value > 0 {
		committer.ReplayBatchRecords = value
	}
	if value := cfg.GetInt("nest.wal.receipt_cleanup_batch"); value > 0 {
		committer.ReceiptCleanupBatch = value
	}
	if value := cfg.GetInt("nest.wal.receipt_cleanup_capacity"); value > 0 {
		committer.ReceiptCleanupCapacity = value
	}
	prefix := cfg.GetString("nest.wal.effects.subject_prefix")
	if prefix == "" {
		prefix = "roost.effect"
	}
	streamName := cfg.GetString("nest.wal.effects.stream")
	if streamName == "" {
		streamName = "ROOST_EFFECTS"
	}
	maxAge := durationDefault(cfg.GetDuration("nest.wal.effects.max_age"), 7*24*time.Hour)
	// Broker dedup is only a short hot-path optimization. Durable exactly-once
	// is provided by MongoEffectInbox, so this window must not scale with the
	// full stream retention period and consume unbounded server memory.
	duplicateWindow := durationDefault(cfg.GetDuration("nest.wal.effects.duplicate_window"), 10*time.Minute)
	m.config = modConfig{
		wal: wal, committer: committer, effectPrefix: prefix,
		startupTimeout: durationDefault(cfg.GetDuration("nest.wal.startup_timeout"), 30*time.Second),
		effectStream: fnats.JetStreamConfig{
			Name: streamName, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge:     maxAge,
			Duplicates: duplicateWindow,
			Replicas:   positiveDefault(cfg.GetInt("nest.wal.effects.replicas"), 1),
			MaxBytes:   cfg.GetInt64("nest.wal.effects.max_bytes"),
		},
	}
	return nil
}

func (m *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return fmt.Errorf("nestwal mod: nil registry")
	}
	cp, ok := app.Lookup[*kitcheckpoint.Mod](registry, mods.ModCheckpoint)
	if !ok || cp == nil || cp.Backend() == nil {
		return fmt.Errorf("nestwal mod: capability %q not found", mods.ModCheckpoint)
	}
	backend := cp.Backend()
	applier := &MongoAtomicApplier{Backend: backend}
	if m.remoteEnabled {
		manager, ok := app.Lookup[entity.IRemoteEntityManager](registry, mods.ModRemoteEntity)
		if !ok || manager == nil {
			return fmt.Errorf("nestwal mod: capability %q not found", mods.ModRemoteEntity)
		}
		remote, ok := manager.(entity.RemoteCommitApplier)
		if !ok {
			return fmt.Errorf("nestwal mod: remote manager has no commit applier")
		}
		store, ok := app.Lookup[kitremote.AtomicCommitStore](registry, mods.ModRemoteEntityAtomicStore)
		if !ok || store == nil {
			return fmt.Errorf("nestwal mod: capability %q not found", mods.ModRemoteEntityAtomicStore)
		}
		applier.Remote = remote
		applier.RemoteStore = store
	}
	jetStream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetStream == nil {
		return fmt.Errorf("nestwal mod: capability %q not found", mods.ModNatsJetStream)
	}
	publisher, err := NewJetStreamEffectPublisher(jetStream, m.config.effectPrefix)
	if err != nil {
		return err
	}
	runtime, err := OpenRuntime(m.config.wal, applier, publisher, m.config.committer)
	if err != nil {
		return err
	}
	m.runtime = runtime
	m.registry = registry
	if err := registry.Register(mods.ModNestWAL, runtime); err != nil {
		_ = runtime.Shutdown(context.Background())
		m.runtime = nil
		return err
	}
	healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("nestwal mod: capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("nest_wal", health.CheckerFunc(m.checkHealth))
	return nil
}

func (m *Mod) Start() error {
	if m == nil || m.runtime == nil || m.registry == nil {
		return fmt.Errorf("nestwal mod: not provided")
	}
	jetStream, _ := app.Lookup[fnats.IJetStream](m.registry, mods.ModNatsJetStream)
	ctx, cancel := context.WithTimeout(context.Background(), m.config.startupTimeout)
	defer cancel()
	if err := jetStream.EnsureStream(ctx, m.config.effectStream); err != nil {
		return fmt.Errorf("nestwal mod: ensure effect stream: %w", err)
	}
	// Recovery completes before Nest starts accepting handlers.
	if err := m.runtime.Flush(ctx); err != nil {
		return fmt.Errorf("nestwal mod: startup recovery: %w", err)
	}
	return nil
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }

func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil || m.runtime == nil {
		return nil
	}
	err := m.runtime.Shutdown(ctx)
	m.runtime = nil
	return err
}

func (m *Mod) Flush(ctx context.Context) error {
	if m == nil || m.runtime == nil {
		return nil
	}
	return m.runtime.Flush(ctx)
}

func (m *Mod) Runtime() *Runtime {
	if m == nil {
		return nil
	}
	return m.runtime
}

func (m *Mod) onFatal(err error) {
	m.fatalMu.Lock()
	m.fatalErr = err
	m.fatalMu.Unlock()
	slog.Error("nestwal: fatal storage outcome; process is fenced", "err", err)
	if m.registry != nil {
		if engine, ok := app.Lookup[*corenest.NestMgr](m.registry, mods.ModNest); ok && engine != nil {
			engine.Fence(err)
		}
		if failure, ok := app.Lookup[*app.RuntimeFailure](m.registry, app.ModRuntimeFailure); ok && failure != nil {
			failure.Fail(fmt.Errorf("nestwal fatal storage outcome: %w", err))
		}
	}
}

func (m *Mod) checkHealth(context.Context) health.Result {
	if m == nil || m.runtime == nil || m.runtime.Committer == nil {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	m.fatalMu.RLock()
	fatal := m.fatalErr
	m.fatalMu.RUnlock()
	if fatal != nil {
		return health.Result{Status: health.StatusFail, Message: "fatal WAL outcome", Err: fatal}
	}
	if err := m.runtime.Committer.Healthy(); err != nil {
		return health.Result{Status: health.StatusFail, Message: "replay unhealthy", Err: err}
	}
	walStats := m.runtime.WAL.Stats()
	commitStats := m.runtime.Committer.Stats()
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("queued=%d appended=%d acked=%d disk_bytes=%d segments=%d oldest_unacked=%s replay_failures=%d receipt_cleanup_pending=%d", walStats.Queued, walStats.Appended, walStats.Acknowledged, walStats.DiskBytes, walStats.SegmentFiles, walStats.OldestUnackedAge, commitStats.ReplayFailures, commitStats.PendingReceiptCleanup)}
}

func durationDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func positiveDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModStopperWithContext = (*Mod)(nil)
