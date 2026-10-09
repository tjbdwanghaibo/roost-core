package kit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P3b: kit Mods configure, publish and forward; they do not orchestrate driver
// internals. The one door into those internals is the drivers' Raw() accessor
// — a Mod that calls it is assembling by hand what core's Assemble* should own.
// Every non-test Go file in the root module is checked. Tests keep their
// freedom: Mod-level integration fixtures may need the raw handle.
func TestKitModsDoNotReachForRawDriverHandles(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range rawHandleCalls(t, path, source) {
			t.Errorf("%s:%d: kit Mod calls Raw(); assembly belongs in core", path, line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rawHandleCalls returns the lines on which source calls a method named Raw
// with no arguments.
func rawHandleCalls(t *testing.T, name string, source []byte) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Raw" {
			lines = append(lines, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return lines
}

func TestRawHandleCallsDetector(t *testing.T) {
	source := []byte("package x\n\nfunc f(c interface{ Raw() int }) int {\n\treturn c.Raw()\n}\n\nfunc g() { _ = raw }\n\nvar raw = 1\n")
	if got := rawHandleCalls(t, "probe.go", source); len(got) != 1 || got[0] != 4 {
		t.Fatalf("detector lines = %v, want [4]", got)
	}
	if got := rawHandleCalls(t, "clean.go", []byte("package x\n\nfunc h(n int) int { return n }\n")); len(got) != 0 {
		t.Fatalf("detector flagged a clean file: %v", got)
	}
}
