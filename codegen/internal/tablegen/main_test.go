package tablegen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMetaRootUsesTargetProjectModulePath(t *testing.T) {
	root := t.TempDir()
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	metaDir := filepath.Join(root, "configs", "schema", "game")
	writeTablegenTestFile(t, filepath.Join(metaDir, "monster.go"), `package game

//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct {
	ID int32 `+"`csv:\"id\" json:\"id\"`"+`
}
`)

	metas, err := parseMetaRoot(filepath.Join(root, "configs", "schema"))
	if err != nil {
		t.Fatalf("parse meta root: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("meta count = %d, want 1", len(metas))
	}
	if got, want := metas[0].ImportPath, "example.com/game/configs/schema/game"; got != want {
		t.Fatalf("import path = %q, want %q", got, want)
	}
}

func writeTablegenTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
