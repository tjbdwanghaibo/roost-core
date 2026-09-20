package session

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
	cfg.Set("session.key_prefix", "roost:session")
	cfg.Set("session.request_ttl", time.Hour)
	return cfg
}

// A run allocates external resources and only the caller knows how to hand
// them back. A default releaser that did nothing would compile, start, pass a
// smoke test, and leak every resource every run ever took — the exact failure
// this package was extracted to prevent, reintroduced as a convenience.
func TestModRefusesWithoutAReleaser(t *testing.T) {
	err := NewMod(nil, nil).Init(modConfig())
	if err == nil {
		t.Fatal("Init accepted a mod with no releaser")
	}
	if !strings.Contains(err.Error(), "releaser") {
		t.Fatalf("the error does not name what is missing: %v", err)
	}
}

// The request ttl has no default, because the right value is a property of the
// caller's retry behaviour: past it a retried Enter is indistinguishable from
// a new one, and the caller gets a second run.
func TestModRequiresARequestTTL(t *testing.T) {
	release := newReleaser()
	cfg := viper.New()
	cfg.Set("session.key_prefix", "roost:session")
	if err := NewMod(release, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing request ttl")
	}
	cfg.Set("session.request_ttl", time.Duration(0))
	if err := NewMod(release, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero request ttl")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(newReleaser(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModSession {
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
