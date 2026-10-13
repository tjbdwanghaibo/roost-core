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

// v1 manifest 目录里有当前 meta 未认领的 JSON 时 tablegen 明确失败，错误文本本身说清两条恢复路：旧生成物删掉；
// 手写数据先移出、跑一次生成把 manifest 升到 v2、再移回，并指向参考文档。
func TestLegacyManifestUntrackedJSONErrorTellsRecoverySteps(t *testing.T) {
	root := t.TempDir()
	csvDir, jsonDir := filepath.Join(root, "table"), filepath.Join(root, "data")
	if err := os.MkdirAll(csvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jsonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(csvDir, "monster.csv"), []byte("id,name,level,code\n1,slime,3,a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jsonDir, "_manifest.json"), []byte("{\"version\":1,\"tables\":{}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(jsonDir, "manual.json")
	if err := os.WriteFile(manual, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := monsterMeta()
	meta.JSON = "monster.json"
	err := convertCSVToJSON([]Meta{meta}, csvDir, jsonDir, true, io.Discard)
	if err == nil {
		t.Fatal("legacy manifest with untracked JSON was accepted")
	}
	for _, want := range []string{manual, "legacy manifest", "move it out", "upgrade", "v2", "CODEGEN_REFERENCE.zh-CN.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text lacks %q: %v", want, err)
		}
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("untracked JSON touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jsonDir, "monster.json")); !os.IsNotExist(err) {
		t.Fatalf("output written before the legacy check: %v", err)
	}
}
