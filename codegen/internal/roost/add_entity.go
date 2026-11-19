package roost

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
	"strconv"
	"strings"
)

const coreEntityImport = "github.com/tjbdwanghaibo/roost-core/framework/entity"

// addEntityComponent 是 roost add component <X> --entity <E>：按 compose 附录 A2 的组件包写法（2026-11-16 起，与模块模板
// 和 game-demo 一致）一次做完组件的三处：
//
//	game/components/<包>/<包>.go      组件包：类型号常量与工厂登记、Component、宿主接口 Owner、NewComponent（业务文件）
//	game/view/component_<包>.go        组件对外的接口 <X> 与 <X>Holder（view 里已有 <X>Holder 时不写）
//	game/entities/<实体>/entity.go     挂字段 `<x> *<包>.Component` 与 comp:"<包>.CompType<X>"
//
// 实体包里不写组件文件、类型别名与 I<X>Entity。组件包已存在（挂到第二个实体）时只挂字段：组件的 Owner 若是别的实体的
// view.<实体>Entity，提示改成最小接口；宿主方法写在 view 的 <X>Holder 里，实体漏写时 roost generate 写的编译期断言报错。
func addEntityComponent(root string, manifest Manifest, options AddOptions) ([]string, error) {
	entityName, entityPath, err := resolveEntitySource(root, options.Entity)
	if err != nil {
		return nil, err
	}
	componentType, fieldName := toPascal(options.Name), toSnake(options.Name)
	pkg := componentPackageName(options.Name)
	if !token.IsIdentifier(pkg) || token.IsKeyword(pkg) {
		return nil, fmt.Errorf("component %q: its package name %q is not a usable Go package name; choose another name", options.Name, pkg)
	}
	// 旧写法（组件在实体包里）留下的同名组件：新挂一个组件包会和它重名。
	if legacy := filepath.Join(filepath.Dir(entityPath), fieldName+"_component.go"); regularFileExists(legacy) {
		return nil, fmt.Errorf("%s already exists (a component written in the entity package); move it to game/components/%s or choose another name", relativeSlash(root, legacy), pkg)
	}
	pkgDir := filepath.Join(root, "game", "components", pkg)
	componentPath := filepath.Join(pkgDir, pkg+".go")
	existingPackage := false
	if info, statErr := os.Stat(pkgDir); statErr == nil && info.IsDir() {
		declared, err := packageDeclaresConst(pkgDir, "CompType"+componentType)
		if err != nil {
			return nil, err
		}
		existingPackage = declared
		if !declared && regularFileExists(componentPath) {
			return nil, fmt.Errorf("%s exists but does not declare CompType%s; choose another component name", relativeSlash(root, componentPath), componentType)
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}

	raw, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	importPath := manifest.Project.Module + "/game/components/" + pkg
	updated, err := wireComponentPackageField(raw, entityPath, toPascal(entityName), fieldName, componentType, importPath)
	if err != nil {
		return nil, err
	}
	var changes []syncChange
	var written []string
	if !existingPackage {
		body, err := format.Source([]byte(renderComponentPackage(manifest.Project.Module, pkg, fieldName, componentType, toPascal(entityName), options.ID)))
		if err != nil {
			return nil, fmt.Errorf("format component scaffold: %w", err)
		}
		changes = append(changes, syncChange{rel: relativeSlash(root, componentPath), path: componentPath, body: body})
		written = append(written, relativeSlash(root, componentPath))
	} else if other, err := componentOwnerEntity(pkgDir); err != nil {
		return nil, err
	} else if other != "" && other != toPascal(entityName) && options.Warnings != nil {
		fmt.Fprintf(options.Warnings, "game/components/%s: Owner is view.%sEntity, which %s does not implement; change Owner to the smallest interface the component needs "+
			"(entity.IThreadSafeEntity + view.%sHolder, with what it needs from the entity as host methods in the Holder), otherwise creating %s fails\n",
			pkg, other, toPascal(entityName), componentType, toPascal(entityName))
	}
	changes = append(changes, syncChange{rel: relativeSlash(root, entityPath), path: entityPath, before: raw, body: updated, existed: true})
	written = append(written, relativeSlash(root, entityPath))
	// view 侧：组件对外的接口 <X> 与 <X>Holder（访问器与宿主方法）。别的实体已经挂过这个组件、或模块带来了它时不动。
	holder, err := viewDeclaresHolder(root, componentType)
	if err != nil {
		return nil, err
	}
	if !holder {
		viewPath := filepath.Join(root, "game", "view", "component_"+pkg+".go")
		if regularFileExists(viewPath) {
			return nil, fmt.Errorf("%s exists but does not declare %sHolder; add the Holder or choose another component name", relativeSlash(root, viewPath), componentType)
		}
		viewBody, err := format.Source([]byte(renderViewComponent(componentType, pkg)))
		if err != nil {
			return nil, fmt.Errorf("format component view scaffold: %w", err)
		}
		changes = append(changes, syncChange{rel: relativeSlash(root, viewPath), path: viewPath, body: viewBody})
		written = append(written, relativeSlash(root, viewPath))
	}
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	return written, nil
}

// componentPackageName 是组件包名：蛇形名去掉下划线（DailyQuest → dailyquest），与模块的组件包同一写法。
func componentPackageName(name string) string { return strings.ReplaceAll(toSnake(name), "_", "") }

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// componentOwnerRe 认组件包里的 `type Owner = view.<E>Entity`。
var componentOwnerRe = regexp.MustCompile(`(?m)^type Owner = view\.(\w+)Entity\b`)

// componentOwnerEntity 返回组件包的 Owner 写成 view.<E>Entity 时的 E；Owner 是别的写法（最小接口）时返回空。
func componentOwnerEntity(dir string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if match := componentOwnerRe.FindSubmatch(raw); match != nil {
			return string(match[1]), nil
		}
	}
	return "", nil
}

