package platform

import domain "github.com/tjbdwanghaibo/roost-core/service/platform"

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
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func TestBugfix6PlatformTaggedClusterControl(t *testing.T) {
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

func TestBugfix6PlatformClusterRejectsMalformedTag(t *testing.T) {
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
