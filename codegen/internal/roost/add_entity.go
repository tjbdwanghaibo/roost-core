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

const coreEntityImport = "github.com/tjbdwanghaibo/roost-core/entity"

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
	if err := writeRelatedBusinessFiles(entityPath, raw, updated, componentPath, body); err != nil {
		return nil, err
	}
	_ = manifest // reserved for future component policy validation
	return []string{relativeSlash(root, componentPath), relativeSlash(root, entityPath)}, nil
}

func addEntityDAO(root string, manifest Manifest, options AddOptions) ([]string, error) {
	entityName, entityPath, err := resolveEntitySource(root, options.Entity)
	if err != nil {
		return nil, err
	}
	daoSnake, daoType := toSnake(options.Name), toPascal(options.Name)+"Dao"
	daoPath := filepath.Join(root, "db", "def", daoSnake+".go")
	if _, err := os.Stat(daoPath); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, daoPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	raw, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	updated, err := wireDAOField(raw, entityPath, toPascal(entityName), manifest.Project.Module+"/db", daoSnake, daoType)
	if err != nil {
		return nil, err
	}
	daoBody, err := format.Source([]byte(renderDAODefinition(daoSnake, daoType)))
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

func wireDAOField(raw []byte, path, entityType, importPath, collection, daoType string) ([]byte, error) {
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
	fieldName := collection
	if strings.EqualFold(strings.TrimSuffix(daoType, "Dao"), entityType) {
		fieldName = "dao"
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
	return fmt.Sprintf(`package %s

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

//roost:component type=%d
const CompType%s entity.ComponentType = %d

// %sComponent owns %s gameplay logic. Persistent or replicated state should
// be changed through generated DAO methods so dirty tracking remains correct.
type %sComponent struct {
	entity.ComponentBase
	owner *%s
}

// I%sEntity is the narrow lock-safe view for Nest handlers using this
// component. The generated Entity wire implements the getter.
type I%sEntity interface {
	entity.IThreadSafeEntity
	%sComp() *%sComponent
}

func init() {
	entity.RegisterComponentFactory(CompType%s, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*%s)
		if !ok {
			return nil, fmt.Errorf("%s component: owner %%T is not *%s", owner)
		}
		return &%sComponent{owner: typed}, nil
	})
}

func (component *%sComponent) Name() string { return %q }

// Owner is available only while the Entity is alive and locked by Nest.
func (component *%sComponent) Owner() *%s { return component.owner }

// Add business methods below. Do not add mutexes here: Nest enters business
// handlers only after locking the owning Entity.
`, pkg, id, componentType, id, componentType, entityType, componentType, entityType, componentType, componentType, componentType, componentType,
		componentType, entityType, name, entityType, componentType, componentType, name,
		componentType, entityType)
}

func renderDAODefinition(collection, typeName string) string {
	return fmt.Sprintf("package dbdef\n\n//roost:dao coll=%s db=game\ntype %s struct {\n\t// Add domain fields here. Example:\n\t// Name string `bson:\"name\" dao:\"persist,sync\"`\n\t// Never declare ID/id or tracker: codegen owns identity and dirty tracking.\n}\n", collection, typeName)
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
