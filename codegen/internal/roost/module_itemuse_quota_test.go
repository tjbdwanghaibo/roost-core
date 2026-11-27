package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 先装两个小模块再叠加效果与联盟宿主。只装玩家额度不引入联盟服务，只装使用道具不引入所有效果提供方。
func TestModuleItemUseQuotaIncremental(t *testing.T) {
	root, roost, _ := moduleProject(t, "supplyrules")
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "quota", "itemuse", "--json")
	if before != moduleProjectDigest(t, root) {
		t.Fatal("planning itemuse and quota wrote files")
	}
	roost("module", "add", "quota", "itemuse")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"quota", "bag", "itemuse"} {
		if _, ok := manifest.Modules[name]; !ok {
			t.Fatalf("minimal consumer missing %q", name)
		}
	}
	for _, name := range []string{"reward", "profile", "quota_alliance", "alliance", "shop", "stamina"} {
		if _, ok := manifest.Modules[name]; ok {
			t.Fatalf("minimal consumer unexpectedly requires %q", name)
		}
	}
	roost("module", "add", "reward", "profile", "quota_alliance")
	manifest, err = LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"game", "alliance"} {
		if !contains(manifest.Services[service].Mods, "configdata") {
			t.Fatalf("%s does not load quota rules", service)
		}
	}
	// 用真实安装清单运行场景，断言缺任何一个桥都不算通过。
	report, err := VerifyModules(context.Background(), root, []string{"itemuse", "quota", "quota_alliance"}, nil)
	if err != nil {
		t.Fatalf("incremental verify: %v %+v", err, report)
	}
	want := map[string]bool{"itemuse": true, "itemuse+reward": true, "itemuse+profile": true, "quota": true, "quota_alliance": true}
	for _, m := range report.Modules {
		if !want[m.Name] || m.Want < 1 || m.Passed != m.Want || m.Skipped != 0 || !m.OK {
			t.Fatalf("bad verification row: %+v", m)
		}
		delete(want, m.Name)
	}
	if !report.OK || len(want) != 0 {
		t.Fatalf("missing verification rows: %v; report OK=%v", want, report.OK)
	}
	before = moduleProjectDigest(t, root)
	roost("module", "add", "quota_alliance", "profile", "reward", "itemuse", "quota")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed existing files")
	}
	roost("generate", "--check")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "testdata", "itemuse-quota", "quota_effect_test.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fixture = bytes.ReplaceAll(fixture, []byte("{{MODULE}}"), []byte("example.com/studio/supplyrules"))
	if err := os.WriteFile(filepath.Join(root, "game", "handler", "quota_effect_generated_test.go"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-race", "-count=1", "-run", "^TestGeneratedItemUseAndOtherBusinessSharePlayerQuota$", "./game/handler")
}
