package chat

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("chat.key_prefix", "roost:chat")
	return cfg
}

// A permissive default policy is precisely what the client-settable Trusted
// bool amounted to: a way for a caller to be treated as privileged without
// anything having decided that it was. And a default SystemAuthenticator
// would mint the privilege token itself, putting back the field this package
// removed.
func TestModRefusesWithoutItsRequiredCollaborators(t *testing.T) {
	policy := allowAllPolicy{}
	registry := testRegistry(t)
	system := grantingAuth()

	cases := []struct {
		name string
		mod  *Mod
		want string
	}{
		{"no policy", NewMod(nil, registry, system, nil, nil), "channel policy"},
		{"no body registry", NewMod(policy, nil, system, nil, nil), "body registry"},
		{"no system authenticator", NewMod(policy, registry, nil, nil, nil), "system authenticator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mod.Init(modConfig())
			if err == nil {
				t.Fatalf("Init accepted a mod with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not name what is missing (%q): %v", tc.want, err)
			}
		})
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(allowAllPolicy{}, testRegistry(t), grantingAuth(), nil, nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModChat {
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

// The rules slice is copied at construction, so a caller mutating its own
// slice afterwards cannot change what the service enforces.
func TestModCopiesTheRulesItWasGiven(t *testing.T) {
	rules := []ChannelRule{{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: 4}}
	mod := NewMod(allowAllPolicy{}, testRegistry(t), grantingAuth(), rules, nil)
	rules[0].Retain = 999
	rules[0].SystemOnly = true
	if mod.rules[0].Retain != 4 {
		t.Fatalf("the mod's rule changed with the caller's slice: retain = %d", mod.rules[0].Retain)
	}
	if mod.rules[0].SystemOnly {
		t.Fatal("the mod's rule became system-only because the caller mutated its slice")
	}
}
