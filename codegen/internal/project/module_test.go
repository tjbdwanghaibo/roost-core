package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverDerivesImportAndPackage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	viewDir := filepath.Join(root, "game", "view")
	writeFile(t, filepath.Join(viewDir, "view.go"), "package view\n")

	info, err := Discover(viewDir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if info.Root != root || info.ModulePath != "example.com/game" {
		t.Fatalf("info = %+v", info)
	}
	importPath, err := info.ImportPath(viewDir)
	if err != nil || importPath != "example.com/game/game/view" {
		t.Fatalf("ImportPath = %q, %v", importPath, err)
	}
	packageName, err := info.PackageName(viewDir)
	if err != nil || packageName != "view" {
		t.Fatalf("PackageName = %q, %v", packageName, err)
	}
}

func TestDiscoverRejectsDirectoryOutsideModule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	info, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := info.ImportPath(t.TempDir()); err == nil {
		t.Fatal("ImportPath outside module succeeded")
	}
}

func TestPackageNameFallsBackToGeneratedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n")
	out := filepath.Join(root, "generated")
	writeFile(t, filepath.Join(out, "gen_config.go"), "package generated\n")

	info, err := Discover(out)
	if err != nil {
		t.Fatal(err)
	}
	name, err := info.PackageName(out)
	if err != nil || name != "generated" {
		t.Fatalf("PackageName = %q, %v", name, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
