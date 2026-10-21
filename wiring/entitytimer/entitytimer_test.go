package entitytimer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	coretimer "github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 正式 Mod 链路：Provide 把 Registry 里的 Nest 客户端交给驱动，业务时间是进程业务时钟（执行里冻结的 fctx.Now），
// Start / Stop 管投递循环；没有 Nest 客户端时在 Provide 点名拒绝。
func TestModBindsNestAndReadsTheFrozenBusinessTime(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	driver := coretimer.NewDriver(coretimer.Config{Name: "wiring.timer", Handler: nest.NewHandlerName("handlerTickWiringTimer")})
	mod := NewMod(access, driver)
	if mod.Name() != mods.ModEntityTimer || len(mod.DependsOn()) != 1 || mod.DependsOn()[0] != mods.ModNest {
		t.Fatalf("name %q depends on %v", mod.Name(), mod.DependsOn())
	}
	cfg := viper.New()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}

	empty := app.NewRegistry(cfg)
	if err := mod.Provide(empty); err == nil || !strings.Contains(err.Error(), "nest client is unavailable") {
		t.Fatalf("Provide without nest = %v", err)
	}

	// 进程业务时钟 = 真实时间 + time.logic_offset（App 启动时设偏移，这里照做）；偏移一年，驱动的时间必须跟着走。
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
	if got := driver.Now(); time.Until(got) < offset-time.Minute {
		t.Fatalf("driver clock = %v, want the process business clock (offset %v)", got, offset)
	}
	message, release := fctx.NewContext()
	if got, want := driver.Now(), time.UnixMilli(message.Now.UnixMilli()); !got.Equal(want) {
		t.Fatalf("driver clock inside an execution = %v, want the execution's frozen business time %v", got, want)
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
}

func TestModRefusesADriverInstalledTwice(t *testing.T) {
	driver := coretimer.NewDriver(coretimer.Config{Name: "dup.timer", Handler: nest.NewHandlerName("handlerTickDupTimer")})
	err := NewMod(entity.NewManagerAccess(entity.NewEntityManager()), driver, driver).Init(viper.New())
	if err == nil || !strings.Contains(err.Error(), "installed twice") {
		t.Fatalf("Init = %v", err)
	}
}
