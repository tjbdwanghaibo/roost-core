package entity

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/genutil"
)

// game/view（v1.47.0）只放接口，用来打断 game/ 下各包之间的循环引用：
//
//   - component_<功能>.go（模块带来，手写）定义组件对外的接口 <X> 与“带有这个组件”的小接口 <X>Holder。
//     <X>Holder 里方法名等于 X、返回 view 类型的那个方法是访问器（例如 Profile() Profile，生成器在实体上实现）；
//     其余方法是“宿主方法”：组件要求实体提供的东西（例如 BagState() *db.BagState）。
//   - entity_<实体>.go（本生成器写）定义实体接口 <E>Entity：嵌入 entity.IThreadSafeEntity、实体的 DAO getter
//     与它挂载的各组件的 <X>Holder。
//   - holder_entities_gen.go（本生成器写）给每个 <X>Holder 一个窄接口 I<X>Entity{ IThreadSafeEntity; <X>Holder }，
//     给只用这一个组件的 Nest handler 当参数类型；任何挂了这个组件的实体都满足它。
//
// 实体包里每个挂在实体上、在 view 里有 <X>Holder 的组件，生成器在 <实体>_gen_wire.go 里给出同名访问器
// （例如 func (e *Player) Profile() view.Profile），返回 view 接口；<X>Comp() 返回具体类型，只给生成的接线与实体包内部用。
// 宿主方法里的纯 DAO 交接也由生成器写（规则见 view_host.go），其余在实体包手写；接线末尾的编译期断言
// var _ view.<E>Entity = (*E)(nil) 保证实体漏写宿主方法时编译失败。
// 组件包、钩子、联动与 game/modules/login 只依赖 view，实体包依赖组件包并实现 view 的接口，所以没有反向引用。
const (
	viewDirRel        = "game/view"
	viewEntityPrefix  = "entity_"
	viewHolderSuffix  = "Holder"
	entityPackagesRel = "game/entities/"
)

// ViewAccessor 是生成在实体上的一个 view 访问器：func (e *E) Name() view.Type { return e.Field }。
type ViewAccessor struct {
	Name, Type, Field string
}

// viewTarget 是一个实体包对应的 game/view：Dir 为空表示这个包不在 game/entities/ 下，不生成 view。
type viewTarget struct {
	Dir, Import string
	// Root / Module：生成工程的根目录与 module 路径（DAO 交接按它找 db/def，事件扫描按它找组件包）。
	Root, Module string
	// holders：<X>Holder 名 → 声明。
	holders map[string]viewHolder
	// declared：view 包手写文件里声明的类型名 → 文件（生成 I<X>Entity 前查重名）。
	declared map[string]string
}

// viewHolder 是 view 里的一个 <X>Holder。
type viewHolder struct {
	Name, Component string // BagHolder、Bag
	// Accessor / AccessorType：访问器方法名（等于 Component）与它返回的 view 类型名。
	Accessor, AccessorType string
	Hosts                  []viewHostMethod
}

// viewHostMethod 是 <X>Holder 里访问器以外的一个方法。
type viewHostMethod struct {
	Name string
	// Signature 是方法在 view 里的写法（例如 "BagState() *db.BagState"），写进接线的注释。
	Signature string
	// Result 是无参、单返回值时返回类型的规范形式（见 viewTypeKey）；有参数或返回值不是一个时为空，不能做 DAO 交接。
	Result string
	// Struct：返回类型是 DAO 包里的结构体指针（只有这种才允许“按类型找唯一字段”）。
	Struct bool
}

