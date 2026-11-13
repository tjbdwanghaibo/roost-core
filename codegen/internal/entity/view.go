package entity

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
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
//   - component_<功能>.go（模块带来，手写）定义组件对外的接口 <X> 与“带有这个组件”的小接口 <X>Holder，
//     后者只有一个访问器方法，例如 ProfileHolder{ Profile() Profile }。
//   - entity_<实体>.go（本生成器写）定义实体接口 <E>Entity：嵌入 entity.IThreadSafeEntity、实体的 DAO getter
//     与它挂载的各组件的 <X>Holder。
//
// 实体包里每个挂在实体上、在 view 里有 <X>Holder 的组件，生成器在 <实体>_gen_wire.go 里给出同名访问器
// （例如 func (e *Player) Profile() view.Profile），返回 view 接口；原来的 <X>Comp() 照旧返回具体类型给 handler 用。
// 组件包、钩子、联动与 game/login 只依赖 view，实体包依赖组件包并实现 view 的接口，所以没有反向引用。
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
	// holders：<X>Holder → 访问器方法名与返回的 view 类型名。
	holders map[string][2]string
}

// viewTargetFor 从实体包目录向上找 go.mod；包在 <模块>/game/entities/ 下时返回 <模块>/game/view。
func viewTargetFor(dir string) (viewTarget, error) {
	for root := dir; ; {
		raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err == nil {
			module := ""
			for _, line := range strings.Split(string(raw), "\n") {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "module ") {
					module = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
					break
				}
			}
			rel, err := filepath.Rel(root, dir)
			if err != nil || module == "" || !strings.HasPrefix(filepath.ToSlash(rel)+"/", entityPackagesRel) {
				return viewTarget{}, nil
			}
			target := viewTarget{Dir: filepath.Join(root, filepath.FromSlash(viewDirRel)), Import: module + "/" + viewDirRel}
			target.holders, err = scanViewHolders(target.Dir)
			return target, err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return viewTarget{}, nil
		}
		root = parent
	}
}

// scanViewHolders 读出 view 包里的 <X>Holder 接口：恰好一个无参方法、返回 view 里的一个类型名。
func scanViewHolders(dir string) (map[string][2]string, error) {
	holders := map[string][2]string{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok || !strings.HasSuffix(ts.Name.Name, viewHolderSuffix) || len(iface.Methods.List) != 1 {
					continue
				}
				method := iface.Methods.List[0]
				fn, ok := method.Type.(*ast.FuncType)
				if !ok || len(method.Names) != 1 || (fn.Params != nil && len(fn.Params.List) > 0) || fn.Results == nil || len(fn.Results.List) != 1 {
					return nil, fmt.Errorf("%s: %s must have exactly one method without parameters that returns one view type", path, ts.Name.Name)
				}
				result, ok := fn.Results.List[0].Type.(*ast.Ident)
				if !ok {
					return nil, fmt.Errorf("%s: %s.%s must return a type declared in game/view", path, ts.Name.Name, method.Names[0].Name)
				}
				holders[ts.Name.Name] = [2]string{method.Names[0].Name, result.Name}
			}
		}
	}
	return holders, nil
}

// applyViewAccessors 给实体挂上 view 访问器：组件 X（CompTypeX）在 view 里有 XHolder 时生成。冷组件不生成
// （字段可能为 nil，装进接口后不等于 nil）。返回实体接口要嵌入的 Holder 名（排序后）。
func applyViewAccessors(ent *EntityDef, target viewTarget) []string {
	ent.ViewAccessors, ent.ViewImport = nil, ""
	if target.Dir == "" {
		return nil
	}
	var embedded []string
	for _, comp := range ent.Components {
		name := componentNameFromCompType(comp.CompType)
		holder, ok := target.holders[name+viewHolderSuffix]
		if name == "" || !ok || comp.Cold {
			continue
		}
		embedded = append(embedded, name+viewHolderSuffix)
		if !hasMethod(*ent, holder[0]) {
			ent.ViewAccessors = append(ent.ViewAccessors, ViewAccessor{Name: holder[0], Type: "view." + holder[1], Field: comp.FieldName})
		}
	}
	// 共用组件在别的实体上的组件号名字与 view 的 Holder 不同名（例如 Alliance 上的 AllianceActivity 是 activity 组件）：
	// 接线文件手写返回 view 类型的访问器，这里按方法签名嵌入对应的 Holder。
	for holderName, holder := range target.holders {
		if ent.ViewMethods[holder[0]] == holder[1] && !slices.Contains(embedded, holderName) {
			embedded = append(embedded, holderName)
		}
	}
	if len(ent.ViewAccessors) > 0 {
		ent.ViewImport = target.Import
	}
	sort.Strings(embedded)
	return embedded
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
