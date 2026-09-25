// Package nest provides app.Mod wiring for the core Nest execution engine.
package nest

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"log/slog"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// Mod owns one instance-scoped Nest engine. It intentionally does not install
// nest.Nest or entity.SendMsg; consumers obtain nest.Client from app.Registry
// and inject it into generated senders.
type Mod struct {
	syncSetup  *EntitySyncSetup
	entitySync *entitysync.Manager
	getter     entity.Getter
	opts       []corenest.NestOption
	engine     *corenest.NestMgr
	config     engineConfig
}

type engineConfig struct {
	fast, slow    corenest.WorkerPoolConfig
	workerNum     int
	hbWorkerNum   int
	remoteWorkers int
	queueCap      int
	tick          time.Duration
	timeout       time.Duration
	delayedCap    int
	maxDelay      time.Duration
}

func NewMod(getter entity.Getter, opts ...corenest.NestOption) *Mod {
	return &Mod{getter: getter, opts: append([]corenest.NestOption(nil), opts...)}
}

func (m *Mod) Name() app.ModName { return mods.ModNest }

func (m *Mod) DependsOn() []app.ModName { return nil }

// OptionalDependsOn ensures the Remote Entity transaction participant is
// visible during Provide when the application has installed it.
func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine, mods.ModRemoteEntity}
}

func (m *Mod) Init(cfg *viper.Viper) error {
	if m == nil || m.getter == nil {
		return corenest.ErrGetterNotSet
	}
	if cfg == nil {
		cfg = viper.New()
	}
	if _, err := mods.ResolvePersistenceEngine(cfg); err != nil {
		return err
	}
	m.config = engineConfig{
		fast:          corenest.WorkerPoolConfig{Workers: cfg.GetInt("nest.fast.workers"), QueueCap: cfg.GetInt("nest.fast.queue_capacity")},
		slow:          corenest.WorkerPoolConfig{Workers: cfg.GetInt("nest.slow.workers"), QueueCap: cfg.GetInt("nest.slow.queue_capacity")},
		workerNum:     cfg.GetInt("nest.worker_num"),
		hbWorkerNum:   cfg.GetInt("nest.heartbeat_worker_num"),
		remoteWorkers: cfg.GetInt("nest.remote_workers"),
		queueCap:      cfg.GetInt("nest.queue_capacity"),
		tick:          cfg.GetDuration("nest.tick_duration"),
		timeout:       cfg.GetDuration("nest.request_timeout"),
		delayedCap:    cfg.GetInt("nest.delayed_capacity"),
		maxDelay:      cfg.GetDuration("nest.max_delay"),
	}
	return m.initEntitySync(cfg)
}

func (m *Mod) Provide(registry *app.Registry) error {
	if m == nil || m.getter == nil {
		return corenest.ErrGetterNotSet
	}
	if registry == nil {
		return fmt.Errorf("nest mod: nil app registry")
	}
	opts := []corenest.NestOption{
		corenest.NestOptionWithGetter(m.getter),
		corenest.NestOptionWithWorkerNumAndMsgCap(m.config.workerNum, m.config.hbWorkerNum, m.config.queueCap),
		corenest.NestOptionWithRemoteWorkers(m.config.remoteWorkers),
		corenest.NestOptionWithWorkerPools(m.config.fast, m.config.slow),
		corenest.NestOptionWithTickDuration(m.config.tick),
		corenest.NestOptionWithSyncTimeout(m.config.timeout),
		corenest.NestOptionWithDelayedAdmission(m.config.delayedCap, m.config.maxDelay),
	}
	provider, ok := app.Lookup[interface{ NestOptions() []corenest.NestOption }](registry, mods.ModDataEngine)
	if !ok || provider == nil {
		return fmt.Errorf("nest mod: required data engine capability %q not found", mods.ModDataEngine)
	}
	engineOptions := provider.NestOptions()
	if len(engineOptions) == 0 {
		return fmt.Errorf("nest mod: data engine capability %q is not initialized", mods.ModDataEngine)
	}
	opts = append(opts, engineOptions...)
	if remoteManager, ok := app.Lookup[entity.IRemoteEntityManager](registry, mods.ModRemoteEntity); ok && remoteManager != nil {
		opts = append(opts, corenest.NestOptionWithRemoteEntityManager(remoteManager))
	}
	opts = append(opts, m.opts...)
	if m.entitySync != nil {
		opts = append(opts, corenest.NestOptionWithEntitySync(m.entitySync))
	}
	m.engine = corenest.NewEngine(opts...)
	capabilities := []mods.Capability{{Name: mods.ModNest, Value: m.engine}}
	if _, exists := registry.Get(mods.ModEntityRuntime); !exists {
		capabilities = append(capabilities, mods.Capability{Name: mods.ModEntityRuntime, Value: m.getter})
	}
	if err := mods.RegisterAll(registry, capabilities...); err != nil {
		m.engine = nil
		return err
	}
	if healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth); ok && healthRegistry != nil {
		healthRegistry.Register("nest", health.CheckerFunc(m.checkHealth))
	}
	return nil
}

func (m *Mod) Start() error {
	if m == nil || m.engine == nil {
		return fmt.Errorf("nest mod: engine not provided")
	}
	if m.entitySync != nil {
		if err := m.entitySync.Start(context.Background()); err != nil {
			return err
		}
		slog.Info("entity sync started", "mode", m.entitySync.Mode().String(), "interval", m.entitySync.Interval())
	}
	if err := m.engine.Start(); err != nil {
		if m.entitySync != nil {
			_ = m.entitySync.Stop(context.Background())
		}
		return err
	}
	return nil
}

func (m *Mod) Stop() {
	_ = m.StopWithContext(context.Background())
}

func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil || m.engine == nil {
		return nil
	}
	if err := m.engine.Shutdown(ctx); err != nil {
		return err
	}
	if m.entitySync != nil {
		if err := m.entitySync.Stop(ctx); err != nil {
			return err
		}
		if err := m.entitySync.Drain(ctx); err != nil {
			return err
		}
		return m.entitySync.Close(ctx)
	}
	return nil
}

func (m *Mod) Engine() *corenest.NestMgr {
	if m == nil {
		return nil
	}
	return m.engine
}

func (m *Mod) checkHealth(context.Context) health.Result {
	if m == nil || m.engine == nil || !m.engine.Running() {
		return health.Result{Status: health.StatusFail, Message: "engine not running"}
	}
	stats := m.engine.Stats()
	return health.Result{
		Status:  health.StatusOK,
		Message: fmt.Sprintf("queue=%d delayed=%d", stats.Fast.QueueLen+stats.Slow.QueueLen+stats.FastContinuations, stats.Delayed),
	}
}
