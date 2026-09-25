package nest

import (
	"context"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"testing"
	"time"
)

func TestEntitySyncModConfigurationAndLifecycle(t *testing.T) {
	for _, mode := range []string{"periodic", "on_change"} {
		t.Run(mode, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("sync.entity.mode", mode)
			cfg.Set("sync.entity.interval", "1s")
			mod := NewModWithEntitySync(emptyGetter{}, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })}, Configure: func(c *entitysync.ManagerConfig) { c.Interval = 50 * time.Millisecond }})
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if mod.EntitySync().Mode().String() != mode || mod.EntitySync().Interval() != 50*time.Millisecond {
				t.Fatal("config precedence")
			}
			registry := app.NewRegistry(cfg)
			if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
				t.Fatal(err)
			}
			if err := mod.Provide(registry); err != nil {
				t.Fatal(err)
			}
			if err := mod.Start(); err != nil {
				t.Fatal(err)
			}
			if err := mod.StopWithContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mod.EntitySync().Start(context.Background()); err == nil {
				t.Fatal("owned manager not closed")
			}
		})
	}
}
func TestEntitySyncModRejectsInvalidInterval(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sync.entity.interval", "nonsense")
	mod := NewModWithEntitySync(emptyGetter{}, EntitySyncSetup{})
	if err := mod.Init(cfg); err == nil {
		t.Fatal("malformed interval became default")
	}
}
