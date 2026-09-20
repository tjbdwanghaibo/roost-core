package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

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

func (b *bindingVerifier) Verify(context.Context, Credential) (Verified, error) {
	return Verified{}, nil
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

func boundMod(verifier Verifier, pending PendingOrders) *Mod {
	if verifier == nil {
		verifier = VerifierFunc(func(context.Context, Credential) (Verified, error) { return Verified{}, nil })
	}
	return NewMod(
		verifier,
		PlayerResolverFunc(func(context.Context, Verified) (int64, error) { return 1, nil }),
		DelivererFunc(func(context.Context, Order) error { return nil }),
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
	err := provideMod(t, boundMod(nil, PendingOrdersFunc(func(context.Context, int) ([]string, error) { return nil, nil })))
	if err == nil || !strings.Contains(err.Error(), string(mods.ModRedis)) {
		t.Fatalf("Provide should have failed on the missing Redis capability, got %v", err)
	}
}
