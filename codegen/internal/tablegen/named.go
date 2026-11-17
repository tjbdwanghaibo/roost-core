package tablegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
)

// 具名类型（维护者 2026-11-17，交接 C5 (a) 选项 1）：
//
//   - schema 包里声明、底层是内建标量的具名类型（type Rank int32、type Tier Rank、别名 type Power = int64）
//     按底层类型处理：CSV 转 JSON、index 的可用性与键、客户端导出（C# 生成底层类型，不生成 enum）都看
//     底层类型（Field.Resolved）。
//   - 服务端生成的 Go 照常使用具名类型本身：生成代码在另一个包，schema 包里声明的类型名写成
//     <别名>.<类型名>（qualifiedType）。
//   - 别的包里的具名类型（time.Duration、foo.Kind）生成器只读 schema 包自己的声明，不知道底层类型；
//     schema 包里底层不是内建标量的具名类型（结构体等）没有标量的 JSON 形状。两者在 index 与客户端导出
//     时仍报错，报错由 NamedTypeReason 说明原因。

// builtinScalars 是内建标量类型名到规范名：byte、rune 是 uint8、int32 的别名。
var builtinScalars = map[string]string{
	"string": "string", "bool": "bool",
	"int": "int", "int8": "int8", "int16": "int16", "int32": "int32", "int64": "int64",
	"uint": "uint", "uint8": "uint8", "uint16": "uint16", "uint32": "uint32", "uint64": "uint64",
	"float32": "float32", "float64": "float64",
	"byte": "uint8", "rune": "int32",
}

// packageTypes 读 dir（一个 Go 包）里全部非测试 .go 文件的类型声明，返回类型名 → 底层内建标量的规范名；
// 底层不是内建标量的（结构体、切片、泛型类型等）值为 ""。具名类型的具名类型与别名沿声明链找到底层。
func packageTypes(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	decls := make(map[string]ast.Expr)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if typeSpec.TypeParams != nil {
					decls[typeSpec.Name.Name] = nil // 泛型类型没有固定的底层标量
					continue
				}
				decls[typeSpec.Name.Name] = typeSpec.Type
			}
		}
	}
	out := make(map[string]string, len(decls))
	for name := range decls {
		out[name] = underlyingScalar(decls, name)
	}
	return out, nil
}

// underlyingScalar 沿 name 的声明链找底层内建标量；声明链里出现非标识符（结构体等）、循环或别的包的类型时为 ""。
// 同包里的声明优先于内建名（type int32 string 这种遮蔽按声明算）。
func underlyingScalar(decls map[string]ast.Expr, name string) string {
	for range len(decls) + 1 {
		ident, ok := decls[name].(*ast.Ident)
		if !ok {
			return ""
		}
		if _, local := decls[ident.Name]; local {
			name = ident.Name
			continue
		}
		return builtinScalars[ident.Name]
	}
	return ""
}

// rewriteType 把类型表达式重新写成文本，其中每个不带包名的标识符交给 ident 改写；指针、切片、数组与 map
// 逐层展开，其余写法（结构体字面量、函数类型等）原样输出。
func rewriteType(expr ast.Expr, ident func(name string) string) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return ident(e.Name)
	case *ast.StarExpr:
		return "*" + rewriteType(e.X, ident)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + rewriteType(e.Elt, ident)
		}
		return "[" + types.ExprString(e.Len) + "]" + rewriteType(e.Elt, ident)
	case *ast.MapType:
		return "map[" + rewriteType(e.Key, ident) + "]" + rewriteType(e.Value, ident)
	case *ast.ParenExpr:
		return "(" + rewriteType(e.X, ident) + ")"
	}
	return types.ExprString(expr)
}

// resolveType 把 schema 包里底层是内建标量的具名类型换成底层类型（Field.Resolved）。
func resolveType(expr ast.Expr, named map[string]string) string {
	return rewriteType(expr, func(name string) string {
		if scalar := named[name]; scalar != "" {
			return scalar
		}
		return name
	})
}

// qualifiedType 是生成的服务端 Go（另一个包）里写字段类型的方式：schema 包里声明的类型名加上
// schema 包的导入别名（Rank → meta0.Rank，[]Rank → []meta0.Rank），内建类型与带包名的类型不变。
func qualifiedType(meta Meta, typeExpr string) string {
	return qualifyType(meta, typeExpr, meta.Alias)
}

// qualifyType 给 typeExpr 里 schema 包声明的类型名加上 qualifier 前缀；qualifier 为空时原样返回。
// 生成代码用导入别名；比较两张表的类型（ref）用导入路径，因为同一个包的每张表各有一个别名。
func qualifyType(meta Meta, typeExpr, qualifier string) string {
	if len(meta.named) == 0 || qualifier == "" {
		return typeExpr
	}
	expr, err := parser.ParseExpr(typeExpr)
	if err != nil {
		return typeExpr
	}
	return rewriteType(expr, func(name string) string {
		if _, local := meta.named[name]; local {
			return qualifier + "." + name
		}
		return name
	})
}

// NamedTypeReason 说明一个（已按 Field.Resolved 展开的）类型表达式里为什么还有生成器处理不了的具名类型；
// 没有具名类型时返回 ""。schema 包里底层是内建标量的具名类型此时已经换成底层类型，所以剩下的不带包名的
// 具名类型一定是底层不是内建标量的（结构体等），带包名的是别的包里的类型。index 与客户端导出共用这段说明。
func NamedTypeReason(goType string) string {
	expr, err := parser.ParseExpr(strings.TrimSpace(goType))
	if err != nil {
		return ""
	}
	name, foreign := firstNamedType(expr)
	switch {
	case name == "":
		return ""
	case foreign:
		return fmt.Sprintf("%s is defined in another package; the generator reads only the schema package's own declarations, so it cannot see the underlying type (declare a named type over a built-in scalar in the schema package)", name)
	}
	return fmt.Sprintf("%s is a named type whose underlying type is not a built-in scalar (for example a struct); only named types over string, bool, integers and floats declared in the schema package are handled as their underlying type", name)
}

// firstNamedType 找类型表达式里（沿指针、切片、数组元素、map 的键与值）第一个具名类型：foreign 表示它带包名。
func firstNamedType(expr ast.Expr) (name string, foreign bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		if _, builtin := builtinScalars[e.Name]; builtin || predeclared[e.Name] {
			return "", false
		}
		return e.Name, false
	case *ast.SelectorExpr:
		return types.ExprString(e), true
	case *ast.StarExpr:
		return firstNamedType(e.X)
	case *ast.ArrayType:
		return firstNamedType(e.Elt)
	case *ast.ParenExpr:
		return firstNamedType(e.X)
	case *ast.MapType:
		if name, foreign := firstNamedType(e.Key); name != "" {
			return name, foreign
		}
		return firstNamedType(e.Value)
	}
	return "", false
}

// predeclared 是 Go 预声明、但不是本生成器支持的标量的类型名：它们不是“具名类型”，NamedTypeReason 不说明。
var predeclared = map[string]bool{"any": true, "error": true, "complex64": true, "complex128": true, "uintptr": true, "comparable": true}
