package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coreRedis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	domain "github.com/tjbdwanghaibo/roost-core/service/platform"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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

// Collaborators are built before the app exists — bootstrap constructs them
// and hands them to NewMod — so a constructor cannot receive a registry. The
// pending-order index is exactly the collaborator that needs one: it keeps its
// index in the same Redis the service stores orders in, and it reads the order
// back to know when an entry may retire. Without a binding hook it would have
// to build its own client out of configuration it cannot see.
//
// account solved this with RegistryBound (account/identity.go); platform gets
// the same hook and the same rule: binding happens in Provide, after the
// process's other Mods have published their capabilities and BEFORE the
// service is built, and a binding error stops the process.

type bindingPending struct {
	bound    int
	registry *app.Registry
	fail     error
}

func (b *bindingPending) BindRegistry(r *app.Registry) error {
	b.bound++
	b.registry = r
	return b.fail
}

func (b *bindingPending) PendingOrders(context.Context, int) ([]string, error) { return nil, nil }

type bindingVerifier struct{ bound int }

func (b *bindingVerifier) BindRegistry(*app.Registry) error { b.bound++; return nil }

func (b *bindingVerifier) Verify(context.Context, domain.Credential) (domain.Verified, error) {
	return domain.Verified{}, nil
}

// provideMod initialises a Mod from a valid configuration and runs Provide
// against a registry with no capabilities, which is what a process that has
// not published Redis looks like.
func provideMod(t *testing.T, mod *Mod) error {
	t.Helper()
	cfg := viper.New()
	cfg.Set("platform.key_prefix", "test:platform")
	cfg.Set("platform.session_secret", "session-secret")
	cfg.Set("platform.payment_secret", "payment-secret")
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	return mod.Provide(app.NewRegistry(viper.New()))
}

func boundMod(verifier domain.Verifier, pending domain.PendingOrders) *Mod {
	if verifier == nil {
		verifier = domain.VerifierFunc(func(context.Context, domain.Credential) (domain.Verified, error) { return domain.Verified{}, nil })
	}
	return NewMod(
		verifier,
		domain.PlayerResolverFunc(func(context.Context, domain.Verified) (int64, error) { return 1, nil }),
		domain.DelivererFunc(func(context.Context, domain.Order) error { return nil }),
		nil,
	).WithPendingOrders(pending)
}

// Binding runs before the order store is built, so a collaborator that looks a
// capability up sees the same registry the store construction would.
func TestCollaboratorsAreBoundBeforeTheServiceIsBuilt(t *testing.T) {
	index, verifier := &bindingPending{}, &bindingVerifier{}
	err := provideMod(t, boundMod(verifier, index))
	if err == nil || !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("this registry has no Redis, so Provide should fail on it; got %v", err)
	}
	if index.bound != 1 || verifier.bound != 1 {
		t.Fatalf("bound pending=%d verifier=%d, want exactly one each", index.bound, verifier.bound)
	}
	if index.registry == nil {
		t.Fatal("the bound index received a nil registry")
	}
}

func TestABindingFailureStopsTheProcess(t *testing.T) {
	index := &bindingPending{fail: errors.New("no index keyspace")}
	err := provideMod(t, boundMod(nil, index))
	if err == nil {
		t.Fatal("a collaborator that could not bind let the process start")
	}
	// Named, because the Redis lookup further down cannot say which
	// collaborator wanted what.
	if !strings.Contains(err.Error(), "pending order index") || !strings.Contains(err.Error(), "no index keyspace") {
		t.Errorf("the error does not name the collaborator and its cause: %v", err)
	}
}

// Binding is opt-in: the function-adapted collaborators are untouched.
func TestCollaboratorsThatDoNotAskAreNotTouched(t *testing.T) {
	err := provideMod(t, boundMod(nil, domain.PendingOrdersFunc(func(context.Context, int) ([]string, error) { return nil, nil })))
	if err == nil || !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("Provide should have failed on the missing Redis capability, got %v", err)
	}
}
