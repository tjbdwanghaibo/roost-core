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
}

// v1 manifest 没有归属记录，不再读取：转换与 `roost generate` 的跳过判断（ManifestOwnsJSON）都明确失败，
// 错误文本给出一次性清理办法并指向参考文档；失败前不写任何输出、不动目录里的 JSON。
// 其它非 v2 版本同样拒绝。
func TestTableManifestOtherThanV2IsRejected(t *testing.T) {
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
	manifestPath := filepath.Join(jsonDir, "_manifest.json")
	if err := os.WriteFile(manifestPath, []byte("{\"version\":1,\"tables\":{}}\n"), 0o644); err != nil {
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
		t.Fatal("a v1 manifest was accepted")
	}
	for _, want := range []string{manifestPath, "version 1", "no longer read", "delete it", "keep hand-maintained", "CODEGEN_REFERENCE.zh-CN.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text lacks %q: %v", want, err)
		}
	}
	if _, owns := ManifestOwnsJSON(jsonDir); owns == nil || !strings.Contains(owns.Error(), "version 1") {
		t.Errorf("ManifestOwnsJSON on a v1 manifest = %v, want the same rejection", owns)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("untracked JSON touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jsonDir, "monster.json")); !os.IsNotExist(err) {
		t.Fatalf("output written before the version check: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("{\"tables\":{}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := convertCSVToJSON([]Meta{meta}, csvDir, jsonDir, true, io.Discard); err == nil || !strings.Contains(err.Error(), "unsupported table manifest version 0") {
		t.Fatalf("a manifest without a version: %v", err)
	}
	// 按提示删掉 v1 manifest 后照常生成，手写 JSON 作为未登记文件保留。
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := convertCSVToJSON([]Meta{meta}, csvDir, jsonDir, true, io.Discard); err != nil {
		t.Fatalf("after the cleanup: %v", err)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("hand-maintained JSON removed after the cleanup: %v", err)
	}
}
