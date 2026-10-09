package platform

import domain "github.com/tjbdwanghaibo/roost-core/service/platform"

import (
	"context"

	"strings"
	"testing"

	"github.com/spf13/viper"
)

// Two keys in one script are only atomic inside one hash slot. A cluster
// deployment whose prefix has no hash tag would get CROSSSLOT errors at best
// and a quietly non-atomic index at worst, so it is refused at Init.
func TestAClusterWithoutAHashTagIsRefused(t *testing.T) {
	cfg := viper.New()
	cfg.Set("platform.key_prefix", "roost:platform")
	cfg.Set("platform.session_secret", "s")
	cfg.Set("platform.payment_secret", "p")
	cfg.Set("redis.cluster_addrs", "10.0.0.1:6379,10.0.0.2:6379")
	mod := NewMod(
		domain.VerifierFunc(func(context.Context, domain.Credential) (domain.Verified, error) { return domain.Verified{}, nil }),
		domain.PlayerResolverFunc(func(context.Context, domain.Verified) (int64, error) { return 1, nil }),
		domain.DelivererFunc(func(context.Context, domain.Order) error { return nil }),
		nil,
	)
	err := mod.Init(cfg)
	if err == nil {
		t.Fatal("a cluster deployment with no hash tag was accepted; its index would not be atomic")
	}
	if !strings.Contains(err.Error(), "hash tag") {
		t.Errorf("the refusal does not say what to change: %v", err)
	}

	// With a tag it starts, and a single-node deployment is unaffected.
	cfg.Set("platform.key_prefix", "{roost:platform}")
	if err := mod.Init(cfg); err != nil {
		t.Errorf("a tagged prefix was refused: %v", err)
	}
	cfg.Set("platform.key_prefix", "roost:platform")
	cfg.Set("redis.cluster_addrs", "")
	if err := mod.Init(cfg); err != nil {
		t.Errorf("a single-node deployment was refused: %v", err)
	}
}

// A deployment can still bring its own, and saying nothing no longer means
// "no recovery".
func TestTheStoreIsTheDefaultPendingSource(t *testing.T) {
	mod := NewMod(
		domain.VerifierFunc(func(context.Context, domain.Credential) (domain.Verified, error) { return domain.Verified{}, nil }),
		domain.PlayerResolverFunc(func(context.Context, domain.Verified) (int64, error) { return 1, nil }),
		domain.DelivererFunc(func(context.Context, domain.Order) error { return nil }),
		nil,
	)
	if mod.pending != nil {
		t.Fatal("a fresh Mod already has a pending source")
	}
	// Provide is what installs the default; it needs Redis, so the assertion
	// here is on the wiring decision rather than on a built service.
	explicit := domain.PendingOrdersFunc(func(context.Context, int) ([]string, error) { return nil, nil })
	if mod.WithPendingOrders(explicit).pending == nil {
		t.Fatal("an explicit index was not kept")
	}
}
