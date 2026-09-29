package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
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
		for _, key := range []string{prefix + ":win:" + k.GroupID, prefix + ":act:" + k.String(), prefix + ":disp:" + (DispatchKey{Activity: k, GameSID: 1}).String(), OwedDispatchKey(prefix, k.GroupID, 1), prefix + ":disp:" + (DispatchKey{Activity: k, GameSID: 2}).String(), OwedDispatchKey(prefix, k.GroupID, 2)} {
			_, _ = client.Del(context.Background(), key)
		}
	})
	return m, nil
}

type bugfix7FailSecondDispatch struct {
	versionstore.Store[DispatchKey, Dispatch]
}

func (s bugfix7FailSecondDispatch) Create(ctx context.Context, key DispatchKey, value Dispatch) (versionstore.Versioned[Dispatch], bool, error) {
	if key.GameSID == 2 {
		return versionstore.Versioned[Dispatch]{}, false, context.DeadlineExceeded
	}
	return s.Store.Create(ctx, key, value)
}

func TestBugfix7ActivityClusterRecoversPartialCompletion(t *testing.T) {
	m, err := bugfix7Activity(t, fmt.Sprintf("bugfix7:{activity-%d}", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := activityKey("cluster-review")
	base := m.service.cfg
	m.service.cfg.Dispatches = bugfix7FailSecondDispatch{base.Dispatches}
	if _, err := m.service.OpenActivity(ctx, k, []int32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.service.NotifyPhase(ctx, k, 1); err != nil {
		t.Fatal(err)
	}
	complete, err := m.service.NotifyPhase(ctx, k, 2)
	if !errors.Is(err, context.DeadlineExceeded) || complete.Status != StatusComplete {
		t.Fatalf("partial completion: %+v %v", complete, err)
	}
	first, found, err := m.service.LookupDispatch(ctx, k, 1)
	if err != nil || !found {
		t.Fatalf("first durable dispatch: %+v %v", first, err)
	}
	// A new service object over the same Redis repairs the complete aggregate.
	restarted, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AdvanceExpired(ctx, k.GroupID, 10); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int32{1, 2} {
		d, found, err := restarted.LookupDispatch(ctx, k, sid)
		if err != nil || !found {
			t.Fatalf("restored %d: %+v %v", sid, d, err)
		}
		if sid == 1 && d.Token != first.Token {
			t.Fatal("recovery replaced the durable token")
		}
		if owed, err := restarted.OwedDispatches(ctx, k.GroupID, sid, 10); err != nil || len(owed) != 1 {
			t.Fatalf("owed %d: %v %v", sid, owed, err)
		}
		if _, err := restarted.AckDispatch(ctx, k, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBugfix7ActivityClusterRejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"activity", "{}:activity", "{activity", "{}:{valid}:activity"} {
		t.Run(prefix, func(t *testing.T) {
			cfg := modConfig()
			cfg.Set("redis.cluster_addrs", "127.0.0.1:1")
			cfg.Set("activity.key_prefix", prefix)
			if err := NewMod(nil).Init(cfg); err == nil || !strings.Contains(err.Error(), "activity.key_prefix") {
				t.Fatalf("configuration rejection: %v", err)
			}
		})
	}
}
func TestBugfix7ActivityStandaloneKeepsPlainPrefix(t *testing.T) {
	if err := NewMod(nil).Init(modConfig()); err != nil {
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
	if a, err := m.service.NotifyPhase(ctx, k, 1); err != nil || a.Status != StatusComplete {
		t.Fatalf("notify: %+v %v", a, err)
	}
	if owed, err := m.service.OwedDispatches(ctx, k.GroupID, 1, 10); err != nil || len(owed) != 1 {
		t.Fatalf("owed: %v %v", owed, err)
	}
	d, err := m.service.AttemptDispatch(ctx, k, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := m.service.AckDispatch(ctx, k, 1, d.Token); err != nil || ack.State != DispatchAcked {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if owed, err := m.service.OwedDispatches(ctx, k.GroupID, 1, 10); err != nil || len(owed) != 0 {
		t.Fatalf("retired: %v %v", owed, err)
	}
}
