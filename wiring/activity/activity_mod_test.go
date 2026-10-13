package activity

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	kitredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	domain "github.com/tjbdwanghaibo/roost-core/service/activity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// modConfig is the smallest configuration Init accepts: the two keys with no
// default and the required groups file, here testGroupsYAML — the groups the
// directly built coordinators use too.
func modConfig(t testing.TB) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.Set("activity.key_prefix", "roost:activity")
	cfg.Set("activity.reservation_ttl", 30*time.Minute)
	cfg.Set("activity.groups_file", writeGroups(t, testGroupsYAML))
	return cfg
}

// The reservation ttl has no default. It must exceed the longest client retry
// horizon: past it a replayed progress request is indistinguishable from a new
// one and the progress is applied twice.
//
// It moved here from global.Mod with the service. The section name changed
// from `global:` to `activity:`, which is the point of the split — each
// service reads its own configuration — so this also pins that the OLD key is
// not what is read: a deployment that left reservation_ttl under `global:`
// must fail loudly at Init rather than start with a defaulted window.
func TestModRequiresAReservationTTL(t *testing.T) {
	cfg := viper.New()
	cfg.Set("activity.key_prefix", "roost:activity")
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing reservation ttl")
	}
	cfg.Set("activity.reservation_ttl", time.Duration(0))
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero reservation ttl")
	}
	// The old location must not satisfy it.
	stale := viper.New()
	stale.Set("activity.key_prefix", "roost:activity")
	stale.Set("global.reservation_ttl", 30*time.Minute)
	if err := NewMod(nil).Init(stale); err == nil {
		t.Fatal("Init accepted a reservation ttl left under the old global: section; a " +
			"deployment that was not updated would start with no reservation window")
	}
}

func TestModRequiresAKeyPrefix(t *testing.T) {
	cfg := viper.New()
	cfg.Set("activity.reservation_ttl", 30*time.Minute)
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a missing key prefix")
	}
}

func TestModRefusesANonPositiveDispatchBudget(t *testing.T) {
	cfg := modConfig(t)
	cfg.Set("activity.dispatch_attempts", 0)
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a zero dispatch budget; an unbounded retry queue never drains")
	}
}

func TestModInitAndProvideContract(t *testing.T) {
	mod := NewMod(nil)
	if err := mod.Init(modConfig(t)); err != nil {
		t.Fatal(err)
	}
	// This service now has a Mod of its own, so "what the Mod is called" and
	// "what it publishes" are one fact. Inside global.Mod the activity
	// capability was a literal on a line of its own, because one Mod cannot
	// take its name from two capabilities.
	if got := mod.Name(); got != domain.CapabilityName {
		t.Fatalf("the mod is named %q but publishes %q", got, domain.CapabilityName)
	}
	if got := mod.Name(); got != mods.ModGlobalActivity {
		t.Fatalf("the mod is named %q, which the name table does not list", got)
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
		t.Fatalf("Start returned %v", err)
	}
}

// 进程兜底处理的组来自配置，空白项丢弃；缺省是组文件里的全部组（组文件必填，所以不会是“无”）。
func TestModReadsSweepGroups(t *testing.T) {
	cfg := modConfig(t)
	cfg.Set("activity.sweep_groups", []string{"alliance-a", " ", "alliance-b"})
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if len(mod.sweepGroups) != 2 || mod.sweepGroups[0] != "alliance-a" || mod.sweepGroups[1] != "alliance-b" {
		t.Fatalf("sweep groups = %v", mod.sweepGroups)
	}
	bare := NewMod(nil)
	if err := bare.Init(modConfig(t)); err != nil || !slices.Equal(bare.sweepGroups, []string{"group-a", "group-b"}) {
		t.Fatalf("no key: groups=%v err=%v, want the groups file's [group-a group-b]", bare.sweepGroups, err)
	}
}

// 活动窗口与协调器读业务时钟（维护者决定 D-L3，time.logic_offset）：Mod 给协调器注入业务时钟，game 一端与协调器一端用同一个钟
// 判断截止、宽限与过期，偏移非 0 时两端不错开。
func TestTheModWiresTheCoordinatorToTheBusinessClock(t *testing.T) {
	cfg := modConfig(t)
	cfg.Set("time.logic_offset", "24h")
	registry := app.NewRegistry(cfg)
	// 真实的 kit 客户端，指向没人监听的地址：go-redis 惰性连接，Provide 只建存储，不发命令。
	client, err := kitredis.NewClient(fredis.DefaultConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := registry.Register(mods.ModRedis, client); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	// 此配置组合的存储使用内存；正式 Provide 的 Redis 路径由 integration/business_clock_test 验证。
	stores := domain.RedisStores{Activities: versionstore.NewMemoryStore[domain.Key, domain.Activity](), Participants: versionstore.NewMemoryStore[domain.ParticipantKey, domain.Participant](), Ledger: versionstore.NewMemoryStore[domain.RequestKey, domain.ProgressReservation](), Audits: versionstore.NewMemoryStore[domain.Key, domain.NotifyAuditLog](), Dispatches: versionstore.NewMemoryStore[domain.DispatchKey, domain.Dispatch](), Windows: versionstore.NewMemoryStore[string, domain.Window]()}
	service, err := mod.newService(stores, app.BusinessClock(registry).Now)
	if err != nil {
		t.Fatal(err)
	}
	activity, err := service.OpenActivity(context.Background(), activityKey("clock-proof"), []int32{1})
	if err != nil {
		t.Fatal(err)
	}
	got := time.Unix(activity.OpenedAtUnix, 0)
	after := time.Now()
	if got.Before(before.Add(24*time.Hour-time.Second)) || got.After(after.Add(24*time.Hour+time.Second)) {
		t.Fatalf("opened=%v before=%v after=%v", got, before, after)
	}
	collecting := domain.Activity{Status: domain.StatusCollecting, GraceDeadlineUnix: time.Now().Add(time.Hour).Unix()}
	if !collecting.GraceExpired(activity.OpenedAtUnix) {
		t.Fatal("business clock did not expire the grace period")
	}
}

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

func TestActivityClusterRejectsInvalidPrefix(t *testing.T) {
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
func TestActivityStandaloneKeepsPlainPrefix(t *testing.T) {
	if err := NewMod(nil).Init(modConfig(t)); err != nil {
		t.Fatal(err)
	}
}
func TestActivityTaggedClusterLifecycle(t *testing.T) {
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
