package account

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("account.key_prefix", "roost:account")
	cfg.Set("account.session_secret", "s3cret")
	return cfg
}

// Each of these three was ABSENT in the implementation this replaces, and the
// absence looked like a default. A Mod that supplied one would put the
// vulnerability back — so the Mod must refuse to start without them, and that
// refusal is the only load-bearing thing in the file.
func TestModRefusesWithoutItsRequiredCollaborators(t *testing.T) {
	verifier := acceptingVerifier()
	allocator := &durableAllocator{}
	rules := simpleNameRules()

	cases := []struct {
		name string
		mod  *Mod
		want string
	}{
		{"no verifier", NewMod(nil, allocator, rules, nil), "identity verifier"},
		{"no allocator", NewMod(verifier, nil, rules, nil), "player id allocator"},
		{"no name rules", NewMod(verifier, allocator, nil, nil), "name validator"},
		{"none at all", NewMod(nil, nil, nil, nil), "identity verifier"},
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

// The session secret is refused when empty, at Init. A secret checked at call
// time turns an unset value into a silent outage.
func TestModRefusesAnEmptySessionSecret(t *testing.T) {
	mod := NewMod(acceptingVerifier(), &durableAllocator{}, simpleNameRules(), nil)
	cfg := viper.New()
	cfg.Set("account.key_prefix", "roost:account")
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a missing session secret")
	}
	cfg.Set("account.session_secret", "  ")
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a whitespace-only session secret")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(acceptingVerifier(), &durableAllocator{}, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModAccount {
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
	err := mod.Provide(app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("Provide succeeded with no Redis capability")
	}
	if !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("the error does not name the missing capability: %v", err)
	}
	// And Stop on a mod that never provided must not panic: a process that
	// failed to start still runs its shutdown path.
	mod.Stop()
	if err := mod.Start(); err != nil {
		t.Fatalf("Start on a mod that never provided returned %v", err)
	}
}
