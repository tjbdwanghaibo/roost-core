package activity

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// D-L3：活动窗口与协调器读业务时钟（time.logic_offset）。game 一端按业务时钟算窗口 id 与关窗截止，
// 协调器一端必须用同一个钟判断截止、宽限与过期，否则偏移非 0 时两端错开一个偏移量。
// 修前 Mod 不给 Config.Now，协调器退回 time.Now，配了 +24h 偏移也照真实时间走。
func TestTheModWiresTheCoordinatorToTheBusinessClock(t *testing.T) {
	cfg := modConfig()
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
	got := mod.service.cfg.Now()
	after := time.Now()
	if got.Before(before.Add(24*time.Hour-time.Second)) || got.After(after.Add(24*time.Hour+time.Second)) {
		t.Fatalf("the coordinator's clock reads %v, %v from the wall clock; want the business clock, 24h ahead",
			got.Format(time.RFC3339), got.Sub(before).Round(time.Second))
	}
	// 派发的重试排期与进度凭证有效期是系统时钟，不跟偏移走。
	if system := mod.service.cfg.SystemNow(); system.Sub(time.Now()).Abs() > time.Second {
		t.Fatalf("the coordinator's system clock reads %v, %v from the wall clock; want the wall clock",
			system.Format(time.RFC3339), system.Sub(time.Now()).Round(time.Second))
	}
	// 过期判定跟着走：一个按真实时间还剩 1 小时的宽限期，在业务时钟上已经过了。
	nowUnix := nowUnix(mod.service)
	collecting := Activity{Status: StatusCollecting, GraceDeadlineUnix: time.Now().Add(time.Hour).Unix()}
	if !collecting.GraceExpired(nowUnix) {
		t.Fatalf("an activity due in an hour of wall time is not past its grace at business time %d; the coordinator is not on the business clock", nowUnix)
	}
}