func addEntityDAO(root string, manifest Manifest, options AddOptions) ([]string, error) {
	entityName, entityPath, err := resolveEntitySource(root, options.Entity)
	if err != nil {
		return nil, err
	}
	daoSnake, daoType := toSnake(options.Name), toPascal(options.Name)+"Dao"
	daoPath := filepath.Join(root, "db", "def", daoSnake+".go")
	raw, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(daoPath); err == nil {
		// -template game 缺省已带 Player 的主 DAO：同一个 DAO 已接在这个实体上时什么都不做（照旧的步骤能重跑），
		// 文件在而没接在这个实体上时仍然报错。
		if entityHoldsDAO(raw, entityPath, toPascal(entityName), daoSnake, daoType) {
			if source, locateErr := locateDaoStruct(root, daoType); locateErr == nil && source != nil {
				fmt.Fprintf(options.Warnings, "%s already exists and %s already holds %s; nothing to do\n", relativeSlash(root, daoPath), toPascal(entityName), daoType)
				return nil, nil
			}
		}
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, daoPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	updated, err := wireDAOField(raw, entityPath, toPascal(entityName), manifest.Project.Module+"/db", daoSnake, daoType)
	if err != nil {
		return nil, err
	}
	// 本地实体的 DAO 按服分库（dbscope=sid）；Remote 托管实体的 DAO 各服共用（remote=managed 要求 dbscope=global）。
	daoBody, err := format.Source([]byte(renderDAODefinition(daoSnake, daoType, !remoteManagedEntitySource(raw))))
	if err != nil {
		return nil, fmt.Errorf("format DAO scaffold: %w", err)
	}
	if err := writeRelatedBusinessFiles(entityPath, raw, updated, daoPath, daoBody); err != nil {
		return nil, err
	}
	return []string{relativeSlash(root, daoPath), relativeSlash(root, entityPath)}, nil
}

func resolveEntitySource(root, requested string) (string, string, error) {
	base := filepath.Join(root, "game", "entities")
	requested = toSnake(requested)
	if requested != "" {
		if !validName(requested) {
			return "", "", fmt.Errorf("invalid Entity name %q", requested)
		}
		path := filepath.Join(base, requested, "entity.go")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return "", "", fmt.Errorf("entity %q does not exist; create it with: roost add entity %s", requested, requested)
			}
			return "", "", err
		}
		return requested, path, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", "", fmt.Errorf("read Entity directory: %w", err)
	}
	type candidate struct{ name, path string }
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(base, entry.Name(), "entity.go")
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			candidates = append(candidates, candidate{name: entry.Name(), path: path})
		}
	}
	if len(candidates) == 1 {
		return candidates[0].name, candidates[0].path, nil
	}
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no Entity exists; run: roost add entity <name>")
	}
	return "", "", fmt.Errorf("multiple Entities exist; select one with --entity <name>")
}

