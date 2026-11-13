package dataengine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 本地预加载 Mod：排在 DataEngine 与 Nest 之后；规则在 Init 强制一次（没有 builder 的 kind 让启动失败）；
// 没有声明 kind 时 Start 什么都不做。预加载本身的契约在 framework/dataengine/engine 的用例里。
func TestLocalPreloadModOrderingAndInit(t *testing.T) {
	mod := NewLocalPreloadMod(entity.EntityKind(218))
	if mod.Name() != mods.ModDataEnginePreload || !slices.Equal(mod.DependsOn(), []app.ModName{mods.ModDataEngine, mods.ModNest}) {
		t.Fatalf("name %q depends on %v, want dataengine.preload after dataengine and nest", mod.Name(), mod.DependsOn())
	}
	if err := mod.Init(viper.New()); !errors.Is(err, engine.ErrLocalPreloadKind) {
		t.Fatalf("Init with a kind that has no builder = %v, want ErrLocalPreloadKind", err)
	}
	empty := NewLocalPreloadMod()
	if err := empty.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := empty.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := empty.Start(); err != nil {
		t.Fatalf("Start without kinds = %v, want nothing to do", err)
	}
}

// 预算来自 dataengine.preload.*，Init 时按声明读一次：缺省 10 分钟、100 万个；写了的照写的；越界在 Init 报错并点名键。
func TestLocalPreloadModReadsItsBudget(t *testing.T) {
	mod := NewLocalPreloadMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if mod.limits != (engine.LocalPreloadLimits{MaxEntities: 1000000, Timeout: 10 * time.Minute}) {
		t.Fatalf("default budget %+v, want 1000000 entities and 10m", mod.limits)
	}
	cfg := viper.New()
	cfg.Set("dataengine.preload.timeout", "90s")
	cfg.Set("dataengine.preload.max_entities", 5000)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.limits != (engine.LocalPreloadLimits{MaxEntities: 5000, Timeout: 90 * time.Second}) {
		t.Fatalf("configured budget %+v, want 5000 entities and 90s", mod.limits)
	}
	bad := viper.New()
	bad.Set("dataengine.preload.max_entities", 0)
	if err := mod.Init(bad); err == nil || !strings.Contains(err.Error(), "dataengine.preload.max_entities") {
		t.Fatalf("max_entities 0 = %v, want an error naming the key", err)
	}
}
