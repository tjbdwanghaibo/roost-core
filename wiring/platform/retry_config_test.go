package platform

import (
	coreRedis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

type assemblyRedis struct{ coreRedis.IRedis }

func TestModCanExplicitlyDisableBackgroundRetry(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
		if disabled {
			mod.WithPendingOrders(nil)
		}
		cfg := modConfig()
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		r := app.NewRegistry(cfg)
		if err := r.Register(mods.ModRedis, &assemblyRedis{}); err != nil {
			t.Fatal(err)
		}
		if err := mod.Provide(r); err != nil {
			t.Fatal(err)
		}
		if mod.service.BackgroundRetryEnabled() == disabled {
			t.Fatalf("disabled=%v: background retry enabled=%v", disabled, mod.service.BackgroundRetryEnabled())
		}
	}
}
