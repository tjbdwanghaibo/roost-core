package lock

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/app"
	flock "github.com/tjbdwanghaibo/roost-core/lock"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

// LockMod implements app.Mod, initializing the global lock manager.
type LockMod struct{}

func NewLockMod() *LockMod {
	return &LockMod{}
}

func (m *LockMod) Name() app.ModName         { return mods.ModLock }
func (m *LockMod) Init(_ *viper.Viper) error { return nil }

func (m *LockMod) Provide(r *app.Registry) error {
	return r.Register(mods.ModLock, flock.NewLockManager(nil))
}

func (m *LockMod) Start() error                          { return nil }
func (m *LockMod) Stop()                                 {}
func (m *LockMod) StopWithContext(context.Context) error { return nil }

var _ app.ModStopperWithContext = (*LockMod)(nil)
