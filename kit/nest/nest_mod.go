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
	// stopResync 停止卸载后重载（ManagerAccess.ConfigureUnloadResync，RR-20260926-59）；未接线或已排空时为 nil。
	// 等 worker 退出超时时保留，重试再等（RR-20261005-NC-171）。
	stopResync func(context.Context) error
	// stopped 在 Nest 与 entitysync 都已停完并释放后置位，之后的 Stop 直接返回 nil。
	stopped bool
}

// entityLoadNotifier 是 DataEngine 的可选能力（kit/dataengine Mod.OnEntityLoaded）。
type entityLoadNotifier interface {
	OnEntityLoaded(func(entity.IThreadSafeEntity)) (func(), error)
}

// unloadResyncConfigurer 是 getter 的可选能力（正式装配里 getter 就是 *entity.ManagerAccess）。
type unloadResyncConfigurer interface {
	ConfigureUnloadResync(entity.UnloadedSubjectSync, entity.UnloadResyncConfig) (func(context.Context) error, error)
}

// loadTimeoutConfigurer 是 getter 的可选能力（*entity.ManagerAccess.ConfigureLoadTimeout，RR-20260926-54）。
type loadTimeoutConfigurer interface {
	ConfigureLoadTimeout(time.Duration)
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
	// unloadResync 来自 nest.unload_resync.*，零值字段取 entity 的默认（Workers 4、Attempts 5、QueueCapacity 4096）。
	unloadResync entity.UnloadResyncConfig
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

// workerPool 是 nest.fast / nest.slow 的声明：0 取框架缺省。
type workerPool struct {
	Workers       int `config:"workers" min:"0"`
	QueueCapacity int `config:"queue_capacity" min:"0"`
}

// config 是 kit/nest 读的键（维护者决定 A4 ①）。数字与时长写 0 取框架缺省（core nest / entity 的默认值）。
type config struct {
	mods.PersistenceConfig
	Nest struct {
		Fast               workerPool    `config:"fast"`
		Slow               workerPool    `config:"slow"`
		WorkerNum          int           `config:"worker_num" min:"0" example:"8"`
		HeartbeatWorkerNum int           `config:"heartbeat_worker_num" min:"0" example:"2"`
		RemoteWorkers      int           `config:"remote_workers" min:"0"`
		QueueCapacity      int           `config:"queue_capacity" min:"0" example:"4096"`
		DelayedCapacity    int           `config:"delayed_capacity" min:"0" example:"4096"`
		MaxDelay           time.Duration `config:"max_delay" min:"0" example:"24h"`
		TickDuration       time.Duration `config:"tick_duration" min:"0" example:"50ms"`
		RequestTimeout     time.Duration `config:"request_timeout" min:"0" example:"3s"`
		EntityLoadTimeout  time.Duration `config:"entity_load_timeout" min:"0" example:"30s" help:"Framework cap for one shared cold entity load; a caller's own deadline does not end it."`
		UnloadResync       struct {
			Workers       int `config:"workers" min:"0" example:"4"`
			Attempts      int `config:"attempts" min:"0" example:"5"`
			QueueCapacity int `config:"queue_capacity" min:"0" example:"4096"`
		} `config:"unload_resync" help:"Reload of unloaded entities that still have Sync subscribers (0 keeps the framework\ndefault). Worst case before the last queued entity falls back to a remove is about\nceil(queue_capacity/workers) * attempts * entity_load_timeout; see the roost-core USER_GUIDE."`
	} `config:"nest"`
	// EntitySync 只在 NewModWithEntitySync 装配时读：写了就覆盖 EntitySyncSetup.Config 的同名字段。
	EntitySync struct {
		Mode           string        `config:"mode" enum:"periodic|on_change" help:"periodic（正式缺省）或 on_change"`
		Interval       time.Duration `config:"interval" min:"0"`
		MaxFrozenBytes int64         `config:"max_frozen_bytes" min:"0"`
	} `config:"sync.entity"`
}

// ConfigSchema 声明 nest.*、sync.entity.* 与持久化引擎的选择。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	if m == nil || m.getter == nil {
		return corenest.ErrGetterNotSet
	}
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("nest mod: %w", err)
	}
	nest := settings.Nest
	m.config = engineConfig{
		fast:          corenest.WorkerPoolConfig{Workers: nest.Fast.Workers, QueueCap: nest.Fast.QueueCapacity},
		slow:          corenest.WorkerPoolConfig{Workers: nest.Slow.Workers, QueueCap: nest.Slow.QueueCapacity},
		workerNum:     nest.WorkerNum,
		hbWorkerNum:   nest.HeartbeatWorkerNum,
		remoteWorkers: nest.RemoteWorkers,
		queueCap:      nest.QueueCapacity,
		tick:          nest.TickDuration,
		timeout:       nest.RequestTimeout,
		delayedCap:    nest.DelayedCapacity,
		maxDelay:      nest.MaxDelay,
		unloadResync: entity.UnloadResyncConfig{
			Workers:       nest.UnloadResync.Workers,
			Attempts:      nest.UnloadResync.Attempts,
			QueueCapacity: nest.UnloadResync.QueueCapacity,
		},
	}
	if err := m.initEntityLoadConfig(nest.EntityLoadTimeout); err != nil {
		return err
	}
	return m.initEntitySync(settings)
}

