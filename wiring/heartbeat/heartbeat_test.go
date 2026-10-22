package heartbeat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	coreheartbeat "github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 正式 Mod 链路：依赖 Nest（Nest 先启动、后停止）；Provide 把 Registry 里的 Nest 客户端交给驱动并登记驱动，业务时间是
// 进程业务时钟（执行里冻结的 fctx.Now）；
// Start / Stop 管驱动 goroutine；没有 Nest 客户端时在 Provide 点名拒绝。
func TestModBindsNestAndReadsTheFrozenBusinessTime(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	driver := coreheartbeat.NewDriver(coreheartbeat.Config{Name: "wiring.hb"})
	mod := NewMod(access, driver)
	if mod.Name() != mods.ModHeartbeat || len(mod.DependsOn()) != 1 || mod.DependsOn()[0] != mods.ModNest {
		t.Fatalf("name %q depends on %v", mod.Name(), mod.DependsOn())
	}
	cfg := viper.New()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := NewMod(access, nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a nil driver")
	}
	if err := mod.Provide(app.NewRegistry(cfg)); err == nil || !strings.Contains(err.Error(), "nest client is unavailable") {
		t.Fatalf("Provide without nest = %v", err)
	}
	if err := mod.Start(); err == nil {
		t.Fatal("Start before Provide bound the driver")
	}

	// 业务时间不由 Mod 接：驱动缺省读 fctx.Now，即进程业务时钟（App 启动时按 time.logic_offset 设偏移，这里照做）。
	offset := 365 * 24 * time.Hour
	clock.SetOffset(offset)
	t.Cleanup(clock.Reset)
	r := app.NewRegistry(cfg)
	engine := nest.NewEngine(nest.NestOptionWithGetter(access))
	if err := r.Register(mods.ModNest, engine); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(r); err != nil {
		t.Fatal(err)
	}
	if got, ok := app.Lookup[*coreheartbeat.Driver](r, mods.ModHeartbeat); !ok || got != driver {
		t.Fatalf("registered driver = %v %v", got, ok)
	}
	if got := driver.Now(); time.Until(got) < offset-time.Minute {
		t.Fatalf("driver clock = %v, want the process business clock (offset %v)", got, offset)
	}
	message, release := fctx.NewContext()
	if got := driver.Now(); !got.Equal(message.Now) {
		t.Fatalf("driver clock inside an execution = %v, want the execution's frozen business time %v", got, message.Now)
	}
	release()
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatalf("second stop = %v", err)
	}
}

// heartbeat.delivery：缺省 batch（维护者 2026-10-22 决定）；配置值在 Init 时交给驱动（Provide 时按它接 Nest 批量作业
// 或单目标 Dispatch）；未知值在声明解码时拒绝（enum），不进驱动。驱动构造时先设成与缺省相反的 dispatch，确认“不配”
// 时是声明的缺省把它改成 batch，而不是驱动自己的空值缺省。
// 变异核对：Init 不把配置交给驱动（去掉 SetDelivery）时不配与配 batch 两种情况下驱动都仍是 dispatch，本用例变红；
// 声明缺省改回 default:"dispatch" 时“不配”一项变红。
func TestModReadsTheDeliveryOnce(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	for _, tc := range []struct {
		value string
		want  coreheartbeat.Delivery
	}{{"", coreheartbeat.DeliveryBatch}, {"batch", coreheartbeat.DeliveryBatch}, {"dispatch", coreheartbeat.DeliveryDispatch}} {
		cfg := viper.New()
		if tc.value != "" {
			cfg.Set("heartbeat.delivery", tc.value)
		}
		initial := coreheartbeat.DeliveryDispatch
		if tc.want == coreheartbeat.DeliveryDispatch {
			initial = coreheartbeat.DeliveryBatch
		}
		driver := coreheartbeat.NewDriver(coreheartbeat.Config{Name: "wiring.delivery." + tc.value, Delivery: initial})
		if err := NewMod(access, driver).Init(cfg); err != nil {
			t.Fatalf("Init(heartbeat.delivery=%q) = %v", tc.value, err)
		}
		if driver.Delivery() != tc.want {
			t.Fatalf("heartbeat.delivery=%q: driver delivery %q, want %q", tc.value, driver.Delivery(), tc.want)
		}
	}
	cfg := viper.New()
	cfg.Set("heartbeat.delivery", "broadcast")
	if err := NewMod(access, coreheartbeat.NewDriver(coreheartbeat.Config{Name: "wiring.bad"})).Init(cfg); err == nil {
		t.Fatal("Init accepted an unknown heartbeat.delivery")
	}
}
