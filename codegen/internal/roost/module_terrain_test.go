package roost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleTerrainInstallAndVerify(t *testing.T) {
	root, roost, _ := moduleProject(t, "terrainlab")
	roost("module", "add", "terrain")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"actor", "scene", "terrain"} {
		if _, ok := manifest.Modules[name]; !ok {
			t.Fatalf("missing dependency %s", name)
		}
	}
	for _, path := range []string{"game/terrain/service/runtime.go", "game/entities/terrain_chunk/terrain_chunk_gen_wire.go", "game/entities/terrain_building/terrain_building_gen_wire.go", "game/entities/terrain_chunk/lifecycle_gen.go", "game/handler/syncsender/terrain_nest_gen.go", "protocol/pb/protocol.pb.go", "internal/service/game/modules_gen.go"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("missing generated integration %s: %v", path, err)
		}
	}
	source, err := os.ReadFile(filepath.Join(root, "game/controllers/terrain/controller.go"))
	if err != nil || !strings.Contains(string(source), "example.com/studio/terrainlab/game/terrain") {
		t.Fatalf("custom module not rendered: %v", err)
	}
	report, err := VerifyModules(context.Background(), root, []string{"terrain"}, nil)
	if err != nil {
		t.Fatalf("verify: %v %+v", err, report)
	}
	if len(report.Modules) != 1 || report.Modules[0].Passed != 28 || report.Modules[0].Skipped != 0 {
		t.Fatalf("scenario counts=%+v", report.Modules)
	}
	roost("module", "add", "monster")
	report, err = VerifyModules(context.Background(), root, []string{"terrain"}, nil)
	if err != nil {
		t.Fatalf("terrain with monster: %v %+v", err, report)
	}
	if len(report.Modules) != 2 || report.Modules[0].Name != "terrain" || report.Modules[0].Passed != 28 || report.Modules[0].Skipped != 0 || report.Modules[1].Name != "terrain+monster" || report.Modules[1].Passed != 2 || report.Modules[1].Skipped != 0 {
		t.Fatalf("combined scenario counts=%+v", report.Modules)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "terrain")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated installation changed project")
	}
}
