package roost

import (
	"context"
	"testing"
)

// 从普通 game 起步，验证 gift 不借用 demo 的控制器、驻留表或预先存在的 Saga。
func TestModuleGiftStandalone(t *testing.T) {
	root, roost, _ := moduleProject(t, "giftonly")
	roost("module", "add", "gift")
	result, err := VerifyModules(context.Background(), root, []string{"gift"}, nil)
	if err != nil || len(result.Modules) != 1 || !result.Modules[0].OK || result.Modules[0].Passed != 7 || result.Modules[0].Skipped != 0 {
		t.Fatalf("gift verify: %+v, %v", result, err)
	}
	runGeneratedGo(t, root, "build", "./...")
	before := moduleProjectDigest(t, root)
	roost("module", "add", "gift")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed the project")
	}
}
