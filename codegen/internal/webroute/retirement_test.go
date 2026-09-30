package webroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDirRetiresLastRouteWithoutRemovingManualFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "handler.go")
	if err := os.WriteFile(source, []byte(webRouteSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateDir(dir, false); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, generatedFileName)
	if err := os.WriteFile(source, []byte(strings.ReplaceAll(webRouteSource, "//roost:web", "//retired:web")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateDir(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("retired route remains: %v", err)
	}
	if err := os.WriteFile(generated, []byte("package web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateDir(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("manual file removed: %v", err)
	}
}