func wireComponentField(raw []byte, path, entityType, fieldName, componentType string) ([]byte, string, error) {
	file, fset, entityAlias, structure, err := parseEntityAggregate(raw, path, entityType)
	if err != nil {
		return nil, "", err
	}
	if structHasNamedField(structure, fieldName) || structHasTagValue(structure, "comp", "CompType"+componentType) {
		return nil, "", fmt.Errorf("entity %s already contains component %s", entityType, componentType)
	}
	ensureEmbeddedManager(structure, entityAlias, "ComponentManager")
	appendStructField(structure, &ast.Field{
		Names: []*ast.Ident{ast.NewIdent(fieldName)},
		Type:  &ast.StarExpr{X: ast.NewIdent(componentType + "Component")},
		Tag:   &ast.BasicLit{Kind: token.STRING, Value: "`comp:\"CompType" + componentType + "\"`"},
	})
	formatted, err := formatAST(fset, file)
	return formatted, file.Name.Name, err
}

// wireComponentPackageField 按组件包的写法把组件挂到实体上（模块清单的 component_package）：字段
// `<fieldName> *<包名>.Component`、标签 `comp:"<包名>.CompType<component>"`，并补上组件包的 import（别名取包目录名）。
// 类型号常量与工厂登记在组件包里，实体包里不再写组件文件。
func wireComponentPackageField(raw []byte, path, entityType, fieldName, component, importPath string) ([]byte, error) {
	file, fset, entityAlias, structure, err := parseEntityAggregate(raw, path, entityType)
	if err != nil {
		return nil, err
	}
	alias := importPath[strings.LastIndex(importPath, "/")+1:]
	for _, spec := range file.Imports {
		existing := strings.Trim(spec.Path.Value, `"`)
		name := existing[strings.LastIndex(existing, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == alias && existing != importPath {
			return nil, fmt.Errorf("entity %s: cannot import component package %s as %s, the alias already names %s", entityType, importPath, alias, existing)
		}
		if existing == importPath && name != alias {
			return nil, fmt.Errorf("entity %s imports %s as %s; the component package must keep its own name %s", entityType, importPath, name, alias)
		}
	}
	compType := alias + ".CompType" + component
	if structHasNamedField(structure, fieldName) || structHasTagValue(structure, "comp", compType) || structHasTagValue(structure, "comp", "CompType"+component) {
		return nil, fmt.Errorf("entity %s already contains component %s", entityType, component)
	}
	if _, err := ensureImport(file, importPath, alias); err != nil {
		return nil, err
	}
	ensureEmbeddedManager(structure, entityAlias, "ComponentManager")
	appendStructField(structure, &ast.Field{
		Names: []*ast.Ident{ast.NewIdent(fieldName)},
		Type:  &ast.StarExpr{X: &ast.SelectorExpr{X: ast.NewIdent(alias), Sel: ast.NewIdent("Component")}},
		Tag:   &ast.BasicLit{Kind: token.STRING, Value: "`comp:\"" + compType + "\"`"},
	})
	return formatAST(fset, file)
}

func wireDAOField(raw []byte, path, entityType, importPath, collection, daoType string) ([]byte, error) {
	fieldName := collection
	if strings.EqualFold(strings.TrimSuffix(daoType, "Dao"), entityType) {
		fieldName = "dao"
	}
	return wireNamedDAOField(raw, path, entityType, importPath, fieldName, collection, daoType)
}

// entityHoldsDAO 报告实体结构体是否已有 `dao:"<collection>"`、类型为 *<db>.<daoType> 的字段。
func entityHoldsDAO(raw []byte, path, entityType, collection, daoType string) bool {
	_, _, _, structure, err := parseEntityAggregate(raw, path, entityType)
	if err != nil || !structHasTagValue(structure, "dao", collection) {
		return false
	}
	for _, field := range structure.Fields.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if selector, ok := star.X.(*ast.SelectorExpr); ok && selector.Sel.Name == daoType {
			return true
		}
	}
	return false
}

// wireNamedDAOField 同时用于 add dao 与模块附加 DAO；只追加声明，不另建运行时注册路径。
func wireNamedDAOField(raw []byte, path, entityType, importPath, fieldName, collection, daoType string) ([]byte, error) {
	file, fset, entityAlias, structure, err := parseEntityAggregate(raw, path, entityType)
	if err != nil {
		return nil, err
	}
	if structHasTagValue(structure, "dao", collection) {
		return nil, fmt.Errorf("entity %s already contains DAO collection %s", entityType, collection)
	}
	ensureEmbeddedManager(structure, entityAlias, "DaoManager")
	dbAlias, err := ensureImport(file, importPath, "db")
	if err != nil {
		return nil, err
	}
	if structHasNamedField(structure, fieldName) {
		return nil, fmt.Errorf("entity %s already contains field %s", entityType, fieldName)
	}
	appendStructField(structure, &ast.Field{
		Names: []*ast.Ident{ast.NewIdent(fieldName)},
		Type:  &ast.StarExpr{X: &ast.SelectorExpr{X: ast.NewIdent(dbAlias), Sel: ast.NewIdent(daoType)}},
		Tag:   &ast.BasicLit{Kind: token.STRING, Value: "`dao:\"" + collection + "\"`"},
	})
	return formatAST(fset, file)
}

func parseEntityAggregate(raw []byte, path, entityType string) (*ast.File, *token.FileSet, string, *ast.StructType, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("parse %s: %w", path, err)
	}
	entityAlias := ""
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != coreEntityImport {
			continue
		}
		entityAlias = "entity"
		if spec.Name != nil {
			entityAlias = spec.Name.Name
		}
	}
	if entityAlias == "" || entityAlias == "." || entityAlias == "_" {
		return nil, nil, "", nil, fmt.Errorf("entity %s must import %s with a normal package alias", entityType, coreEntityImport)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != entityType {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return nil, nil, "", nil, fmt.Errorf("entity %s is not a struct", entityType)
			}
			return file, fset, entityAlias, structure, nil
		}
	}
	return nil, nil, "", nil, fmt.Errorf("entity struct %s not found in %s", entityType, path)
}

