package modulehost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// rulesConfig 是一个模块的配置声明（清单的 config:）：键、缺省值与范围写在 tag 上，跨键规则写在 ValidateConfig。
type rulesConfig struct {
	PerLevel int64         `config:"rules.per_level" default:"100" min:"1" example:"100" help:"每级经验"`
	Max      int64         `config:"rules.max" default:"100" min:"0"`
	HardCap  int64         `config:"rules.hard_cap" default:"999" min:"0"`
	Regen    time.Duration `config:"rules.regen_interval" default:"5m" min:"0"`
}

func (c *rulesConfig) ValidateConfig(bool) error {
	if c.HardCap < c.Max {
		return errors.New("config: rules.hard_cap must not be below rules.max")
	}
	return nil
}

// boardConfig 是另一个模块的声明，与 rulesConfig 装在同一个进程里。
type boardConfig struct {
	BoardID string `config:"board.id" default:"total"`
}

func configured(values map[string]any) *viper.Viper {
	cfg := viper.New()
	for key, value := range values {
		cfg.Set(key, value)
	}
	return cfg
}

// 模块的配置声明经这个 Mod 进入 App 的配置声明：--print-config-schema 列得出、--check-config（app.CheckConfig）
// 按它检查值，跨键规则（ValidateConfig）也在检查时执行，不等到进程启动。错误点名键。
func TestModuleConfigDeclarationsAreCheckedLikeAModsOwn(t *testing.T) {
	var rules Value[rulesConfig]
	var board Value[boardConfig]
	mod := NewMod("game", "", Module{Name: "rules", Config: ConfigOf(rules.Use)}, Module{Name: "board", Config: ConfigOf(board.Use)})

	declared := map[string]bool{}
	for _, key := range mod.ConfigSchema().Keys {
		declared[key.Name] = true
	}
	for _, key := range []string{"rules.per_level", "rules.max", "rules.hard_cap", "rules.regen_interval", "board.id"} {
		if !declared[key] {
			t.Fatalf("the module host Mod does not declare %s; declared: %v", key, declared)
		}
	}
	if err := app.CheckConfig(configured(map[string]any{"server_type": "game", "sid": 1}), mod); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
	err := app.CheckConfig(configured(map[string]any{"server_type": "game", "sid": 1, "rules.per_level": 0}), mod)
	if err == nil || !strings.Contains(err.Error(), "rules.per_level") {
		t.Fatalf("CheckConfig with rules.per_level: 0 = %v, want an error naming rules.per_level", err)
	}
	err = app.CheckConfig(configured(map[string]any{"server_type": "game", "sid": 1, "rules.max": 1000}), mod)
	if err == nil || !strings.Contains(err.Error(), "rules.hard_cap must not be below rules.max") {
		t.Fatalf("CheckConfig with hard_cap below max = %v, want the module's own cross-key rule", err)
	}
}

// Init 按声明装载一次，把值交给模块（Use）；之前与没有装载过的进程（模块场景测试）读到声明的缺省值。
func TestModuleConfigIsLoadedAtInitAndHandedToTheModule(t *testing.T) {
	var rules Value[rulesConfig]
	if got := rules.Get(); got.PerLevel != 100 || got.HardCap != 999 || got.Regen != 5*time.Minute {
		t.Fatalf("before loading: %+v, want the declared defaults", got)
	}
	mod := NewMod("game", "", Module{Name: "rules", Config: ConfigOf(rules.Use)})
	if err := mod.Init(configured(map[string]any{"rules.per_level": 7, "rules.regen_interval": "1m"})); err != nil {
		t.Fatal(err)
	}
	if got := rules.Get(); got.PerLevel != 7 || got.Regen != time.Minute || got.Max != 100 {
		t.Fatalf("after Init: %+v, want per_level 7 and regen 1m from the config, max from the default", got)
	}
	// 只声明、自己在入口里读配置的模块（use 为 nil）：Init 不装载，检查照常。
	declaredOnly := NewMod("account", owner, Module{Name: "portal", Start: func(*app.Registry) (func(context.Context) error, error) { return nil, nil }, Config: ConfigOf[boardConfig](nil)})
	if err := declaredOnly.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if len(declaredOnly.ConfigSchema().Keys) == 0 {
		t.Fatal("a declaration without a receiver is still a declaration")
	}
	if err := NewMod("game", "", Module{Name: "nothing"}).Init(viper.New()); err == nil {
		t.Fatal("a module with neither a start function nor a config was accepted")
	}
}
