package project

import (
	"path/filepath"
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/project` 1/2：一个目录里有两个非生成包时 PackageName 拒绝，而不是任选一个。
func TestPackageNameRefusesADirectoryWithTwoPackages(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	dir := filepath.Join(root, "mixed")
	writeFile(t, filepath.Join(dir, "a.go"), "package alpha\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package beta\n")
	info, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := info.PackageName(dir); err == nil || !strings.Contains(err.Error(), "expected one non-generated package") || !strings.Contains(err.Error(), "found 2") {
		t.Fatalf("PackageName over two packages = %v", err)
	}
}
