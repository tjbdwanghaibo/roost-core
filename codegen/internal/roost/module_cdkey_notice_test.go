package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 自定义 module 的全新 game 工程：依赖、directory Mod、协议、配置表、服务 runtime 与真实生成场景同时验收。
func TestModuleCdkeyNoticeInstallAndVerify(t *testing.T) {
	root, roost, _ := moduleProject(t, "redemption")
	roost("module", "add", "cdkey", "notice")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wallet", "mailbox", "cdkey", "chat", "notice"} {
		if _, ok := manifest.Modules[name]; !ok {
			t.Fatalf("missing module %s", name)
		}
	}
	for _, path := range []string{"game/controllers/cdkey/controller.go", "game/handler/syncsender", "configs/data/cdkey.json", "game/notice/notice.go", "internal/service/game/modules_gen.go"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("missing generated integration %s: %v", path, err)
		}
	}
	controller, err := os.ReadFile(filepath.Join(root, "game/controllers/cdkey/controller.go"))
	if err != nil || !strings.Contains(string(controller), "example.com/studio/redemption/game/mailbox") {
		t.Fatalf("custom module not rendered: %v", err)
	}
	report, err := VerifyModules(context.Background(), root, []string{"cdkey", "notice"}, nil)
	if err != nil {
		t.Fatalf("verify: %v %+v", err, report)
	}
	counts := map[string]int{}
	for _, module := range report.Modules {
		if !module.OK || module.Skipped != 0 {
			t.Fatalf("module=%+v", module)
		}
		counts[module.Name] = module.Passed
	}
	if counts["cdkey"] != 12 || counts["notice"] != 8 {
		t.Fatalf("scenario counts=%v", counts)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "notice", "cdkey")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated installation changed the project")
	}
}
