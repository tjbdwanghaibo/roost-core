package dataengine

import (
	"context"
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

type preloadAdmitterFunc func(ctx context.Context, kind entity.EntityKind, ids []int64) ([]int64, error)

func (f preloadAdmitterFunc) AdmitPreload(ctx context.Context, kind entity.EntityKind, ids []int64) ([]int64, error) {
	return f(ctx, kind, ids)
}

// 准入检查按 kind 从 Registry 取（PreloadAdmitCapability）：登记了的 kind 问它，没登记的全部加载；一个都没登记时不包一层。
func TestLocalPreloadModAsksTheAdmitterRegisteredForAKind(t *testing.T) {
	guarded, open := entity.EntityKind(216), entity.EntityKind(217)
	mod := NewLocalPreloadMod(guarded, open)
	if !slices.Equal(mod.Kinds(), []entity.EntityKind{guarded, open}) {
		t.Fatalf("Kinds() = %v", mod.Kinds())
	}
	registry := app.NewRegistry(viper.New())
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if mod.admitters() != nil {
		t.Fatal("no admitter is registered, yet the preload is wrapped")
	}
	var asked []entity.EntityKind
	if err := registry.Register(PreloadAdmitCapability(guarded), PreloadAdmitter(preloadAdmitterFunc(func(_ context.Context, kind entity.EntityKind, ids []int64) ([]int64, error) {
		asked = append(asked, kind)
		return ids[:1], nil
	}))); err != nil {
		t.Fatal(err)
	}
	admit := mod.admitters()
	if admit == nil {
		t.Fatal("the registered admitter is not used")
	}
	if got, err := admit(context.Background(), guarded, []int64{1, 2}); err != nil || !slices.Equal(got, []int64{1}) {
		t.Fatalf("guarded kind admitted %v, %v; want what its admitter answered", got, err)
	}
	if got, err := admit(context.Background(), open, []int64{1, 2}); err != nil || !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("a kind without an admitter admitted %v, %v; want all", got, err)
	}
	if !slices.Equal(asked, []entity.EntityKind{guarded}) {
		t.Fatalf("the admitter was asked about %v, want only the guarded kind", asked)
	}
}
