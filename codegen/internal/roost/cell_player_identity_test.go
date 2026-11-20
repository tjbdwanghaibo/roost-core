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
	// Fresh clients generated after a recipe change must still interpret the old login responses correctly.
	for module, codes := range map[string]map[string]int64{
		"enter": {"enter_created": 100000, "enter_name_long": 100001, "enter_need_create": 100002},
		"login": {"login_elsewhere": 100003}, "portal": {"portal_request_invalid": 100004, "portal_transport_unavailable": 100005},
		"cellspace": {"cellspace_disabled": 100006, "cellspace_invalid_velocity": 100007, "cellspace_not_entered": 100008, "cellspace_stale_session": 100009},
	} {
		for name, want := range codes {
			if got := m.Modules[module].IDs["errcode"][name]; got != want {
				t.Fatalf("%s.%s = %d, want %d", module, name, got, want)
			}
		}
	}
	if m.Modules["cellspace"].IDs["protocol"]["CellMove"] != 10003 || m.Modules["cellworld"].IDs["entity"]["CellPlayer"] != 3 {
		t.Fatal("recipe changed CellMove or CellPlayer ID")
	}
	if _, err := os.Stat(filepath.Join(root, "internal/service/game/service.go")); !os.IsNotExist(err) {
		t.Fatalf("unexpected default host: %v", err)
	}
	host, err := os.ReadFile(filepath.Join(root, "internal/service/arena/service.go"))
	if err != nil || !bytes.Contains(host, []byte("cellspace.Start")) {
		t.Fatalf("missing host on arena: %v", err)
	}
	// A positional DAO tag is part of the client wire format even for a recipe-only change.
	schema, err := os.ReadFile(filepath.Join(root, "db/dao_sync.proto"))
	if err != nil {
		t.Fatal(err)
	}
	_, tail, found := strings.Cut(string(schema), "message SyncPlayerDao {\n")
	msg, _, end := strings.Cut(tail, "\n}")
	wantMsg := `  int64 entity_id = 1;
  uint32 sync_format = 2047;
  SyncPlayerDaoPosXField pos_x = 4;
  SyncPlayerDaoPosYField pos_y = 5;
  SyncPlayerDaoCellHPField cell_hp = 25;
  SyncPlayerDaoCellMaxHPField cell_max_hp = 28;
  SyncPlayerDaoCellAttackField cell_attack = 29;
  SyncPlayerDaoCellPowerField cell_power = 30;`
	if !found || !end || msg != wantMsg {
		t.Fatalf("unexpected SyncPlayerDao wire definition:\n%s", msg)
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