// moduleRootFor 从 dir 向上找 go.mod，返回工程根目录与 module 路径；找不到时 root 为空。
func moduleRootFor(dir string) (root, module string, err error) {
	for root = dir; ; {
		raw, readErr := os.ReadFile(filepath.Join(root, "go.mod"))
		if readErr == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "module ") {
					return root, strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`), nil
				}
			}
			return root, "", nil
		}
		if !os.IsNotExist(readErr) {
			return "", "", readErr
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", "", nil
		}
		root = parent
	}
}

// viewTargetFor 从实体包目录向上找 go.mod；包在 <模块>/game/entities/ 下时返回 <模块>/game/view。
func viewTargetFor(dir string) (viewTarget, error) {
	root, module, err := moduleRootFor(dir)
	if err != nil || root == "" || module == "" {
		return viewTarget{}, err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || !strings.HasPrefix(filepath.ToSlash(rel)+"/", entityPackagesRel) {
		return viewTarget{}, nil
	}
	target := viewTarget{Dir: filepath.Join(root, filepath.FromSlash(viewDirRel)), Import: module + "/" + viewDirRel, Root: root, Module: module}
	target.holders, target.declared, err = scanViewHolders(target.Dir, target.Import)
	return target, err
}

// scanViewHolders 读出 view 包里的 <X>Holder 接口：访问器 X() <view 类型> 必须有，其余方法都是宿主方法。
// 同时返回手写文件里声明的类型名（生成的 holder_entities_gen.go 不算）。
func scanViewHolders(dir, viewImport string) (map[string]viewHolder, map[string]string, error) {
	holders := map[string]viewHolder{}
	declared := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || filepath.Base(path) == holderEntitiesFile {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		imports := map[string]string{}
		for alias, imp := range collectImports(file) {
			imports[alias] = imp.Path
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				declared[ts.Name.Name] = filepath.Base(path)
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok || !strings.HasSuffix(ts.Name.Name, viewHolderSuffix) || ts.Name.Name == viewHolderSuffix {
					continue
				}
				holder, err := parseViewHolder(fset, path, ts.Name.Name, iface, imports, viewImport)
				if err != nil {
					return nil, nil, err
				}
				holders[holder.Name] = holder
			}
		}
	}
	return holders, declared, nil
}

// parseViewHolder 解析一个 <X>Holder：方法名等于 X、无参、返回一个 view 里声明的类型的方法是访问器，
// 其余（导出的）方法是宿主方法。嵌入别的接口不支持：嵌入的方法是谁的、由谁实现说不清。
func parseViewHolder(fset *token.FileSet, path, name string, iface *ast.InterfaceType, imports map[string]string, viewImport string) (viewHolder, error) {
	holder := viewHolder{Name: name, Component: strings.TrimSuffix(name, viewHolderSuffix)}
	for _, method := range iface.Methods.List {
		fn, ok := method.Type.(*ast.FuncType)
		if !ok || len(method.Names) != 1 {
			return viewHolder{}, fmt.Errorf("%s: %s embeds %s; a Holder lists its accessor %s() and the host methods itself", path, name, exprString(fset, method.Type), holder.Component)
		}
		methodName := method.Names[0].Name
		params := fn.Params != nil && len(fn.Params.List) > 0
		results := 0
		if fn.Results != nil {
			for _, field := range fn.Results.List {
				results += max(1, len(field.Names))
			}
		}
		if methodName == holder.Component {
			result, isIdent := ast.Expr(nil), false
			if results == 1 {
				result = fn.Results.List[0].Type
				_, isIdent = result.(*ast.Ident)
			}
			if params || !isIdent || isPredeclared(result.(*ast.Ident).Name) {
				return viewHolder{}, fmt.Errorf("%s: %s.%s is the component accessor and must take no parameters and return one type declared in game/view", path, name, methodName)
			}
			holder.Accessor, holder.AccessorType = methodName, result.(*ast.Ident).Name
			continue
		}
		if !ast.IsExported(methodName) {
			return viewHolder{}, fmt.Errorf("%s: host method %s.%s must be exported (the entity implements it in another package)", path, name, methodName)
		}
		host := viewHostMethod{Name: methodName, Signature: methodName + strings.TrimPrefix(exprString(fset, fn), "func")}
		if !params && results == 1 {
			host.Result, host.Struct = viewTypeKey(fn.Results.List[0].Type, imports, viewImport)
		}
		holder.Hosts = append(holder.Hosts, host)
	}
	if holder.Accessor == "" {
		return viewHolder{}, fmt.Errorf("%s: %s must declare the component accessor %s() <view type>", path, name, holder.Component)
	}
	return holder, nil
}

// viewTypeKey 把一个类型写成与包别名无关的形式：预声明类型原样（int64），其余是 [*]<导入路径>.<名字>。
// 第二个返回值表示它是另一个包里的命名类型的指针（DAO 交接里结构体字段 getter 的返回形式）。
// 不认识的形式（切片、map、函数……）返回空串。
func viewTypeKey(expr ast.Expr, imports map[string]string, localImport string) (string, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		if isPredeclared(t.Name) {
			return t.Name, false
		}
		return localImport + "." + t.Name, false
	case *ast.SelectorExpr:
		alias, ok := t.X.(*ast.Ident)
		if !ok || imports[alias.Name] == "" {
			return "", false
		}
		return imports[alias.Name] + "." + t.Sel.Name, false
	case *ast.StarExpr:
		inner, _ := viewTypeKey(t.X, imports, localImport)
		if inner == "" || isPredeclared(inner) {
			return "", false
		}
		return "*" + inner, true
	}
	return "", false
}

// isPredeclared 报告 name 是不是 Go 的预声明基础类型。
func isPredeclared(name string) bool {
	switch name {
	case "bool", "string", "byte", "rune", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128", "error", "any":
		return true
	}
	return false
}

func exprString(fset *token.FileSet, node any) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, node); err != nil {
		return fmt.Sprintf("%T", node)
	}
	return b.String()
}

// applyViewAccessors 给实体挂上 view 访问器：组件 X（CompTypeX）在 view 里有 XHolder 时生成。冷组件不生成
// （字段可能为 nil，装进接口后不等于 nil）。再按嵌入的 Holder 解出宿主方法：能做纯 DAO 交接的写进 HostMethods，
// 其余进 ManualHosts（手写）。返回实体接口要嵌入的 Holder 名（排序后）。
func applyViewAccessors(ent *EntityDef, target viewTarget) ([]string, error) {
	ent.ViewAccessors, ent.ViewImport, ent.HostMethods, ent.ManualHosts = nil, "", nil, nil
	if target.Dir == "" {
		return nil, nil
	}
	var embedded []string
	for _, comp := range ent.Components {
		name := componentNameFromCompType(comp.CompType)
		holder, ok := target.holders[name+viewHolderSuffix]
		if name == "" || !ok || comp.Cold {
			continue
		}
		if comp.FieldName == holder.Accessor {
			return nil, fmt.Errorf("entity %s: field %s has the same name as the view accessor %s() of %s; rename the field (for example %s)",
				ent.Name, comp.FieldName, holder.Accessor, holder.Name, strings.ToLower(comp.FieldName[:1])+comp.FieldName[1:])
		}
		embedded = append(embedded, holder.Name)
		if !hasMethod(*ent, holder.Accessor) {
			ent.ViewAccessors = append(ent.ViewAccessors, ViewAccessor{Name: holder.Accessor, Type: "view." + holder.AccessorType, Field: comp.FieldName})
		}
	}
	// 共用组件在别的实体上的组件号名字与 view 的 Holder 不同名（例如 Alliance 上的 AllianceActivity 是 activity 组件）：
	// 接线文件手写返回 view 类型的访问器，这里按方法签名嵌入对应的 Holder。
	for holderName, holder := range target.holders {
		if ent.ViewMethods[holder.Accessor] == holder.AccessorType && !slices.Contains(embedded, holderName) {
			embedded = append(embedded, holderName)
		}
	}
	if len(ent.ViewAccessors) > 0 {
		ent.ViewImport = target.Import
	}
	sort.Strings(embedded)
	if err := resolveHostMethods(ent, target, embedded); err != nil {
		return nil, err
	}
	return embedded, nil
}

// viewDirOfFullScan：扫描目录是 <模块>/game 或 <模块>/game/entities（看得到全部实体）时返回 <模块>/game/view，否则空串。
func viewDirOfFullScan(scanDir string) string {
	game := scanDir
	if filepath.Base(game) == "entities" {
		game = filepath.Dir(game)
	}
	if filepath.Base(game) != "game" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(game), "go.mod")); err != nil {
		return ""
	}
	return filepath.Join(game, "view")
}

// entityTypeRe 找 //roost:entity 标记后面紧跟（隔一行注释以内）的结构体名。
var entityTypeRe = regexp.MustCompile(`(?m)^//roost:entity[^\n]*\n(?://[^\n]*\n)?type (\w+) struct`)

// liveViewEntityFiles 扫 <模块>/game/entities/*/ 下手写文件里的 //roost:entity，返回仍存在的实体对应的 entity_<snake>.go。
// 只读标记，不解析整个包：每次生成都能便宜地确认哪些实体还在（不再要求整个 game/entities 一起扫描）。
func liveViewEntityFiles(viewDir string) (map[string]bool, error) {
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(viewDir), "entities", "*", "*.go"))
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || isGeneratedWireFile(filepath.Base(path)) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, match := range entityTypeRe.FindAllSubmatch(raw, -1) {
			live[viewEntityFile(string(match[1]))] = true
		}
	}
	return live, nil
}

