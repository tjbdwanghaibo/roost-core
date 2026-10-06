package configschema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// “声明与读取一致”的源码守卫（维护者决定 A4 ①）：读配置只经声明（LoadConfig），声明了的字段必须被读。
// 框架（app、kit）的守卫测试与 codegen 对生成工程的守卫用同一份实现，所以放在这个叶子包里（只依赖标准库）。
//
//   - 读了没声明（UndeclaredReads）：直接调 *viper.Viper 的读方法（参数、字段、变量、Registry.Config() 的返回值），
//     或 app.ConfigBool 一类单键读取。读到的键不在任何声明里，App 启动检查、生成器与 doctor 都看不到它。
//   - 声明了没读（UnreadFields）：带 config tag 的结构体字段，在同一个包里没有被读（没有任何选择子 .Field）。
//     声明了不读的键会被生成器写进配置、被检查当成合法，运维改了它却什么也不发生。
//
// 两者都是按名字的语法检查，不做类型推导：误报时改名或改写法，不加豁免。

// viperReadMethods 是 *viper.Viper 读配置的方法。写入（Set、SetDefault）与读文件（ReadInConfig）不算读键。
var viperReadMethods = map[string]bool{
	"Get": true, "GetBool": true, "GetDuration": true, "GetFloat64": true, "GetInt": true, "GetInt32": true,
	"GetInt64": true, "GetIntSlice": true, "GetSizeInBytes": true, "GetString": true, "GetStringMap": true,
	"GetStringMapString": true, "GetStringMapStringSlice": true, "GetStringSlice": true, "GetTime": true,
	"GetUint": true, "GetUint8": true, "GetUint16": true, "GetUint32": true, "GetUint64": true,
	"IsSet": true, "AllKeys": true, "AllSettings": true, "InConfig": true, "Sub": true,
	"Unmarshal": true, "UnmarshalKey": true, "UnmarshalExact": true,
}

// singleKeyReads 是 app 的单键读取：读到的键不在任何声明里。
var singleKeyReads = map[string]bool{"ConfigBool": true, "ConfigDuration": true, "ConfigInt": true, "ConfigInt64": true}

// GoPackages 返回 roots 下全部非测试 Go 源文件，按目录（包）分组；testdata、vendor 与以点开头的目录
// （.git、生成器的暂存目录）跳过。
func GoPackages(roots ...string) (map[string][]string, error) {
	packages := map[string][]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && path != root && (entry.Name() == "testdata" || entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			dir := filepath.Dir(path)
			packages[dir] = append(packages[dir], path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return packages, nil
}

// UndeclaredReads 返回 packages 里直接读配置的调用位置（file:line:col），排序。skip 为 true 的文件不查
// （app 里声明读取的 viper 适配器与单键读取本身）；skip 可以为 nil。
func UndeclaredReads(packages map[string][]string, skip func(path string) bool) ([]string, error) {
	var out []string
	for _, files := range packages {
		fset := token.NewFileSet()
		parsed, err := parseGoFiles(fset, files)
		if err != nil {
			return nil, err
		}
		viperFields := map[string]bool{} // 同一个包里类型是 *viper.Viper 的参数与字段的名字
		for _, file := range parsed {
			ast.Inspect(file, func(node ast.Node) bool {
				if field, ok := node.(*ast.Field); ok && isViperType(field.Type) {
					for _, name := range field.Names {
						viperFields[name.Name] = true
					}
				}
				return true
			})
		}
		for path, file := range parsed {
			if skip != nil && skip(path) {
				continue
			}
			viperNames := map[string]bool{}
			for name := range viperFields {
				viperNames[name] = true
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.ValueSpec:
					if isViperType(n.Type) {
						for _, name := range n.Names {
							viperNames[name.Name] = true
						}
					}
				case *ast.AssignStmt:
					for i, rhs := range n.Rhs {
						if call, ok := rhs.(*ast.CallExpr); ok && i < len(n.Lhs) && returnsViper(call) {
							if ident, ok := n.Lhs[i].(*ast.Ident); ok {
								viperNames[ident.Name] = true
							}
						}
					}
				}
				return true
			})
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				var bad bool
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch {
					case viperReadMethods[fun.Sel.Name] && isViperValue(fun.X, viperNames, viperFields):
						bad = true
					case singleKeyReads[fun.Sel.Name] || fun.Sel.Name == "NewConfigReader":
						if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "app" {
							bad = true
						}
					}
				case *ast.Ident:
					// app 包自己里的单键读取（不带包名）。
					bad = singleKeyReads[fun.Name] && file.Name.Name == "app"
				}
				if bad {
					out = append(out, fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out, nil
}

// UnreadFields 返回 packages 里声明了却没有被读的配置字段（file:line:col: Field (config:"key")），排序。
func UnreadFields(packages map[string][]string) ([]string, error) {
	var out []string
	for _, files := range packages {
		fset := token.NewFileSet()
		parsed, err := parseGoFiles(fset, files)
		if err != nil {
			return nil, err
		}
		selected := map[string]bool{}
		type declared struct {
			name, key string
			pos       token.Pos
		}
		var fields []declared
		for _, file := range parsed {
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					selected[n.Sel.Name] = true
				case *ast.StructType:
					for _, field := range n.Fields.List {
						if field.Tag == nil || len(field.Names) == 0 {
							continue
						}
						tag, _ := strconv.Unquote(field.Tag.Value)
						key, ok := reflect.StructTag(tag).Lookup("config")
						if !ok {
							continue
						}
						for _, name := range field.Names {
							fields = append(fields, declared{name.Name, key, name.Pos()})
						}
					}
				}
				return true
			})
		}
		for _, field := range fields {
			if !selected[field.name] {
				out = append(out, fset.Position(field.pos).String()+": "+field.name+" (config:\""+field.key+"\")")
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseGoFiles(fset *token.FileSet, files []string) (map[string]*ast.File, error) {
	parsed := make(map[string]*ast.File, len(files))
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		parsed[path] = file
	}
	return parsed, nil
}

func isViperType(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Viper"
}

func returnsViper(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "viper" && sel.Sel.Name == "New" {
		return true
	}
	return sel.Sel.Name == "Config" && len(call.Args) == 0
}

func isViperValue(expr ast.Expr, names, fields map[string]bool) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		return names[x.Name]
	case *ast.SelectorExpr:
		return fields[x.Sel.Name]
	case *ast.CallExpr:
		return returnsViper(x)
	case *ast.ParenExpr:
		return isViperValue(x.X, names, fields)
	}
	return false
}
