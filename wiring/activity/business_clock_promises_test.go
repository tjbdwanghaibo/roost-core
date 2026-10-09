package activity

import domain "github.com/tjbdwanghaibo/roost-core/service/activity"

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// D-L3：活动窗口与协调器读业务时钟（time.logic_offset）。game 一端按业务时钟算窗口 id 与关窗截止，
// 协调器一端必须用同一个钟判断截止、宽限与过期，否则偏移非 0 时两端错开一个偏移量。
// 修前 Mod 不给 Config.Now，协调器退回 time.Now，配了 +24h 偏移也照真实时间走。
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
