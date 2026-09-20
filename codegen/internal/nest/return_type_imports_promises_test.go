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

// U-0227 · C8 · 无 RR（实施 RR-20260917-08 时发现）：handler 半的生成文件不能 import
// 只被返回类型用到的包。旧行为：`generatedTypeRefs` 对 handler 半也收集 `f.Returns`
// 的包，而 handler 半从不写出返回类型的名字（`ret, err = handlerX(...)`，ret 是 any），
// 于是一个返回"第三个包里的类型"的 handler 生成出 `imported and not used`，整个包编译不过。
// 之前没暴露是因为已有 handler 的返回类型要么是内置类型，要么来自实体参数已经 import 的包。

// unusedImports parses a generated file and returns the imports no identifier
// in it refers to — the check `format.Source` does not do (it formats invalid
// programs happily) and the text comparisons in this package never made.
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
