package entity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0166 · C2 · RR-20260910-05：category 引用业务包常量时,生成物必须自己导入那个包。
//
// Go 的 import 作用域是单个文件,源文件导入 view 不能替代生成文件导入 view。
// collectEntityImports 收集了 EntityKind、SyncTopic、packer、component、dao 的限定包,
// M-05 把 category 加进标记时漏了它:生成成功,消费者编译报 undefined: view。
//
// 断言方式是解析生成物、把每一个 pkg.Sel 限定名都要求有对应 import,而不是只找某个字符串 ——
// 这一类缺陷(用了某个包却没导入)只有这样才能一次覆盖住。
func TestGeneratedWiringImportsEveryPackageItQualifies(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A category constant that lives in a business package, imported by the
	// source file and used only inside the category expression.
	body := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer category=view.EntityCategoryPlayer", 1)
	if body == string(source) {
		t.Fatal("fixture marker not found")
	}
	body = strings.Replace(body,
		`"github.com/tjbdwanghaibo/roost-core/entity"`,
		"\"github.com/tjbdwanghaibo/roost-core/entity\"\n\tview \"github.com/tjbdwanghaibo/cube/game/view\"", 1)
	body += "\nvar _ = view.EntityCategoryPlayer\n"
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, out, raw, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, raw)
	}
	imported := map[string]bool{}
	for _, spec := range file.Imports {
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			path := strings.Trim(spec.Path.Value, `"`)
			name = path[strings.LastIndex(path, "/")+1:]
		}
		imported[name] = true
	}
	// Locals shadow nothing here: the generated file declares no package-level
	// identifier that could be mistaken for a qualifier.
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			declared[fn.Name.Name] = true
		}
	}
	missing := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Obj != nil || declared[ident.Name] {
			return true
		}
		if !imported[ident.Name] {
			missing[ident.Name] = true
		}
		return true
	})
	if len(missing) != 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		t.Fatalf("generated wiring qualifies %v without importing it:\n%s", names, raw)
	}
	if !imported["view"] {
		t.Fatalf("the category's package was not imported:\n%s", raw)
	}
}
