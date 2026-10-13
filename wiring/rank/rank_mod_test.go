package rank

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	driver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	domain "github.com/tjbdwanghaibo/roost-core/service/rank"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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

// C6：service_metrics.enabled: false 让 Mod 不把 collaborator 的 Reporter 交给服务。
func TestModHonoursTheServiceMetricsSwitch(t *testing.T) {
	cfg := viper.New()
	cfg.Set("rank.key_prefix", "roost:rank")
	cfg.Set("service_metrics.enabled", false)
	mod := NewMod(servicemetrics.NewRecorder())
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.metrics != nil {
		t.Fatal("service_metrics.enabled: false left the service reporting")
	}
}

func bugfix6ClusterRank(t *testing.T, prefix string) (*Mod, error) {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addr == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.cluster_addrs", addr)
	cfg.Set("rank.key_prefix", prefix)
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		return mod, err
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
	t.Cleanup(func() {
		_, _ = client.Del(context.Background(), prefix+":z:"+arena().String())
		_, _ = client.Del(context.Background(), prefix+":o:"+arena().String())
	})
	return mod, nil
}

func TestRankClusterRejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"rank", "{}:rank", "{rank", "{}:{valid}:rank"} {
		t.Run(prefix, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("redis.cluster_addrs", "127.0.0.1:1")
			cfg.Set("rank.key_prefix", prefix)
			if err := NewMod(nil).Init(cfg); err == nil || !strings.Contains(err.Error(), "rank.key_prefix") {
				t.Fatalf("expected configuration rejection, got %v", err)
			}
		})
	}
}
func TestRankTaggedClusterLifecycle(t *testing.T) {
	mod, err := bugfix6ClusterRank(t, fmt.Sprintf("bugfix6:{rank-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	s := mod.store
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		e, err := s.Submit(ctx, arena(), domain.Score{OwnerID: 1, Value: 10, Tie: 1}, domain.UpdateAdd, "same")
		if err != nil || e.Score.Value != 10 {
			t.Fatalf("dedup: %+v %v", e, err)
		}
	}
	p, err := s.Page(ctx, arena(), 0, 10)
	if err != nil || p.Total != 1 || len(p.Entries) != 1 {
		t.Fatalf("page: %+v %v", p, err)
	}
	if err := s.Remove(ctx, arena(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, arena(), 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{2, 3} {
		if _, err := s.Submit(ctx, arena(), domain.Score{OwnerID: id, Value: 10}, domain.UpdateSet, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reset(ctx, arena()); err != nil {
		t.Fatal(err)
	}
	size, err := s.Size(ctx, arena())
	if err != nil || size != 0 {
		t.Fatalf("reset: %d %v", size, err)
	}
}

func arena() domain.Board {
	return domain.Board{ID: "arena", Scope: domain.ScopeServer, ScopeID: 1, Season: 3}
}
