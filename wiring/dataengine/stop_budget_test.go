package dataengine

import (
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-42：dataengine.shutdown_timeout 必须通过 App 的停机预算接口生效，而不只是
// 兼容 Stop() 使用。App 的 StopWithContext 路径按声明值给截止时间（总时长足够时）。
func TestDataEngineModDeclaresShutdownTimeoutAsStopBudget(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	var declared app.Mod = mod
	budgeter, ok := declared.(interface{ StopBudget() time.Duration })
	if !ok {
		t.Fatal("dataengine mod does not declare a stop budget; App splits shutdown.total_timeout evenly instead")
	}
	if got := budgeter.StopBudget(); got != 30*time.Second {
		t.Fatalf("default stop budget=%v, want dataengine.shutdown_timeout default 30s", got)
	}
	cfg.Set("dataengine.shutdown_timeout", 45*time.Second)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := budgeter.StopBudget(); got != 45*time.Second {
		t.Fatalf("configured stop budget=%v, want 45s", got)
	}
	// 未 Init 的 Mod 不声明（<= 0 由 App 视为未声明）。
	if got := NewMod().StopBudget(); got > 0 {
		t.Fatalf("uninitialized stop budget=%v, want none", got)
	}
}
