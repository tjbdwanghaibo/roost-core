package configdata

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// U-0150 · C2 · gap map kit `configdata` 3/3：Provide 对缺指标 / 生命周期能力的注册表拒绝；未 Init 就 Start 拒绝。
func TestConfigDataModRefusesBareRegistriesAndStartWithoutStore(t *testing.T) {
	if err := (&Mod{}).Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModMetrics)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v", err)
	}
	if err := (&Mod{}).Start(); err == nil || !strings.Contains(err.Error(), "store is nil") {
		t.Fatalf("Start without Init = %v", err)
	}
}
