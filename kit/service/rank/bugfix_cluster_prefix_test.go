package rank

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	driver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

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
		_, _ = client.Del(context.Background(), mod.store.boardKey(arena()))
		_, _ = client.Del(context.Background(), mod.store.ownerKey(arena()))
	})
	return mod, nil
}

func TestBugfix6RankClusterRejectsInvalidPrefix(t *testing.T) {
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
func TestBugfix6RankTaggedClusterLifecycle(t *testing.T) {
	mod, err := bugfix6ClusterRank(t, fmt.Sprintf("bugfix6:{rank-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	s := mod.store
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		e, err := s.Submit(ctx, arena(), Score{OwnerID: 1, Value: 10, Tie: 1}, UpdateAdd, "same")
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
		if _, err := s.Submit(ctx, arena(), Score{OwnerID: id, Value: 10}, UpdateSet, ""); err != nil {
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
