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

	"github.com/tjbdwanghaibo/roost-core/app"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	coremanager "github.com/tjbdwanghaibo/roost-core/manager"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/spf13/viper"
)

// ErrManagerRegisterAfterStart is returned by Register once Start has run. It
// is the engine's sentinel under its historical kit name, so errors.Is holds
// for callers comparing against either.
var ErrManagerRegisterAfterStart = coremanager.ErrRegisterAfterStart

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
	_ app.Mod                   = (*ManagerMod)(nil)
	_ app.ModStopperWithContext = (*ManagerMod)(nil)
)
