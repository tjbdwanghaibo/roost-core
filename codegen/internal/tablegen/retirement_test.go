package tablegen

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertCSVToJSONRetiresOwnedDataAndPreservesManualData(t *testing.T) {
	root := t.TempDir()
	csvDir, jsonDir := filepath.Join(root, "table"), filepath.Join(root, "data")
	if err := os.MkdirAll(csvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(csvDir, "monster.csv"), []byte("id,name,level,code\n1,slime,3,a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := monsterMeta()
	meta.JSON = "monster.json"
	if err := convertCSVToJSON([]Meta{meta}, csvDir, jsonDir, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(jsonDir, "manual.json")
	if err := os.WriteFile(manual, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := convertCSVToJSON(nil, csvDir, jsonDir, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(jsonDir, "monster.json")); !os.IsNotExist(err) {
		t.Fatalf("retired JSON remains: %v", err)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("manual JSON removed: %v", err)
	}
}

func TestConvertCSVToJSONRefusesEditedOrLegacyUnownedRetirement(t *testing.T) {
	root := t.TempDir()
	csvDir, jsonDir := filepath.Join(root, "table"), filepath.Join(root, "data")
	if err := os.MkdirAll(csvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(csvDir, "monster.csv"), []byte("id,name,level,code\n1,slime,3,a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := monsterMeta()
	meta.JSON = "monster.json"
	if err := convertCSVToJSON([]Meta{meta}, csvDir, jsonDir, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(jsonDir, meta.JSON)
	if err := os.WriteFile(jsonPath, []byte("[{\"id\":99}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := convertCSVToJSON(nil, csvDir, jsonDir, true, io.Discard); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("edited output: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("edited output removed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jsonDir, "_manifest.json"), []byte("{\"version\":1,\"tables\":{}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := convertCSVToJSON(nil, csvDir, jsonDir, true, io.Discard); err == nil || !strings.Contains(err.Error(), "legacy manifest") {
		t.Fatalf("legacy output: %v", err)
	}
}
