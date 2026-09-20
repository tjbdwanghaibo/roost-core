package global

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("global.key_prefix", "roost:global")
	return cfg
}

// The activity settings this Mod used to read — reservation_ttl,
// grace_window, dispatch_attempts, dispatch_backoff — moved with the service
// to activity.Mod, and so did the tests that pin their contracts. What is left
// here has to keep requiring the key prefix, which has no default in any
// service: a defaulted prefix is two deployments quietly sharing a keyspace.
func TestModRequiresAKeyPrefix(t *testing.T) {
	if err := NewMod(nil).Init(viper.New()); err == nil {
		t.Fatal("Init accepted a missing key prefix")
	}
}

// The activity keys are no longer read from this section. A deployment that
// still sets them gets no error — viper ignores unknown keys — so this pins
// that they are also not silently REQUIRED here any more, which is what would
// keep a split deployment from starting.
func TestModIgnoresTheActivitySettingsThatMovedOut(t *testing.T) {
	cfg := modConfig()
	if err := NewMod(nil).Init(cfg); err != nil {
		t.Fatalf("Init failed with only the routing settings: %v", err)
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModGlobal {
		t.Fatalf("the mod is named %q", got)
	}
	if got := mod.Name(); got != CapabilityName {
		t.Fatalf("the mod is named %q but publishes %q", got, CapabilityName)
	}
	var asAny any = mod
	provider, ok := asAny.(app.ModDependencyProvider)
	if !ok {
		t.Fatal("the Mod does not declare its dependencies")
	}
	if len(provider.DependsOn()) == 0 || provider.DependsOn()[0] != mods.ModRedis {
		t.Fatalf("DependsOn = %v, want redis", provider.DependsOn())
	}
	if err := mod.Provide(app.NewRegistry(viper.New())); err == nil {
		t.Fatal("Provide succeeded with no Redis capability")
	} else if !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
	mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatalf("Start returned %v", err)
	}
}
