package dataengine

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyCheckpointWritePathIsAbsent(t *testing.T) {
	root := filepath.Clean("..")
	legacyDirectory := filepath.Join(root, "check"+"point")
	if _, err := os.Stat(legacyDirectory); err == nil {
		t.Fatalf("legacy write package still exists: %s", legacyDirectory)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	legacyImport := "github.com/tjbdwanghaibo/roost-core/" + "check" + "point"
	legacySymbols := []string{
		"Snapshot" + "WAL",
		"Entity" + "Snapshotter",
		"Remove" + "Snapshot",
		"TakePersist" + "Dirty",
		"Rollback" + "Persist",
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if path != root && filepath.Base(path) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, symbol := range legacySymbols {
			if strings.Contains(string(raw), symbol) {
				t.Errorf("legacy write symbol %s remains in %s", symbol, path)
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, importSpec := range file.Imports {
			value, _ := strconv.Unquote(importSpec.Path.Value)
			if value == legacyImport {
				t.Errorf("legacy write import remains in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