// viewEntityFile 是实体接口文件名：game/view/entity_<snake>.go。
func viewEntityFile(entityName string) string { return viewEntityPrefix + toSnake(entityName) + ".go" }

// generateViewEntity 写出 game/view/entity_<snake>.go：<E>Entity 嵌入 IThreadSafeEntity、DAO getter 与 holders。
func generateViewEntity(ent EntityDef, target viewTarget, holders []string, force bool) (string, bool, error) {
	var b bytes.Buffer
	b.WriteString(entityGeneratedHeader + "\n")
	b.WriteString("package view\n\nimport (\n")
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n")
	imported := map[string]bool{}
	var daos []string
	for _, dao := range ent.Daos {
		if dao.GetterName == "" || dao.Cold {
			continue
		}
		daos = append(daos, fmt.Sprintf("\t%s() %s\n", dao.GetterName, dao.TypeName))
		alias, _, qualified := strings.Cut(strings.TrimPrefix(dao.TypeName, "*"), ".")
		if !qualified || imported[alias] {
			continue
		}
		for _, imp := range ent.Imports {
			if imp.Alias == alias {
				imported[alias] = true
				fmt.Fprintf(&b, "\t%s %q\n", alias, imp.Path)
			}
		}
		if !imported[alias] {
			// DAO 在实体包本地声明：view 引用不到，实体接口不带它。
			daos = daos[:len(daos)-1]
		}
	}
	b.WriteString(")\n\n")
	fmt.Fprintf(&b, "// %sEntity 是 %s 实体在 game/ 下各包之间的接口（生成，按实体挂载的组件嵌入各组件的 Holder）。\n", ent.Name, ent.Name)
	fmt.Fprintf(&b, "// 组件、钩子与联动只收用得到的最小接口（例如 BagHolder）；要整个实体时才收它。只在实体被 Nest 锁住期间使用。\n")
	fmt.Fprintf(&b, "type %sEntity interface {\n\tentity.IThreadSafeEntity\n", ent.Name)
	for _, line := range daos {
		b.WriteString(line)
	}
	for _, holder := range holders {
		fmt.Fprintf(&b, "\t%s\n", holder)
	}
	b.WriteString("}\n")
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return "", false, fmt.Errorf("format %s: %w", viewEntityFile(ent.Name), err)
	}
	if err := os.MkdirAll(target.Dir, 0o755); err != nil {
		return "", false, err
	}
	path := filepath.Join(target.Dir, viewEntityFile(ent.Name))
	changed, err := genutil.WriteIfChanged(path, formatted, force)
	return path, changed, err
}

// retireViewEntities 删掉 view 目录里不再对应任何实体的生成文件 entity_*.go（只删带生成头的）。
func retireViewEntities(viewDir string, keep map[string]bool, stdout interface{ Write([]byte) (int, error) }) error {
	paths, err := filepath.Glob(filepath.Join(viewDir, viewEntityPrefix+"*.go"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if keep[filepath.Base(path)] {
			continue
		}
		removed, err := retireEntityFile(path)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(stdout, "removed: %s\n", path)
		}
	}
	return nil
}
