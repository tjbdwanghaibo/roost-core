package mods

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// U-0150 · C2 · gap map kit `mods` 5/14：批量注册拒绝 nil 注册表、空能力名、批内重名；Redis 能力缺失时
// 报错并提示加 Mod。（必填时长自 A4 ① 起是各服务 Mod 的声明，用例在各服务包。）
func TestRegisterAllAndLookupsRefuseMissingInputs(t *testing.T) {
	if err := RegisterAll(nil, Capability{Name: "x", Value: 1}); err == nil || !strings.Contains(err.Error(), "nil registry") {
		t.Fatalf("RegisterAll(nil) = %v", err)
	}
	registry := app.NewRegistry(viper.New())
	if err := RegisterAll(registry, Capability{Name: "", Value: 1}); err == nil || !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("RegisterAll with an unnamed capability = %v", err)
	}
	if err := RegisterAll(registry, Capability{Name: "x", Value: 1}, Capability{Name: "x", Value: 2}); err == nil || !strings.Contains(err.Error(), `duplicate capability "x"`) {
		t.Fatalf("RegisterAll with a duplicate = %v", err)
	}
	if _, ok := registry.Get("x"); ok {
		t.Fatal("a refused batch published a capability")
	}
	if _, err := Redis(registry); err == nil || !strings.Contains(err.Error(), "add kit/redis.NewRedisMod()") {
		t.Fatalf("Redis without the redis mod = %v", err)
	}
}