// appendStructField 把字段追加到结构体末尾，并把新字段的位置放在右花括号上。
// 新节点不带位置时，go/printer 会把结构体后面紧跟的注释（例如下一个方法的说明）打印进结构体里。
func appendStructField(structure *ast.StructType, field *ast.Field) {
	placeNodeAt(field, structure.Fields.Closing)
	structure.Fields.List = append(structure.Fields.List, field)
}

// placeNodeAt 把节点里所有标识符和字面量的位置设为 pos。
func placeNodeAt(node ast.Node, pos token.Pos) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			x.NamePos = pos
		case *ast.BasicLit:
			x.ValuePos = pos
		case *ast.StarExpr:
			x.Star = pos
		}
		return true
	})
}

func ensureEmbeddedManager(structure *ast.StructType, alias, name string) {
	for _, field := range structure.Fields.List {
		if len(field.Names) != 0 {
			continue
		}
		selector, ok := field.Type.(*ast.SelectorExpr)
		if ok {
			pkg, pkgOK := selector.X.(*ast.Ident)
			if pkgOK && pkg.Name == alias && selector.Sel.Name == name {
				return
			}
		}
	}
	structure.Fields.List = append([]*ast.Field{{Type: &ast.SelectorExpr{X: ast.NewIdent(alias), Sel: ast.NewIdent(name)}}}, structure.Fields.List...)
}

