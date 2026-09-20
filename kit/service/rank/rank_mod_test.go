package rank

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// The key prefix is required and has no default. A default would be the same
// string in every deployment, so two services of the same kind sharing one
// Redis — staging beside production, two shards, a replay harness — would
// silently share state. Refusing at startup makes that a configuration error
// instead of a data corruption.
func TestModRefusesAConfigurationWithoutAKeyPrefix(t *testing.T) {
	cases := []struct {
		name string
		set  func(*viper.Viper)
	}{
		{"unset", func(*viper.Viper) {}},
		{"empty", func(v *viper.Viper) { v.Set("rank.key_prefix", "") }},
		{"whitespace only", func(v *viper.Viper) { v.Set("rank.key_prefix", "   ") }},
		{"contains whitespace", func(v *viper.Viper) { v.Set("rank.key_prefix", "roost rank") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			tc.set(cfg)
			if err := NewMod(nil).Init(cfg); err == nil {
				t.Fatalf("Init accepted a %s key prefix", tc.name)
			}
		})
	}
}

func TestModReadsItsKeyPrefix(t *testing.T) {
	cfg := viper.New()
	cfg.Set("rank.key_prefix", "roost:rank")
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.prefix != "roost:rank" {
		t.Fatalf("the mod read prefix %q", mod.prefix)
	}
}

// A missing Redis capability must fail at Provide, naming what is absent —
// not with a nil dereference on the first request.
func TestModFailsWithoutTheRedisCapability(t *testing.T) {
	mod := NewMod(nil)
	cfg := viper.New()
	cfg.Set("rank.key_prefix", "roost:rank")
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	err := mod.Provide(app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("Provide succeeded with no Redis capability registered")
	}
	if !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
}

// The Mod declares its dependency, so the app orders Redis before it rather
// than leaving the order to registration sequence.
func TestModDeclaresItsRedisDependency(t *testing.T) {
	var mod any = NewMod(nil)
	provider, ok := mod.(app.ModDependencyProvider)
	if !ok {
		t.Fatal("the Mod does not declare its dependencies, so its Provide order is incidental")
	}
	found := false
	for _, name := range provider.DependsOn() {
		if name == mods.ModRedis {
			found = true
		}
	}
	if !found {
		t.Fatalf("DependsOn does not name %q: %v", mods.ModRedis, provider.DependsOn())
	}
}

func TestModName(t *testing.T) {
	if got := NewMod(nil).Name(); got != mods.ModRank {
		t.Fatalf("the mod is named %q, want %q", got, mods.ModRank)
	}
}
