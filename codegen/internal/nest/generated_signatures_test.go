package nest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 业务参数名是合法 Go 标识符，不应与 Sender 自己的路由参数、接收者或结果变量冲突。
func TestSenderNamesDoNotCollideWithBusinessArguments(t *testing.T) {
	dir := t.TempDir()
	source := `package handler
import player "example.com/demo/game/entities/player"
//roost:nest rollback=undo durability=strict
func handlerCancel(target player.IBagEntity, id, idArg, ctx, delay, ids, client, err, ret, ok, s, nest, ret0, clientErr, retXXX, errXXX, retValues, groupIDs int64) (int64, bool, error) { return 0, true, nil }
`
	if err := os.WriteFile(filepath.Join(dir, "cancel.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"sender", "syncsender"} {
		path := filepath.Join(dir, pkg, "cancel_nest_gen.go")
		if _, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors|parser.DeclarationErrors); err != nil {
			t.Fatalf("%s has colliding generated declarations: %v", pkg, err)
		}
	}
}

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

// sender / syncsender 是 handler 包的子包，签名里的参数与结果类型原样搬过去；类型声明在 handler 包里时子包看不见它，
// 生成物编不过（undefined: BuyItemResult）。子包又不能反向导入 handler 包（handler 会用 sender，成环），
// 所以生成器必须当场拒绝，点名类型并要求移到 handler 导入的包里，而不是写出编不过的代码。
func TestSenderRefusesATypeDeclaredInTheHandlerPackage(t *testing.T) {
	for _, tt := range []struct {
		name      string
		signature string
		local     string
	}{
		{name: "result", signature: "(target player.IShopEntity, itemID int32) (BuyItemResult, error)", local: "BuyItemResult"},
		{name: "parameter", signature: "(target player.IShopEntity, order []*BuyOrder) error", local: "BuyOrder"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			source := `package handler

import player "example.com/demo/game/entities/player"

type ` + tt.local + ` struct{ Gold int64 }

//roost:nest rollback=undo durability=strict
func handlerBuyItem` + tt.signature + ` {
	panic("unused")
}
`
			if err := os.WriteFile(filepath.Join(dir, "buy_item.go"), []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			err := Run([]string{"-dir", dir}, io.Discard)
			if err == nil {
				generated, _ := os.ReadFile(filepath.Join(dir, "syncsender", "buy_item_nest_gen.go"))
				t.Fatalf("generation accepted %s declared in the handler package; the sender packages cannot see it:\n%s", tt.local, generated)
			}
			for _, want := range []string{"handlerBuyItem", tt.local, "move"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %q: %v", want, err)
				}
			}
		})
	}
}

// Remote 请求 struct 必须声明在 handler 包里（访问器生成在这里），与 sender 子包看不见本包类型冲突。
// 报错不能再建议“移走类型”——照做访问器就生成不出来；要直接说明这个组合还不支持。
func TestSenderRefusesARemoteRequestStructWithoutSuggestingAMove(t *testing.T) {
	dir := t.TempDir()
	source := `package handler

import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	player "example.com/demo/game/entities/player"
	view "example.com/demo/game/view"
)

var _ view.PlayerViewMapSnapshot

type QueryRequest struct {
	Target entity.RemoteViewRef ` + "`" + `remote:"view.PlayerViewMapSnapshot,consistency=monotonic,required"` + "`" + `
}

//roost:nest rollback=undo durability=strict
func handlerQuery(target player.IShopEntity, req QueryRequest) error {
	panic("unused")
}
`
	if err := os.WriteFile(filepath.Join(dir, "query.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"-dir", dir}, io.Discard)
	if err == nil {
		t.Fatal("generation accepted a Remote request struct with senders; the sender packages cannot see it")
	}
	for _, want := range []string{"handlerQuery", "QueryRequest", "remote", "KNOWN-LIMITS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "move QueryRequest") {
		t.Errorf("error suggests moving the Remote request struct, which would drop its accessors: %v", err)
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