func structHasNamedField(structure *ast.StructType, name string) bool {
	for _, field := range structure.Fields.List {
		for _, candidate := range field.Names {
			if candidate.Name == name {
				return true
			}
		}
	}
	return false
}

func structHasTagValue(structure *ast.StructType, key, expected string) bool {
	for _, field := range structure.Fields.List {
		if field.Tag == nil {
			continue
		}
		raw, err := strconv.Unquote(field.Tag.Value)
		if err == nil && reflectStructTag(raw, key) == expected {
			return true
		}
	}
	return false
}

// reflectStructTag keeps add.go independent of unsafe string surgery while
// avoiding reflection on synthetic AST nodes.
func reflectStructTag(raw, key string) string {
	needle := key + ":\""
	start := strings.Index(raw, needle)
	if start < 0 {
		return ""
	}
	value := raw[start+len(needle):]
	if end := strings.IndexByte(value, '"'); end >= 0 {
		return value[:end]
	}
	return ""
}

func ensureImport(file *ast.File, importPath, preferredAlias string) (string, error) {
	used := make(map[string]string)
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		alias := filepath.Base(path)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if path == importPath {
			if alias == "." || alias == "_" {
				return "", fmt.Errorf("import %s must use a normal package alias", importPath)
			}
			return alias, nil
		}
		used[alias] = path
	}
	alias := preferredAlias
	if _, exists := used[alias]; exists {
		alias = "projectdb"
		if _, exists := used[alias]; exists {
			return "", fmt.Errorf("cannot add %s: aliases %s and %s are already used", importPath, preferredAlias, alias)
		}
	}
	spec := &ast.ImportSpec{Name: ast.NewIdent(alias), Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(importPath)}}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.IMPORT {
			// 新 import 放在原有最后一条之后、括号之内：位置落在原来的 import 范围里，go/printer 才不会把紧跟在 import
			// 后面的注释（例如实体标记上方的说明与 //roost:entity）挪进 import 块。单行 import 补上括号。
			last := gen.Specs[len(gen.Specs)-1].End()
			if !gen.Lparen.IsValid() {
				gen.Lparen, gen.Rparen = gen.Specs[0].Pos(), last
			}
			placeNodeAt(spec, last)
			spec.EndPos = last
			gen.Specs = append(gen.Specs, spec)
			file.Imports = append(file.Imports, spec)
			return alias, nil
		}
	}
	decl := &ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}
	file.Decls = append([]ast.Decl{decl}, file.Decls...)
	file.Imports = append(file.Imports, spec)
	return alias, nil
}

