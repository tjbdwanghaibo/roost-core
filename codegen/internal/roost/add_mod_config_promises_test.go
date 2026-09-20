package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 承诺：项目创建之后再加的 Mod（`add mod`、`add saga` 把 saga 加进服务）要把自己的配置段
// 写进该服务的两份配置（本机与生产示例），和创建时就有的 Mod 一样。旧行为：配置在创建时
// 渲染一次、此后应用自有，后加的 Mod 只能靠代码默认值——game-demo 实跑时 saga 用默认的
// 8 GiB stream_max_bytes 建流，隔离环境的 JetStream 存不下，game 进程起不来，而配置文件里
// 根本没有可改的 saga 段。
func TestAddModAppendsItsConfigSectionToExistingServiceConfigs(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}, Features: []string{"saga"}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if cfg := read("configs/service/config.game.yaml"); strings.Contains(cfg, "\nredis:\n") || strings.Contains(cfg, "\nsaga:\n") {
		t.Fatalf("precondition: a configdata-only project already has redis/saga config:\n%s", cfg)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "AllianceRally", Service: "game", Steps: []string{"Reserve", "March"}}); err != nil {
		t.Fatal(err)
	}
	dev := read("configs/service/config.game.yaml")
	for _, want := range []string{"\nredis:\n", "\nsaga:\n", "stream_max_bytes: 8589934592", "\nmongo:\n", "\nnats:\n"} {
		if !strings.Contains(dev, want) {
			t.Errorf("dev config lacks %q after add mod/saga:\n%s", want, dev)
		}
	}
	if strings.Count(dev, "\nsaga:\n") != 1 || strings.Count(dev, "\nmongo:\n") != 1 {
		t.Errorf("a section was appended twice:\n%s", dev)
	}
	prod := read("configs/service/config.game.prod.example.yaml")
	if !strings.Contains(prod, "\nsaga:\n") || !strings.Contains(prod, "\nredis:\n") {
		t.Errorf("production example lacks the added sections:\n%s", prod)
	}
	if strings.Contains(prod, "127.0.0.1") || !strings.Contains(prod, "CHANGE_ME") {
		t.Errorf("production example appended sections keep loopback addresses instead of CHANGE_ME:\n%s", prod)
	}
	// Adding the same mod again changes nothing.
	before := dev
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if after := read("configs/service/config.game.yaml"); after != before {
		t.Errorf("re-adding a mod rewrote the config:\n%s", after)
	}
}
