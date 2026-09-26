// Package nest provides app.Mod wiring for the core Nest execution engine.
package nest

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"log/slog"
	"sync/atomic"
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
	// autoWatermark 表示 EntitySync 的外发水位由 kit 接线，来源在 Provide 构造引擎后写入 watermark；
	// Sync 的 Flush 可能在其他 goroutine 读取，所以用原子指针。
	autoWatermark bool
	watermark     atomic.Pointer[durableWatermarkSource]
	getter        entity.Getter
	opts          []corenest.NestOption
	engine        *corenest.NestMgr
	config        engineConfig
	// dataEngine 是 Provide 时查到的 DataEngine 能力；Start 时用它把 Sync 接上重新加载的实体。
	dataEngine   any
	unhookLoaded func()
	// stopResync 停止卸载后重载（ManagerAccess.ConfigureUnloadResync，RR-20260926-59）；未接线时为 nil。
	stopResync func(context.Context) error
}

// entityLoadNotifier 是 DataEngine 的可选能力（kit/dataengine Mod.OnEntityLoaded）。
type entityLoadNotifier interface {
	OnEntityLoaded(func(entity.IThreadSafeEntity)) (func(), error)
}

// unloadResyncConfigurer 是 getter 的可选能力（正式装配里 getter 就是 *entity.ManagerAccess）。
type unloadResyncConfigurer interface {
	ConfigureUnloadResync(entity.UnloadedSubjectSync, entity.UnloadResyncConfig) (func(context.Context) error, error)
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
	m.dataEngine = provider
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
	m.bindDurableWatermark(m.engine)
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
		// DataEngine 驱逐实体（例如原生步骤被 lease fence 跳过，RR-20260926-30）会关闭它的同步状态；
		// 重新加载后把仍登记着的 subject 接到新对象上，原订阅者收到全量而不是续发增量。
		if loads, ok := m.dataEngine.(entityLoadNotifier); ok && m.unhookLoaded == nil {
			unhook, err := loads.OnEntityLoaded(m.rebindEntitySync)
			if err != nil {
				_ = m.entitySync.Stop(context.Background())
				return fmt.Errorf("nest mod: entity sync reload hook: %w", err)
			}
			m.unhookLoaded = unhook
		}
		// 实体被仅内存卸载（RR-30 驱逐、RR-39 持久拒绝）后仍有订阅者时，框架在快池之外从权威重载并 Rebind，
		// 重载不了退回 remove（RR-20260926-59）。重载走 ManagerAccess 的共享加载，发布经 NewEngine 绑定给 getter
		// 的 NestMgr.RunLocal（BindLocalExecutor）回快池。
		if resync, ok := m.getter.(unloadResyncConfigurer); ok && m.stopResync == nil {
			stop, err := resync.ConfigureUnloadResync(m.entitySync, entity.UnloadResyncConfig{})
			if err != nil {
				m.unhookEntitySync()
				_ = m.entitySync.Stop(context.Background())
				return fmt.Errorf("nest mod: entity sync unload resync: %w", err)
			}
			m.stopResync = stop
		}
	}
	if err := m.engine.Start(); err != nil {
		if m.entitySync != nil {
			_ = m.stopUnloadResync(context.Background())
			m.unhookEntitySync()
			_ = m.entitySync.Stop(context.Background())
		}
		return err
	}
	return nil
}

func (m *Mod) stopUnloadResync(ctx context.Context) error {
	if m.stopResync == nil {
		return nil
	}
	err := m.stopResync(ctx)
	m.stopResync = nil
	return err
}

func (m *Mod) unhookEntitySync() {
	if m.unhookLoaded != nil {
		m.unhookLoaded()
		m.unhookLoaded = nil
	}
}

// rebindEntitySync 只处理仍登记着、且旧状态已关闭的 subject；普通冷加载返回 ErrSubjectNotRegistered，忽略。
func (m *Mod) rebindEntitySync(loaded entity.IThreadSafeEntity) {
	if loaded == nil || loaded.Base() == nil {
		return
	}
	state := loaded.Base().Sync()
	if state == nil {
		return
	}
	if err := m.entitySync.Rebind(state); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) && !errors.Is(err, entitysync.ErrSubjectRegistered) {
		slog.Warn("entity sync: reloaded entity was not rebound", "entity", loaded.ID(), "err", err)
	}
}

func (m *Mod) Stop() {
	_ = m.StopWithContext(context.Background())
}

func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil || m.engine == nil {
		return nil
	}
	// 先停卸载后重载：停机期间不再为订阅者重载（取消在途重载，不发 remove），也不让重载撞上正在停止的 Nest。
	// 等 worker 退出超时也继续停 Nest，错误一并返回。
	resyncErr := m.stopUnloadResync(ctx)
	if err := m.engine.Shutdown(ctx); err != nil {
		return errors.Join(resyncErr, err)
	}
	m.unhookEntitySync()
	if m.entitySync != nil {
		if err := m.entitySync.Stop(ctx); err != nil {
			return errors.Join(resyncErr, err)
		}
		if err := m.entitySync.Drain(ctx); err != nil {
			return errors.Join(resyncErr, err)
		}
		return errors.Join(resyncErr, m.entitySync.Close(ctx))
	}
	return resyncErr
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