// initEntityLoadConfig 接上 ManagerAccess 的共享冷加载上限 nest.entity_load_timeout（RR-54，缺省
// entity.DefaultEntityLoadTimeout）；卸载后重载的 nest.unload_resync.*（RR-59）在 Start 时交给 ConfigureUnloadResync
// （RR-20260927-13）。负值由声明拒绝；0 或不写时行为与之前相同。
func (m *Mod) initEntityLoadConfig(loadTimeout time.Duration) error {
	if loadTimeout == 0 {
		return nil
	}
	configurer, ok := m.getter.(loadTimeoutConfigurer)
	if !ok {
		return fmt.Errorf("nest mod: nest.entity_load_timeout is set but the entity getter %T has no ConfigureLoadTimeout", m.getter)
	}
	configurer.ConfigureLoadTimeout(loadTimeout)
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
	// 任何 fail-stop（单实例锁丢失、DataEngine fatal、Remote fatal）都由 App 的 RuntimeFailure 在唤醒
	// 停机之前先执行这个回调：立即拒绝新的和排队中的派发，模块不用各自“找 Nest、围栏”。
	// Fence 只拿 lifecycleMu 与 dispatcher.mu 各一次、不做 I/O，满足 OnFail“快速、不阻塞”的要求；
	// 失败若已经发生（例如启动期间），登记时立即围栏。
	if failure, ok := app.Lookup[*app.RuntimeFailure](registry, mods.ModRuntimeFailure); ok && failure != nil {
		failure.OnFail(m.engine.Fence)
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
			stop, err := resync.ConfigureUnloadResync(m.entitySync, m.config.unloadResync)
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
	// 句柄只在 worker 确实退出后清除：超时清掉它，重试就不再等 worker，而它的目标 entitysync 随后被关闭
	// （RR-20261005-NC-171）。
	if err := m.stopResync(ctx); err != nil {
		return err
	}
	m.stopResync = nil
	return nil
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
	if m == nil || m.engine == nil || m.stopped {
		return nil
	}
	// 三步停机（RR-20261005-NC-171）：
	//  1. 发起：停卸载后重载（停机期间不再为订阅者重载，取消在途重载，不发 remove），发起 Nest 停机——
	//     两者都幂等，等 worker 退出超时也照样发起 Nest 停机。
	//  2. 等待：在 ctx 内等重载 worker 与 Nest 排空；超时返回 ctx 错误，句柄与 entitysync 都保留，重试再等。
	//  3. 释放：两者都排空后才停止、排空并关闭 entitysync——它是重载 worker 的 Rebind / Retract 目标。
	resyncErr := m.stopUnloadResync(ctx)
	if err := m.engine.Shutdown(ctx); err != nil {
		return errors.Join(resyncErr, err)
	}
	if resyncErr != nil {
		return resyncErr
	}
	m.unhookEntitySync()
	if m.entitySync != nil {
		if err := m.entitySync.Stop(ctx); err != nil {
			return err
		}
		if err := m.entitySync.Drain(ctx); err != nil {
			return err
		}
		if err := m.entitySync.Close(ctx); err != nil {
			return err
		}
	}
	m.stopped = true
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
