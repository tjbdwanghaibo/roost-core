package activity

import domain "github.com/tjbdwanghaibo/roost-core/service/activity"

import (
	"context"

	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"

	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func bugfix7Activity(t *testing.T, prefix string) (*Mod, error) {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addr == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.cluster_addrs", addr)
	cfg.Set("activity.key_prefix", prefix)
	cfg.Set("activity.reservation_ttl", time.Hour)
	cfg.Set("activity.groups_file", writeGroups(t, groupYAML("group-a", []int64{1, 2, 3})))
	m := NewMod(nil)
	if err := m.Init(cfg); err != nil {
		return m, err
	}
	rcfg := redis.DefaultConfig("")
	rcfg.ClusterAddrs = strings.Split(addr, ",")
	client, err := driver.NewClient(rcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := app.NewRegistry(cfg)
	if err := mods.RegisterAll(r, mods.Capability{Name: mods.ModRedis, Value: client}); err != nil {
		t.Fatal(err)
	}
	if err := m.Provide(r); err != nil {
		t.Fatal(err)
	}
	k := activityKey("cluster-review")
	t.Cleanup(func() {
		for _, key := range []string{prefix + ":win:" + k.GroupID, prefix + ":act:" + k.String(), prefix + ":disp:" + (domain.DispatchKey{Activity: k, GameSID: 1}).String(), domain.OwedDispatchKey(prefix, k.GroupID, 1), prefix + ":disp:" + (domain.DispatchKey{Activity: k, GameSID: 2}).String(), domain.OwedDispatchKey(prefix, k.GroupID, 2)} {
			_, _ = client.Del(context.Background(), key)
		}
	})
	return m, nil
}

func TestBugfix7ActivityClusterRejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"activity", "{}:activity", "{activity", "{}:{valid}:activity"} {
		t.Run(prefix, func(t *testing.T) {
			cfg := modConfig(t)
			cfg.Set("redis.cluster_addrs", "127.0.0.1:1")
			cfg.Set("activity.key_prefix", prefix)
			if err := NewMod(nil).Init(cfg); err == nil || !strings.Contains(err.Error(), "activity.key_prefix") {
				t.Fatalf("configuration rejection: %v", err)
			}
		})
	}
}
func TestBugfix7ActivityStandaloneKeepsPlainPrefix(t *testing.T) {
	if err := NewMod(nil).Init(modConfig(t)); err != nil {
		t.Fatal(err)
	}
}
func TestBugfix7ActivityTaggedClusterLifecycle(t *testing.T) {
	m, err := bugfix7Activity(t, fmt.Sprintf("review8:{activity-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := activityKey("cluster-review")
	if _, err := m.service.OpenActivity(ctx, k, []int32{1}); err != nil {
		t.Fatal(err)
	}
	if a, err := m.service.NotifyPhase(ctx, k, 1); err != nil || a.Status != domain.StatusComplete {
		t.Fatalf("notify: %+v %v", a, err)
	}
	if owed, err := m.service.OwedDispatches(ctx, k.GroupID, 1, 10); err != nil || len(owed) != 1 {
		t.Fatalf("owed: %v %v", owed, err)
	}
	d, err := m.service.AttemptDispatch(ctx, k, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := m.service.AckDispatch(ctx, k, 1, d.Token); err != nil || ack.State != domain.DispatchAcked {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if owed, err := m.service.OwedDispatches(ctx, k.GroupID, 1, 10); err != nil || len(owed) != 0 {
		t.Fatalf("retired: %v %v", owed, err)
	}
}
