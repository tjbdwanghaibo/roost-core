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
	"strconv"
	"strings"
)

const coreEntityImport = "github.com/tjbdwanghaibo/roost-core/framework/entity"

// addEntityComponent creates a component in its owning Entity package and
// updates the Entity aggregate in one operation. A component generated in an
// unrelated package is not useful until its factory and field are wired, so
// the high-level command deliberately owns that repetitive integration.
func addEntityComponent(root string, manifest Manifest, options AddOptions) ([]string, error) {
	entityName, entityPath, err := resolveEntitySource(root, options.Entity)
	if err != nil {
		return nil, err
	}
	componentSnake, componentType := toSnake(options.Name), toPascal(options.Name)
	componentPath := filepath.Join(filepath.Dir(entityPath), componentSnake+"_component.go")
	if _, err := os.Stat(componentPath); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, componentPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	raw, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	updated, pkg, err := wireComponentField(raw, entityPath, toPascal(entityName), componentSnake, componentType)
	if err != nil {
		return nil, err
	}
	body, err := format.Source([]byte(renderEntityComponent(pkg, toPascal(entityName), componentSnake, componentType, options.ID)))
	if err != nil {
		return nil, fmt.Errorf("format component scaffold: %w", err)
	}
	changes := []syncChange{
		{rel: filepath.ToSlash(componentPath), path: componentPath, body: body},
		{rel: filepath.ToSlash(entityPath), path: entityPath, before: raw, body: updated, existed: true},
	}
	written := []string{relativeSlash(root, componentPath), relativeSlash(root, entityPath)}
	// view 侧：组件对外的接口 <X> 与 <X>Holder（访问器与宿主方法）。别的实体已经加过同名组件、或模块带来了这个文件时不动它。
	viewPath := filepath.Join(root, "game", "view", "component_"+componentSnake+".go")
	if _, statErr := os.Stat(viewPath); os.IsNotExist(statErr) {
		viewBody, err := format.Source([]byte(renderViewComponent(componentType, toSnake(entityName), componentSnake)))
		if err != nil {
			return nil, fmt.Errorf("format component view scaffold: %w", err)
		}
		changes = append(changes, syncChange{rel: filepath.ToSlash(viewPath), path: viewPath, body: viewBody})
		written = append(written, relativeSlash(root, viewPath))
	} else if statErr != nil {
		return nil, statErr
	}
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	_ = manifest // reserved for future component policy validation
	return written, nil
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
	structure.Fields.List = append(structure.Fields.List, &ast.Field{
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
	structure.Fields.List = append(structure.Fields.List, &ast.Field{
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
	structure.Fields.List = append(structure.Fields.List, &ast.Field{
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
			gen.Lparen = token.Pos(1)
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

func renderEntityComponent(pkg, entityType, name, componentType string, id int64) string {
	return fmt.Sprintf(`package %[1]s

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

//roost:component type=%[2]d
const CompType%[3]s entity.ComponentType = %[2]d

// %[3]sComponent owns %[4]s gameplay logic. It keeps no state a transaction
// changes: such state lives in the DAO and moves through the generated
// mutators — a nopersist field when it must not be stored — so the DAO's
// rollback is the only rollback and dirty tracking stays correct. Do not
// register undo here.
//
// Other packages reach it through game/view: view.%[3]s (component_%[5]s.go) lists
// the methods they may call, and Nest handlers take the generated narrow
// interface view.I%[3]sEntity and call target.%[3]s().<Method>().
type %[3]sComponent struct {
	entity.ComponentBase
	owner *%[4]s
}

func init() {
	entity.RegisterComponentFactory(CompType%[3]s, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*%[4]s)
		if !ok {
			return nil, fmt.Errorf("%[5]s component: owner %%T is not *%[4]s", owner)
		}
		return &%[3]sComponent{owner: typed}, nil
	})
}

func (component *%[3]sComponent) Name() string { return %[6]q }

// Owner is available only while the Entity is alive and locked by Nest.
func (component *%[3]sComponent) Owner() *%[4]s { return component.owner }

// Add business methods below. Add the ones handlers call to view.%[3]s too.
// Do not add mutexes here: Nest enters business handlers only after locking
// the owning Entity.
`, pkg, id, componentType, entityType, name, name)
}

// renderViewComponent 写 roost add component 的 view 侧：组件对外的接口 <X> 与 <X>Holder。访问器 <X>() 与
// 窄接口 view.I<X>Entity 由 roost generate（entity 生成器）写。
func renderViewComponent(componentType, entitySnake, componentSnake string) string {
	return fmt.Sprintf(`package view

// %[1]s is what other packages may call on the %[1]s component (implemented by
// %[1]sComponent in game/entities/%[2]s/%[3]s_component.go). Add each method a
// handler or another component calls; they run only while Nest holds the
// entity's lock.
type %[1]s interface{}

// %[1]sHolder is "has a %[1]s component". roost generate implements the
// accessor %[1]s() on every entity that mounts the component and writes the
// narrow interface I%[1]sEntity (holder_entities_gen.go) that Nest handlers
// take. Methods the component needs from its entity (host methods) go here too
// and the entity implements them; one that only hands over a field of the
// entity's DAO (for example State() *db.%[1]sState) is generated.
type %[1]sHolder interface{ %[1]s() %[1]s }
`, componentType, entitySnake, componentSnake)
}

// defaultDAODatabase 是 DAO 骨架 db= 的逻辑库名（D9 修订版 A）：代码里写固定的逻辑名，物理库名交给部署配置
// dataengine.databases（生成配置写 main: <daoDatabaseName(工程名)>，新工程开箱的物理库名与工程名派生的规则一致）。
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

// daoDatabaseName 是按工程名派生的 Mongo 物理库名（生成配置的 {project_db}：dataengine.database 与
// dataengine.databases 里 main 的映射）：工程名（roost.yaml project.name，已满足 ^[a-z][a-z0-9_]*$，
// 本身就是合法的 Mongo 库名）。两条修正：
//   - Mongo 保留库 admin / local / config 加后缀 "_data"，业务集合不能写进系统库；
//   - 截到 52 字节并去掉末尾的 "_"：Mongo 库名上限 63 字节，dbscope=sid 还要追加 "_<sid>"（最长 11 字节）。
func daoDatabaseName(project string) string {
	name := strings.ToLower(strings.TrimSpace(project))
	switch name {
	case "admin", "local", "config":
		name += "_data"
	}
	if len(name) > 52 {
		name = strings.TrimRight(name[:52], "_")
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
