package ops

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// U-0150 · C2 · gap map kit `ops` 6/7：开了管理端口却没有 token 不能 Init；Provide 拒绝 nil 注册表与缺健康 /
// 指标 / 管理 / 生命周期能力的注册表（用空注册表一次覆盖第一条，其余三条与之同构且顺序在后）。
func TestOpsModRefusesTokenlessAdminAndBareRegistries(t *testing.T) {
	cfg := viper.New()
	cfg.Set("ops.admin_enabled", true)
	if err := NewOpsMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "admin_enabled requires admin_token") {
		t.Fatalf("Init with admin enabled and no token = %v", err)
	}
	mod := NewOpsMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(nil); err == nil || !strings.Contains(err.Error(), "app registry is nil") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	// 四条能力查找是链式同文本：空注册表只能钉住第一条，所以核对它点名的是 health。
	if err := mod.Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModHealth)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v, want the health capability named", err)
	}
}
