package nest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 生成签名必须使用可引用的参数名，并且只导入实际使用的类型包。
func TestABlankParameterNameGeneratesUsableCode(t *testing.T) {
	dir := t.TempDir()
	source := `package handler

import player "example.com/demo/game/entities/player"

//roost:nest rollback=undo durability=strict
func handlerGrant(target player.IBagEntity, orderID string, _ int64) (bool, error) {
	return true, nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "grant.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Every generated file, the wrapper and both sender packages: a blank
	// name must never be something the generated code declares or passes.
	found := false
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_nest_gen.go") {
			return err
		}
		found = true
		generated, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, forbidden := range []string{"_ int64", "paidAtUnix, _)", ", _ "} {
			if strings.Contains(string(generated), forbidden) {
				t.Errorf("%s carries the blank parameter name (%q):\n%s", filepath.Base(path), forbidden, generated)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("nothing was generated")
	}
}

func unusedImports(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	used := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := selector.X.(*ast.Ident); ok {
			used[ident.Name] = true
		}
		return true
	})
	var unused []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", spec.Path.Value, err)
		}
		name := path
		if index := strings.LastIndex(path, "/"); index >= 0 {
			name = path[index+1:]
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			name = spec.Name.Name
		}
		if !used[name] {
			unused = append(unused, path)
		}
	}
	return unused
}

func TestHandlerSideGenerationDoesNotImportReturnOnlyPackages(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "handler.go")
	// The shape a project hits as soon as a handler answers with a domain
	// type of its own: the result type's package is used nowhere else in the
	// handler's signature.
	const body = `package handler

import (
	"example.com/game/dungeon"
	player "example.com/game/entities/player"
)

//roost:nest rollback=undo durability=strict
func handlerClaimDungeon(target player.IProfileEntity, runID string) (dungeon.ClaimResult, error) {
	_ = runID
	_ = target
	return dungeon.ClaimResult{}, nil
}
`
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	funcs, pkg, err := parseFile(source)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}

	handlerSide := filepath.Join(dir, "handler_nest_gen.go")
	if _, err := generate(funcs, pkg, handlerSide, true, false, "RegisterHandlerNestHandlers"); err != nil {
		t.Fatalf("generate handler side: %v", err)
	}
	if unused := unusedImports(t, handlerSide); len(unused) != 0 {
		t.Errorf("handler-side file imports packages it never names: %v", unused)
	}

	// The sync sender is the file that does name the return type, so its
	// import must stay — the fix must not take that one away.
	senderSide := filepath.Join(dir, "handler_sync_sender_gen.go")
	if _, err := generateSyncSender(funcs, "handler_syncsender", senderSide, true); err != nil {
		t.Fatalf("generate sync sender: %v", err)
	}
	raw, err := os.ReadFile(senderSide)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"example.com/game/dungeon"`) || !strings.Contains(string(raw), "dungeon.ClaimResult") {
		t.Errorf("the sync sender lost the return type's import:\n%s", raw)
	}
	if unused := unusedImports(t, senderSide); len(unused) != 0 {
		t.Errorf("sync-sender file imports packages it never names: %v", unused)
	}
}
