package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleAdditionalDAOPlanRejectsOwnedFieldsWithoutWriting(t *testing.T) {
	root, roost, _ := moduleProject(t, "attached")
	roost("module", "add", "wallet")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := ModuleSpec{Name: "combat", Entity: "Player", DAOs: []ModuleDAOField{{Field: "combatState", Type: "CombatDAO", Tag: "combat_state"}}}
	before := moduleProjectDigest(t, root)
	path, updated, err := moduleDAOFields(root, manifest, spec)
	if err != nil {
		t.Fatal(err)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("planning attached DAO wrote the project")
	}
	if !bytes.Contains(updated, []byte("*db.CombatDAO")) || !bytes.Contains(updated, []byte("`dao:\"combat_state\"`")) {
		t.Fatalf("missing aggregate declaration:\n%s", updated)
	}
	paths, err := moduleTouchedFiles(root, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, relativeSlash(root, path)) {
		t.Fatal("attached DAO aggregate is outside rollback files")
	}
	if err := os.WriteFile(path, updated, 0644); err != nil {
		t.Fatal(err)
	}
	occupied := moduleProjectDigest(t, root)
	if _, _, err := moduleDAOFields(root, manifest, spec); err == nil || !strings.Contains(err.Error(), "already contains DAO collection") {
		t.Fatalf("existing collection accepted: %v", err)
	}
	spec.DAOs[0].Tag = "combat_second"
	if _, _, err := moduleDAOFields(root, manifest, spec); err == nil || !strings.Contains(err.Error(), "already contains field") {
		t.Fatalf("existing field accepted: %v", err)
	}
	if moduleProjectDigest(t, root) != occupied {
		t.Fatal("rejected attached DAO changed source")
	}
	if got := filepath.ToSlash(path); !strings.HasSuffix(got, "game/entities/player/entity.go") {
		t.Fatal(got)
	}
}

func TestModuleAdditionalDAOManifestRejectsAmbiguousDeclarations(t *testing.T) {
	files := map[string]string{"game/x.go": "package x\n// {{ID:component:Demo}} {{ID:errcode:demo_bad}}\n"}
	valid := "daos: [{field: demoState, type: DemoDAO, tag: demo_state}]\n"
	if _, err := loadModuleCatalogFS(moduleFixtureFS(moduleFixtureYAML+valid, files)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		"daos: [{field: demo, type: DemoDAO, tag: demo_state}]\n",
		"daos: [{field: demoState, type: DemoDAO, tag: another_state}]\n",
		"daos: [{field: demoState, type: DemoDAO, tag: demo_state}, {field: demoOther, type: OtherDAO, tag: demo_state}]\n",
	} {
		if _, err := loadModuleCatalogFS(moduleFixtureFS(moduleFixtureYAML+invalid, files)); err == nil {
			t.Fatalf("accepted ambiguous declaration %s", invalid)
		}
	}
}

func TestModuleSelfSyncActivationRollsBackWithModuleFiles(t *testing.T) {
	root, roost, try := moduleProject(t, "syncrollback")
	roost("module", "add", "wallet")
	roost("add", "transport", "tcp")
	broken := filepath.Join(root, "game", "handler", "zz_broken.go")
	if err := os.WriteFile(broken, []byte("package handler\n\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken() error { return nil }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "enter"); err == nil || !strings.Contains(err.Error(), "generate after installing modules") {
		t.Fatalf("failed module generation: %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("failed install retained self_sync or partial module files")
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	roost("module", "add", "enter")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !selfSyncEnabled(manifest) {
		t.Fatal("successful install did not enable self_sync")
	}
	if _, err := checkSelfSync(root, manifest); err != nil {
		t.Fatal(err)
	}
	roost("generate", "--check")
}
