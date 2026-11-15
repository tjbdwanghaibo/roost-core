// Package manager provides ManagerMod, the app.Mod that owns a service's
// in-memory singleton managers.
//
// The lifecycle itself — dependency-ordered start, reverse stop, rollback of
// only the managers that started, a shutdown that aborts a start still in
// progress — is roost-core/manager.Engine (M-09). This Mod is the assembly
// around it: the Mod name, the capability registration under mods.ModManager,
// and the Mod-shaped Stop / StopWithContext.
//
// Managers are per service, not per process: the same singleton may appear in
// several services' manager sets, and only the service actually running starts
// it. That is why ManagerMod is constructed per service rather than shared.
package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coremanager "github.com/tjbdwanghaibo/roost-core/framework/manager"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/spf13/viper"
)

// ErrManagerRegisterAfterStart is returned by Register once Start has run. It
// is the engine's sentinel re-exported under the wiring name, so errors.Is
// holds for callers comparing against either.
var ErrManagerRegisterAfterStart = coremanager.ErrRegisterAfterStart

// ErrManagerStartState preserves the engine's one-attempt lifecycle error.
var ErrManagerStartState = coremanager.ErrStartState

// ManagerMod starts a service's managers in dependency order and stops them in
// reverse. Layers: Service -> Mod (ManagerMod) -> Engine -> IManager.
type ManagerMod struct {
	engine *coremanager.Engine
}

// NewManagerMod builds a mod for the given managers, in registration order.
// Order among managers that declare no dependency on each other is preserved.
func NewManagerMod(managers ...app.IManager) *ManagerMod {
	return &ManagerMod{engine: coremanager.NewEngine(managers...)}
}

// Register appends a manager. Assembly normally happens through
// NewManagerMod; Register exists for conditional wiring.
func (m *ManagerMod) Register(manager app.IManager) error { return m.engine.Register(manager) }

// MustRegister is Register for assembly code that has no error path. It panics
// rather than continue with a manager that will never start.
func (m *ManagerMod) MustRegister(manager app.IManager) { m.engine.MustRegister(manager) }

// Managers returns the registered managers in registration order, as a copy.
func (m *ManagerMod) Managers() []app.IManager { return m.engine.Managers() }

// Manager returns the registered manager with the given name.
func (m *ManagerMod) Manager(name string) (app.IManager, bool) { return m.engine.Manager(name) }

func (m *ManagerMod) Name() app.ModName         { return mods.ModManager }
func (m *ManagerMod) Init(_ *viper.Viper) error { return nil }

// managerInfrastructure 是 manager 的后台工作会调用、因而要比 manager 先启动后停止的 Mod（D22）：模块的后台循环
// （恢复扫描、CellWorld 的 Node、换服的后台步骤）经 Nest、Remote、DataEngine、Redis、NATS、Saga 与心跳工作，停机时
// manager 取消并在期限内排空它们，那时这些依赖必须都还在；排空超期时 ManagerMod 返回期限错误，App 不再停排在它之前
// 启动的 Mod（见 framework/app 的 stopModsReverseBefore），依赖与单实例锁照样保留。
//
// 用 OptionalDependsOn：服务没装的不算缺失。实体定时器（entity_timer）不在其中：定时器的 handler 可以用 manager 发布的
// 能力，manager 不靠定时器排空。玩家接入、模块的 RPC Mod 这类入口排在 ManagerMod 之后（登记在它之后，或自己 DependsOn
// manager），停机时先停。
var managerInfrastructure = []app.ModName{
	mods.ModConfigData, mods.ModMongo, mods.ModRedis, mods.ModNats, mods.ModEtcd,
	mods.ModDataEngine, mods.ModDataEnginePreload, mods.ModNest,
	mods.ModRemoteEntity, mods.ModRemoteEntityPreload, mods.ModRemoteMirror,
	mods.ModSyncBus, mods.ModSaga, mods.ModHeartbeat, mods.ModLock,
}

// OptionalDependsOn 让 ManagerMod 排在 managerInfrastructure 之后启动、之前停止（不论 bootstrap 里的登记顺序）。
func (m *ManagerMod) OptionalDependsOn() []app.ModName {
	return append([]app.ModName(nil), managerInfrastructure...)
}

// Provide hands the registry to the engine (every manager receives it from
// Start) and publishes the mod under mods.ModManager.
func (m *ManagerMod) Provide(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("manager mod: registry is nil")
	}
	if err := m.engine.Provide(r); err != nil {
		return err
	}
	return r.Register(mods.ModManager, m)
}

func (m *ManagerMod) Start() error { return m.engine.Start() }

func (m *ManagerMod) Stop() {
	if err := m.engine.Stop(fctx.BaseContext()); err != nil {
		slog.Error("manager stop failed", "err", err)
	}
}

func (m *ManagerMod) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	return m.engine.Stop(ctx)
}

var (
	_ app.Mod                           = (*ManagerMod)(nil)
	_ app.ModStopperWithContext         = (*ManagerMod)(nil)
	_ app.ModOptionalDependencyProvider = (*ManagerMod)(nil)
)
