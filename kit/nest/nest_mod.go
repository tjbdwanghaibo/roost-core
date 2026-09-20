// Package nest provides app.Mod wiring for the core Nest execution engine.
package nest

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// Mod owns one instance-scoped Nest engine. It intentionally does not install
// nest.Nest or entity.SendMsg; consumers obtain nest.Client from app.Registry
// and inject it into generated senders.
type Mod struct {
	getter entity.Getter
	opts   []corenest.NestOption
	engine *corenest.NestMgr
	config engineConfig
}

type engineConfig struct {
	workerNum   int
	hbWorkerNum int
	queueCap    int
	tick        time.Duration
	timeout     time.Duration
	delayedCap  int
	maxDelay    time.Duration
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
		workerNum:   cfg.GetInt("nest.worker_num"),
		hbWorkerNum: cfg.GetInt("nest.heartbeat_worker_num"),
		queueCap:    cfg.GetInt("nest.queue_capacity"),
		tick:        cfg.GetDuration("nest.tick_duration"),
		timeout:     cfg.GetDuration("nest.request_timeout"),
		delayedCap:  cfg.GetInt("nest.delayed_capacity"),
		maxDelay:    cfg.GetDuration("nest.max_delay"),
	}
	return nil
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
	return m.engine.Start()
}

func (m *Mod) Stop() {
	_ = m.StopWithContext(context.Background())
}

func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil || m.engine == nil {
		return nil
	}
	return m.engine.Shutdown(ctx)
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
		Message: fmt.Sprintf("queue=%d delayed=%d", stats.Main.QueueLen+stats.Heart.QueueLen+stats.Cost.QueueLen, stats.Delayed),
	}
}
