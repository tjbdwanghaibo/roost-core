package roost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strings"
	"testing"
)

// U-0218 · C4 · 发版验证发现:renderFrameworkCollaborators 无条件 import 服务包,collaborators 正文只用
// servicemetrics 时(U-0217 删掉 match 的 Grouping() 之后就是这样)生成的文件 "imported and not used",
// 整个工程编译不过;codegen 自己的测试不编译生成物,所以 v1.15.5 带着它发出去了。承诺:每个托管服务的
// collaborators 文件里,每一个 import 都被正文里的选择表达式引用——注释里出现的 `match.Grouping` 不算引用。
func TestFrameworkCollaboratorsImportOnlyWhatTheyUse(t *testing.T) {
	for framework := range frameworkCatalog {
		m := DefaultManifest("planet", "example.com/planet", []string{framework}, nil, nil)
		m.Services[framework] = ServiceSpec{Framework: framework}
		src := renderFrameworkCollaborators(m, framework)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, framework+"/collaborators.go", src, 0)
		if err != nil {
			t.Fatalf("%s collaborators do not parse: %v\n%s", framework, err, src)
		}
		used := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					used[ident.Name] = true
				}
			}
			return true
		})
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			name := path.Base(importPath)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if !used[name] {
				t.Errorf("%s collaborators import %q but never use it:\n%s", framework, importPath, src)
			}
		}
	}
}
