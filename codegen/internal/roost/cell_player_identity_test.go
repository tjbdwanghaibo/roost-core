package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The recipe must use the host selected at project creation, not leave a second,
// unused service under internal/service/game. Reuse enter's standard controller.
func TestCellworldRecipeUsesTheSelectedHostAndStockEnter(t *testing.T) {
	root, _, _ := moduleProjectFrom(t, NewOptions{Name: "cellstarter", Template: "game", Recipe: "cellworld", Services: []string{"arena"}})
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"cellworld", "cellspace", "enter"} {
		if m.Modules[n].Service != "arena" {
			t.Fatalf("%s service=%s", n, m.Modules[n].Service)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "internal/service/game/service.go")); !os.IsNotExist(err) {
		t.Fatalf("unexpected default host: %v", err)
	}
	host, err := os.ReadFile(filepath.Join(root, "internal/service/arena/service.go"))
	if err != nil || !bytes.Contains(host, []byte("cellspace.Start")) {
		t.Fatalf("missing host on arena: %v", err)
	}
	const rel = "game/controllers/enter.go"
	want, err := renderModuleFile(m, "arena", ModuleSpec{Name: "enter"}, "files", rel, m.Modules["enter"].IDs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("recipe forked enter's endpoint: %v", err)
	}
}

// The demo's kind is already used in public subject IDs. A source rename must not
// allocate a second kind or move the original kind to a different slot.
func TestDemoCellPlayerKeepsItsPublishedEntityKind(t *testing.T) {
	root := copyOfNewProject(t, "game-demo")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	kinds := m.Modules["cellworld"].IDs["entity"]
	if len(kinds) != 1 || kinds["CellPlayer"] != 5 {
		t.Fatalf("cellworld entities = %v; demo client subject IDs require kind 5", kinds)
	}
	for _, path := range []string{"game/entities/scene_actor", "db/def/scene_actor.go"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("old entity still present: %s: %v", path, err)
		}
	}
	for path, code := range map[string]string{
		"game/entities/cell_player/entity.go": "type CellPlayer struct",
		"db/def/cell_player.go":               "type CellPlayerDao struct",
	} {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !strings.Contains(string(b), code) {
			t.Fatalf("%s lacks %s: %v", path, code, err)
		}
	}
}