func formatAST(fset *token.FileSet, file *ast.File) ([]byte, error) {
	var output bytes.Buffer
	if err := format.Node(&output, fset, file); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// renderComponentPackage 写 roost add component 的组件包 game/components/<包>/<包>.go（compose 附录 A2）。
// Owner 缺省是整个宿主实体（view.<E>Entity：DAO getter 与它挂的各组件访问器，roost generate 生成），与只挂在一个实体上的
// 模块组件（checkin、profile）同一写法。
func renderComponentPackage(module, pkg, name, componentType, entityType string, id int64) string {
	return fmt.Sprintf(`package %[1]s

import (
	"fmt"

	"%[7]s/game/view"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=%[2]d
const CompType%[3]s entity.ComponentType = %[2]d

// init 登记组件工厂：实体结构体里 comp:"%[1]s.CompType%[3]s" 的字段经它创建组件。
func init() {
	entity.RegisterComponentFactory(CompType%[3]s, NewComponent)
}

// Owner 是 %[3]s 组件的宿主，只在实体被 Nest 锁住期间使用。缺省是整个 %[4]s（view.%[4]sEntity：实体的 DAO getter 与它
// 挂的各组件的访问器，roost generate 生成）。要把组件挂到别的实体上时改成组件用得到的最小接口，例如
//
//	type Owner interface {
//		entity.IThreadSafeEntity
//		view.%[3]sHolder // 访问器 %[3]s() + 组件要实体提供的宿主方法（例如 %[3]sState() *db.%[3]sState）
//	}
//
// 宿主方法写在 game/view/component_%[1]s.go 的 %[3]sHolder 里；只交出实体 DAO 某个字段的由 roost generate 写，其余在实体包
// game/entities/<实体>/%[1]s.go 手写，漏写时生成的编译期断言报错。
type Owner = view.%[4]sEntity

// Component 是 %[3]s 的业务逻辑。它不保存事务会改的状态：状态放在实体的 DAO 里，经生成的 setter 修改（不落盘的写成
// nopersist 字段），所以回滚只有 DAO 一处、脏标记由 Nest 收集；不要在这里登记 undo。
type Component struct {
	entity.ComponentBase
	owner Owner
}

// NewComponent 是组件工厂；上面的 init 登记。
func NewComponent(value any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
	owner, ok := value.(Owner)
	if !ok {
		return nil, fmt.Errorf("%[5]s component: owner %%T does not implement %[1]s.Owner", value)
	}
	return &Component{owner: owner}, nil
}

func (component *Component) Name() string { return %[6]q }

// Owner 只在实体存活且被 Nest 锁住期间可用。
func (component *Component) Owner() Owner { return component.owner }

// 下面写业务方法（实体上的业务规则都写在组件里，不写成实体方法）。handler 与别的组件要调用的方法同时写进
// view.%[3]s（game/view/component_%[1]s.go），它们经访问器调用：handler 收 view.I%[3]sEntity，写 target.%[3]s().<方法>()；
// 跨组件写 component.owner.<别的组件>().<方法>()。做完一件别的功能要跟着动的事时，在 game/event/ 定义事件并
// component.owner.Publish(event.<事件>{...})，错误原样返回；监听方在自己的组件上写 //roost:listen 方法。
// 不要在组件里加锁：Nest 锁住所属实体之后才进入业务 handler。
`, pkg, id, componentType, entityType, name, name, module)
}

// renderViewComponent 写 roost add component 的 view 侧 game/view/component_<包>.go：组件对外的接口 <X> 与 <X>Holder。
// 访问器 <X>() 与窄接口 view.I<X>Entity 由 roost generate（entity 生成器）写。
func renderViewComponent(componentType, pkg string) string {
	return fmt.Sprintf(`package view

// %[1]s 是 %[1]s 组件对 game/ 下其它包公开的方法（实现在 game/components/%[2]s 的 *Component）；只在宿主被 Nest 锁住期间
// 调用。handler 或别的组件要调用的方法都列在这里；签名只用内建类型、db、协议与别的 view 类型（结果结构体以 %[1]s 作前缀
// 写在本文件，组件包里留同名别名）。
type %[1]s interface{}

// %[1]sHolder 是“带有 %[1]s 组件”的小接口：roost generate 在挂了这个组件的每个实体上实现访问器 %[1]s()，并写出
// handler 的参数类型 I%[1]sEntity（holder_entities_gen.go）。组件要实体提供的东西（宿主方法）也写在这里由实体实现；
// 只交出实体 DAO 某个字段的（例如 %[1]sState() *db.%[1]sState）由生成器写。
type %[1]sHolder interface{ %[1]s() %[1]s }
`, componentType, pkg)
}

// defaultDAODatabase 是 DAO 骨架 db= 的逻辑库名（D9 修订版 A）：代码里写固定的逻辑名，物理库名交给部署配置
// dataengine.databases（生成配置写 main: game，Mongo 里再加 mongo.database_prefix，即 <工程名>_game）。
const defaultDAODatabase = "main"

// remoteManagedEntitySource 报告实体源码的 //roost:entity 标记是不是 remote=managed。
func remoteManagedEntitySource(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//roost:entity ") && strings.Contains(" "+line+" ", " remote=managed ") {
			return true
		}
	}
	return false
}

