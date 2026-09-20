package registry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// U-0167 · C2 · RR-20260910-06：聚合注册的固定 import 名必须对动态别名预留。
//
// M-05 让模板无条件导入 core 的 entity 包(末尾要调 ValidateEntityRegistry),而
// importAliases 的保留集只有 registry / sync / fmt / err。业务注册包路径以 entity 结尾时,
// 它被分配到 entity 这个别名,生成物同时出现两个 entity:消费者编译报
// `entity redeclared in this block` 与 `undefined: entity.RegisterEntity`。
//
// 断言方式是解析生成物、要求 import 名互不重复,并且 entity 这个名字确实指向 core ——
// 只检查文本会漏掉"名字对了但指向错了"。
func TestAggregateReservesItsFixedImportNames(t *testing.T) {
	generated, err := render("example.com/alias", []Registration{
		{ImportPath: "example.com/alias/entity", Func: "RegisterEntity", Phase: "entity"},
		{ImportPath: "example.com/alias/fmt", Func: "RegisterFormats", Phase: "config"},
		{ImportPath: "example.com/alias/sync", Func: "RegisterSync", Phase: "post"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", generated, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, generated)
	}

	byName := map[string]string{}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if previous, dup := byName[name]; dup {
			t.Errorf("import name %q is used by both %q and %q; the consumer would fail to compile", name, previous, path)
		}
		byName[name] = path
	}
	if got := byName["entity"]; got != "github.com/tjbdwanghaibo/roost-core/entity" {
		t.Errorf("the name entity resolves to %q, not the core package the template calls", got)
	}
	for _, reserved := range []string{"fmt", "sync"} {
		if got := byName[reserved]; got != reserved {
			t.Errorf("the name %q resolves to %q, not the standard library package the template uses", reserved, got)
		}
	}

	// Every business registration is still called, under whatever alias it got.
	var called []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok {
				called = append(called, ident.Name+"."+sel.Sel.Name)
			}
		}
		return true
	})
	for _, want := range []string{"RegisterEntity", "RegisterFormats", "RegisterSync"} {
		found := false
		for _, call := range called {
			if strings.HasSuffix(call, "."+want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not called; calls: %v\n%s", want, called, generated)
		}
	}
}
