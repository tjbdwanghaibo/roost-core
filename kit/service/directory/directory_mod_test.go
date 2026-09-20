package directory

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
	cfg.Set("directory.key_prefix", "roost:directory")
	cfg.Set("directory.reservation_ttl", time.Minute)
	return cfg
}

// The normalizer decides which raw keys are the SAME key. Selecting it by
// string would mean an unrecognized string had to do something, and every
// available answer is wrong: fall back to identity and uniqueness quietly
// weakens; fall back to lower-case and a case-sensitive namespace quietly
// merges.
func TestModRefusesWithoutANormalizer(t *testing.T) {
	err := NewMod(nil, nil).Init(modConfig())
	if err == nil {
		t.Fatal("Init accepted a mod with no normalizer")
	}
	if !strings.Contains(err.Error(), "normalizer") {
		t.Fatalf("the error does not name what is missing: %v", err)
	}
}

// A reservation that never expires burns the key when the caller dies, which
// is the defect this primitive exists to prevent. There is no default because
// the right value depends on how long the caller's commit path takes.
func TestModRequiresAPositiveReservationTTL(t *testing.T) {
	cfg := viper.New()
	cfg.Set("directory.key_prefix", "roost:directory")
	if err := NewMod(NormalizeLower, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing reservation ttl")
	}
	cfg.Set("directory.reservation_ttl", time.Duration(0))
	if err := NewMod(NormalizeLower, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero reservation ttl")
	}
	cfg.Set("directory.reservation_ttl", -time.Second)
	if err := NewMod(NormalizeLower, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a negative reservation ttl")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(NormalizeLower, nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModDirectory {
		t.Fatalf("the mod is named %q", got)
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
