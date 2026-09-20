package platform

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func modConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("platform.key_prefix", "roost:platform")
	cfg.Set("platform.session_secret", "s3ssion")
	cfg.Set("platform.payment_secret", "p4yment")
	return cfg
}

// This is the highest-consequence refusal in the repository. The
// implementation this replaces signed a session token for whatever player id
// arrived in the request body — there was no verifier at all, and the absence
// looked exactly like a default. A Mod that supplied one would restore that.
func TestModRefusesWithoutItsRequiredCollaborators(t *testing.T) {
	verifier := acceptingVerifier()
	players := resolver()
	deliver := newDeliverer()

	cases := []struct {
		name string
		mod  *Mod
		want string
	}{
		{"no verifier", NewMod(nil, players, deliver, nil), "identity verifier"},
		{"no player resolver", NewMod(verifier, nil, deliver, nil), "player resolver"},
		{"no deliverer", NewMod(verifier, players, nil, nil), "recharge deliverer"},
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

// Both secrets are refused when empty, at Init. An unset payment secret in
// the implementation this replaces was checked at call time, so it turned
// every provider callback into an invalid-signature refusal: a silent outage
// that looked like an attack.
func TestModRefusesAnEmptySecretAtStartupNotAtCallTime(t *testing.T) {
	for _, key := range []string{"platform.session_secret", "platform.payment_secret"} {
		t.Run(key, func(t *testing.T) {
			mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
			cfg := modConfig()
			cfg.Set(key, "")
			if err := mod.Init(cfg); err == nil {
				t.Fatalf("Init accepted an empty %s", key)
			} else if !strings.Contains(err.Error(), key) {
				t.Fatalf("the error does not name %s: %v", key, err)
			}
		})
	}
}

func TestModRefusesANonPositiveAttemptBudget(t *testing.T) {
	mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
	cfg := modConfig()
	cfg.Set("platform.delivery_attempts", 0)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a zero attempt budget; an unbounded retry queue never drains")
	}
	cfg.Set("platform.delivery_attempts", -1)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a negative attempt budget")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	if got := mod.Name(); got != mods.ModPlatform {
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
		t.Fatalf("Start on a mod that never provided returned %v", err)
	}
}