// renderDAODefinition 写 DAO 定义骨架。db= 是逻辑库名（物理库名在配置 dataengine.databases 里），文档 _id 是
// 完整 EntityID（roost id entity 换算），两点都写进骨架注释，免得运维按逻辑名或玩家 ID 去 Mongo 里找。
//
// serverScoped 为 true 时写 dbscope=sid（维护者 2026-11-13：“local 就是 dbscope=sid”）：挂在本地 Nest 实体上的 DAO
// （add dao --entity、模块自带的本地实体、-template game-demo 的 Player / World）按服分库，物理库 <库>_<sid>，
// 每个服一个；起服预加载（preload=startup）与按 ID 冷加载都只看本服库。dbscope=global 只留给 Remote 托管等各服
// 共用的实体（remote=managed 要求它）。不挂实体的独立 DAO（add dao 不带 --entity）不知道给谁用，不写，取缺省 global。
func renderDAODefinition(collection, typeName string, serverScoped bool) string {
	scope, note := "", ""
	if serverScoped {
		scope = " dbscope=sid"
		note = "// dbscope=sid: one database per server, <physical>_<sid>; this server's\n// entities are the documents in its own database. Use dbscope=global only for\n// an entity every server shares (remote=managed requires it).\n"
	}
	return fmt.Sprintf("package dbdef\n\n// %s is stored in collection %q of the logical database %q (db=, the\n// constant db.%sDBName). The Mongo database it lands in is deployment\n// configuration: dataengine.databases maps %s to a physical database (the\n// generated config maps it to one named after the project).\n%s// Each document's _id is the full EntityID, not the bare player id: convert\n// with `roost id entity --unique <player-id> --entity <Entity>` and back with\n// `roost id entity <_id>`.\n//\n//roost:dao coll=%s db=%s%s\ntype %s struct {\n\t// Add domain fields here. Example:\n\t// Name string `store:\"name\" dao:\"persist,sync\"`\n\t// store:\"...\" is the required stable field key; missing or duplicate keys fail generation.\n\t// Write it on every generated field: renaming the Go field then keeps the\n\t// stored data, as long as the store key stays.\n\t// Never declare ID/id or tracker: codegen owns identity and dirty tracking.\n}\n", typeName, collection, defaultDAODatabase, typeName, defaultDAODatabase, note, collection, defaultDAODatabase, scope, typeName)
}

// daoDatabaseName 是按工程名派生的 Mongo 库名前缀（生成配置的 {project_db}：mongo.database_prefix，维护者 2026-11-19）：
// 工程名（roost.yaml project.name，已满足 ^[a-z][a-z0-9_]*$）。两条修正：
//   - 与 Mongo 系统库同名（admin / local / config）时加后缀 "_data"（前缀不能是系统库名）；
//   - 截到 40 字节（mods.MaxMongoDatabasePrefix）并去掉末尾的 "_"：Mongo 库名上限 63 字节，要放下
//     <前缀>_<库名>_<sid>。
func daoDatabaseName(project string) string {
	name := strings.ToLower(strings.TrimSpace(project))
	switch name {
	case "admin", "local", "config":
		name += "_data"
	}
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "_")
	}
	return name
}

func writeRelatedBusinessFiles(entityPath string, entityBefore, entityBody []byte, newPath string, newBody []byte) error {
	return commitSyncChanges([]syncChange{
		{rel: filepath.ToSlash(newPath), path: newPath, body: newBody},
		{rel: filepath.ToSlash(entityPath), path: entityPath, before: entityBefore, body: entityBody, existed: true},
	})
}

func relativeSlash(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}
