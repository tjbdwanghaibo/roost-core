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

func TestProductionCodeDoesNotImportLegacyCheckpoint(t *testing.T) {
	root := filepath.Clean("..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if path != root && (filepath.Base(path) == ".git" || filepath.Base(path) == "checkpoint") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, importSpec := range file.Imports {
			value, _ := strconv.Unquote(importSpec.Path.Value)
			if value == "github.com/tjbdwanghaibo/cube-core/checkpoint" {
				t.Errorf("legacy checkpoint import remains in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
