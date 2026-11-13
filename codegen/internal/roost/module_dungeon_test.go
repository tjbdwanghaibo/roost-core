package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 全新自定义module工程同时验收独立实体、官方LevelDAO、Player奖励和可选排行榜真实生成链。
func TestModuleDungeonInstallAndVerify(t *testing.T) {
	root, roost, _ := moduleProject(t, "labyrinth")
	roost("module", "add", "dungeon")
	if output, err := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-run", "^TestDungeon", "./game/controllers/dungeon"); err != nil {
		t.Fatalf("standalone dungeon: %v\n%s", err, output)
	}
	roost("module", "add", "leaderboard")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"profile", "dungeon", "leaderboard"} {
		if _, ok := manifest.Modules[name]; !ok {
			t.Fatalf("missing dependency %s", name)
		}
	}
	for _, path := range []string{"db/dungeon_level.go", "game/entities/dungeon/dungeon_gen_wire.go", "game/entities/dungeon/lifecycle_gen.go", "game/controllers/dungeon/controller.go", "game/handler/syncsender/dungeon_nest_gen.go", "protocol/pb/protocol.pb.go"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("missing generated integration %s: %v", path, err)
		}
	}
	source, err := os.ReadFile(filepath.Join(root, "game/controllers/dungeon/controller.go"))
	if err != nil || !strings.Contains(string(source), "example.com/studio/labyrinth/game/dungeon") {
		t.Fatalf("custom module not rendered: %v", err)
	}
	report, err := VerifyModules(context.Background(), root, []string{"dungeon"}, nil)
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
	if counts["dungeon"] != 21 || counts["dungeon+leaderboard"] != 3 {
		t.Fatalf("scenario counts=%v", counts)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "dungeon", "leaderboard")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
}
