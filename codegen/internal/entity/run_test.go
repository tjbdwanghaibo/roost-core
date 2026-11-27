package entity

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 工程总表只查 game/entities/<包>。索引之外不能悄悄接受关系，只生成一个运行期总是失败的 helper。
func TestCompanionOutsideProjectIndexRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "game", "entities", "nested", "journal")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entity.go"), []byte("package journal\n//roost:entity companionOf=Archive\ntype Journal struct{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, io.Discard); err == nil || !strings.Contains(err.Error(), "game/entities/<package>") {
		t.Fatalf("missing path diagnostic: %v", err)
	}
}

func TestRunReturnsFlagErrors(t *testing.T) {
	if err := Run([]string{"-unknown"}, io.Discard); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
	if err := Run([]string{"unexpected"}, io.Discard); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
}
