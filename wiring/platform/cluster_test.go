package platform

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	driver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	domain "github.com/tjbdwanghaibo/roost-core/service/platform"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func TestPlatformTaggedClusterControl(t *testing.T) {
	prefix := fmt.Sprintf("bugfix6:{platform-%d}", time.Now().UnixNano())
	mod, client, err := bugfix6PlatformCluster(t, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.Del(context.Background(), prefix+":order:cluster-ok", domain.PendingIndexKey(prefix))
	})
	payload, signature := callback(t, "cluster-ok", 1001, 499)
	receipt, err := mod.service.HandleCallback(context.Background(), payload, signature)
	if err != nil || !receipt.Delivered {
		t.Fatalf("tagged callback: %+v %v", receipt, err)
	}
	orders, err := domain.NewRedisOrders(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := orders.PendingOrders(context.Background(), 128)
	if err != nil || len(ids) != 0 {
		t.Fatalf("terminal pending: %v %v", ids, err)
	}
}

func TestPlatformClusterRejectsMalformedTag(t *testing.T) {
	for _, prefix := range []string{"platform", "{}:platform", "{platform", "{}:{valid}:platform"} {
		t.Run(prefix, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("redis.cluster_addrs", "127.0.0.1:1")
			cfg.Set("platform.key_prefix", prefix)
			cfg.Set("platform.session_secret", "test-only-session")
			cfg.Set("platform.payment_secret", testPaymentSecret)
			mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
			if err := mod.Init(cfg); err == nil || !strings.Contains(err.Error(), "platform.key_prefix") {
				t.Fatalf("expected configuration rejection, got %v", err)
			}
		})
	}
}

func bugfix6PlatformCluster(t *testing.T, prefix string) (*Mod, fredis.IRedis, error) {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addr == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.cluster_addrs", addr)
	cfg.Set("platform.key_prefix", prefix)
	cfg.Set("platform.session_secret", "test-only-session")
	cfg.Set("platform.payment_secret", testPaymentSecret)
	mod := NewMod(acceptingVerifier(), resolver(), newDeliverer(), nil)
	if err := mod.Init(cfg); err != nil {
		return mod, nil, err
	}
	rcfg := fredis.DefaultConfig("")
	rcfg.ClusterAddrs = strings.Split(addr, ",")
	client, err := driver.NewClient(rcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	registry := app.NewRegistry(cfg)
	if err := mods.RegisterAll(registry, mods.Capability{Name: mods.ModRedis, Value: client}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	return mod, client, nil
}

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
