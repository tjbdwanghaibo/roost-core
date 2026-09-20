package nest

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

type emptyOptionsProvider struct{}

func (emptyOptionsProvider) NestOptions() []corenest.NestOption { return nil }

// U-0150 · C2 · gap map kit `nest` 6/6：无实体获取器的 Mod 不能 Init / Provide；Provide 拒绝 nil 注册表、
// 缺数据引擎能力、数据引擎未初始化（无选项）；未 Provide 就 Start 拒绝。
func TestModRefusesMissingGetterRegistryAndDataEngine(t *testing.T) {
	cfg := viper.New()
	var none *Mod
	if err := none.Init(cfg); err != corenest.ErrGetterNotSet {
		t.Fatalf("Init on a nil mod = %v", err)
	}
	if err := none.Provide(app.NewRegistry(cfg)); err != corenest.ErrGetterNotSet {
		t.Fatalf("Provide on a nil mod = %v", err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(nil); err == nil || !strings.Contains(err.Error(), "nil app registry") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	if err := mod.Provide(app.NewRegistry(cfg)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Provide without the data engine capability = %v", err)
	}
	uninitialised := app.NewRegistry(cfg)
	if err := uninitialised.Register(mods.ModDataEngine, emptyOptionsProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(uninitialised); err == nil || !strings.Contains(err.Error(), "is not initialized") {
		t.Fatalf("Provide with an uninitialised data engine = %v", err)
	}
	if err := mod.Start(); err == nil || !strings.Contains(err.Error(), "engine not provided") {
		t.Fatalf("Start before Provide = %v", err)
	}
}
