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
