package roost

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func CheckFormat(root string) error {
	var unformatted []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "bin", "dist":
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
		formatted, err := format.Source(raw)
		if err != nil {
			return fmt.Errorf("format %s: %w", path, err)
		}
		if !bytes.Equal(raw, formatted) {
			rel, _ := filepath.Rel(root, path)
			unformatted = append(unformatted, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(unformatted) > 0 {
		sort.Strings(unformatted)
		return fmt.Errorf("unformatted Go files: %s", strings.Join(unformatted, ", "))
	}
	return nil
}
