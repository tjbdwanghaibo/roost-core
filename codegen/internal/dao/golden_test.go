package dao

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestGoldenGeneratedOutput locks the complete generator output for the
// testdata definitions. Fragment checks (strings.Contains) cannot see an
// unintended template drift; a byte-exact golden diff can. Refresh after an
// intentional template change with:
//
//	go test ./internal/dao -run TestGoldenGeneratedOutput -update
func TestGoldenGeneratedOutput(t *testing.T) {
	defDir, err := filepath.Abs("./testdata/def")
	if err != nil {
		t.Fatal(err)
	}
	goldenDir, err := filepath.Abs("./testdata/golden")
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "testdata"}, io.Discard); err != nil {
		t.Fatal(err)
	}

	generated, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if *updateGolden {
		if err := os.RemoveAll(goldenDir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool, len(generated))
	for _, entry := range generated {
		name := entry.Name()
		seen[name] = true
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatal(err)
		}
		goldenPath := filepath.Join(goldenDir, name)
		if *updateGolden {
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		genutil.AssertGolden(t, goldenPath, got, false)
	}
	if *updateGolden {
		return
	}
	goldens, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range goldens {
		if !seen[entry.Name()] {
			t.Errorf("golden file %s was not produced by the generator (stale golden?)", entry.Name())
		}
	}
}
