package activity

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("activity.key_prefix", "roost:activity")
	cfg.Set("activity.reservation_ttl", 30*time.Minute)
	return cfg
}

// The reservation ttl has no default. It must exceed the longest client retry
// horizon: past it a replayed progress request is indistinguishable from a new
// one and the progress is applied twice.
//
// It moved here from global.Mod with the service. The section name changed
// from `global:` to `activity:`, which is the point of the split — each
// service reads its own configuration — so this also pins that the OLD key is
// not what is read: a deployment that left reservation_ttl under `global:`
// must fail loudly at Init rather than start with a defaulted window.
func TestModRequiresAReservationTTL(t *testing.T) {
	cfg := viper.New()
	cfg.Set("activity.key_prefix", "roost:activity")
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing reservation ttl")
	}
	cfg.Set("activity.reservation_ttl", time.Duration(0))
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero reservation ttl")
	}
	// The old location must not satisfy it.
	stale := viper.New()
	stale.Set("activity.key_prefix", "roost:activity")
	stale.Set("global.reservation_ttl", 30*time.Minute)
	if err := NewMod(nil).Init(stale); err == nil {
		t.Fatal("Init accepted a reservation ttl left under the old global: section; a " +
			"deployment that was not updated would start with no reservation window")
	}
}

func TestModRequiresAKeyPrefix(t *testing.T) {
	cfg := viper.New()
	cfg.Set("activity.reservation_ttl", 30*time.Minute)
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing key prefix")
	}
}

func TestModRefusesANonPositiveDispatchBudget(t *testing.T) {
	cfg := modConfig()
	cfg.Set("activity.dispatch_attempts", 0)
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero dispatch budget; an unbounded retry queue never drains")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	// This service now has a Mod of its own, so "what the Mod is called" and
	// "what it publishes" are one fact. Inside global.Mod the activity
	// capability was a literal on a line of its own, because one Mod cannot
	// take its name from two capabilities.
	if got := mod.Name(); got != CapabilityName {
		t.Fatalf("the mod is named %q but publishes %q", got, CapabilityName)
	}
	if got := mod.Name(); got != mods.ModGlobalActivity {
		t.Fatalf("the mod is named %q, which the name table does not list", got)
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

// U-0119: the groups a process back-stops come from configuration; blank
// entries are dropped and a missing key means none.
func TestModReadsSweepGroups(t *testing.T) {
	cfg := modConfig()
	cfg.Set("activity.sweep_groups", []string{"alliance-a", " ", "alliance-b"})
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if len(mod.sweepGroups) != 2 || mod.sweepGroups[0] != "alliance-a" || mod.sweepGroups[1] != "alliance-b" {
		t.Fatalf("sweep groups = %v", mod.sweepGroups)
	}
	bare := NewMod(nil)
	if err := bare.Init(modConfig()); err != nil || bare.sweepGroups != nil {
		t.Fatalf("no key: groups=%v err=%v", bare.sweepGroups, err)
	}
}
