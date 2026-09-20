package entity

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Marker format: //roost:entity entityKind=EntityKindPlayer
var markerRe = marker.Regexp("entity", `\s+(.+)`)

// EntityDef holds parsed entity definition.
type EntityDef struct {
	Name            string // struct name, e.g. "Player"
	EntityKind      string // concrete entity kind constant, e.g. "EntityKindPlayer"
	Category        string // optional category constant expression from the marker
	RemotePolicy    string // entity.RemotePolicy constant expression
	Lifetime        string // entity.EntityLifetime constant expression
	NoPersist       bool   // EntityBase AutoPersist returns false
	Sync            bool   // enable entity base sync by default
	SyncTopic       string // optional sync topic for generated builder
	SyncPacker      string // optional entity.EntitySyncBuilderParam.PackerFactory expression
	SubjectPacker   string // optional observer-free SubjectSyncPacker factory expression
	Components      []ComponentField
	Daos            []DaoField
	SourceFile      string
	Imports         []ImportDef
	ExistingMethods map[string]bool
	RemoteBase      bool // embeds entity.RemoteEntityBase
}

// ImportDef is an imported package referenced by generated code.
type ImportDef struct {
	Alias    string
	Path     string
	Explicit bool
}

// ComponentField is a component field in the entity struct.
type ComponentField struct {
	FieldName  string // Go field name, e.g. "bag"
	TypeName   string // concrete type name, e.g. "BagComponent"
	TypePkg    string // package path if external, empty if same package
	CompType   string // ComponentType constant, e.g. "CompTypeBag"
	Cold       bool   // skip startup wiring; business code initializes on demand
	GetterName string
	GetterType string
}

// DaoField is a DAO field in the entity struct.
type DaoField struct {
	FieldName  string // Go field name, e.g. "dao"
	TypeName   string // concrete type name, e.g. "PlayerDao"
	TypePkg    string // package path if external
	CollName   string // collection name, e.g. "players"
	Cold       bool   // skip startup creation unless explicitly supplied
	GetterName string
}

// parseDir scans all .go files in dir for entity markers.
func parseDir(dir string) ([]EntityDef, string, error) {
	fset := token.NewFileSet()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}

	var entities []EntityDef
	var pkg string
	type parsedFile struct {
		path    string
		content []byte
		ast     *ast.File
		imports map[string]ImportDef
	}
	var files []parsedFile
	methods := make(map[string]map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// Skip generated files
		if isGeneratedWireFile(entry.Name()) {
			continue
		}
		// Skip test files
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, "", err
		}

		f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
		if err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", entry.Name(), err)
		}

		if pkg == "" {
			pkg = f.Name.Name
		}

		for recv, names := range collectMethods(f) {
			if methods[recv] == nil {
				methods[recv] = make(map[string]bool)
			}
			for name := range names {
				methods[recv][name] = true
			}
		}
		files = append(files, parsedFile{
			path:    filePath,
			content: content,
			ast:     f,
			imports: collectImports(f),
		})
	}

	for _, file := range files {
		// Find marker comments and their associated structs
		ents, err := extractEntities(fset, file.ast, file.content, file.path, file.imports, methods)
		if err != nil {
			return nil, "", err
		}
		entities = append(entities, ents...)
	}

	return entities, pkg, nil
}

// extractEntities finds //roost:entity markers and extracts struct info.
func extractEntities(fset *token.FileSet, f *ast.File, content []byte, filePath string, importMap map[string]ImportDef, methods map[string]map[string]bool) ([]EntityDef, error) {
	var entities []EntityDef

	// Build line→comment map from marker comments
	type markerInfo struct {
		line     int
		params   map[string]string
		attached bool
	}
	var markers []*markerInfo

	for _, cg := range f.Comments {
		for _, c := range cg.List {
			matches := markerRe.FindStringSubmatch(c.Text)
			if matches == nil {
				continue
			}
			line := fset.Position(c.Pos()).Line
			params, err := parseMarkerParams(matches[1])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: //roost:entity %w", filePath, line, err)
			}
			markers = append(markers, &markerInfo{line: line, params: params})
		}
	}

	if len(markers) == 0 {
		return nil, nil
	}

	// Find struct declarations
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}

			structLine := fset.Position(typeSpec.Pos()).Line

			// Check if there's a marker on the line before this struct
			for _, m := range markers {
				if m.line == structLine-1 || m.line == structLine-2 {
					m.attached = true
					ent := EntityDef{
						Name:            typeSpec.Name.Name,
						EntityKind:      m.params["entityKind"],
						Category:        strings.TrimSpace(m.params["category"]),
						SourceFile:      filePath,
						ExistingMethods: methods[typeSpec.Name.Name],
					}
					if ent.EntityKind == "" {
						ent.EntityKind = "EntityKind" + ent.Name
					}
					if err := validateMarkerValues(m.params); err != nil {
						return nil, fmt.Errorf("%s:%d: //roost:entity %w", filePath, m.line, err)
					}
					ent.RemotePolicy = parseRemoteParam(m.params["remote"])
					ent.NoPersist = parseBoolParam(m.params["noPersist"])
					ent.Lifetime = parseLifetimeParam(m.params["lifetime"], ent.NoPersist, ent.RemotePolicy)
					ent.Sync = parseBoolParam(m.params["sync"])
					ent.SyncTopic = m.params["syncTopic"]
					ent.SyncPacker = m.params["syncPacker"]
					ent.SubjectPacker = m.params["subjectPacker"]
					// Core takes exactly one packer factory
					// (EntitySyncBuilderParam.PackerFactory). `subjectPacker`
					// is the current spelling and `syncPacker` still works
					// for projects written before the two collapsed into one;
					// setting both would mean two values for one field, so it
					// is refused here rather than resolved by a coin flip
					// (RR-20260918-01).
					if ent.SyncPacker != "" && ent.SubjectPacker != "" {
						return nil, fmt.Errorf("%s:%d: //roost:entity sets both syncPacker=%s and subjectPacker=%s; they name the same factory (entity.EntitySyncBuilderParam.PackerFactory) — keep subjectPacker", filePath, m.line, ent.SyncPacker, ent.SubjectPacker)
					}
					if ent.SyncPacker != "" && !ent.Sync {
						return nil, fmt.Errorf("%s:%d: //roost:entity has syncPacker=%s but sync is not true", filePath, m.line, ent.SyncPacker)
					}
					if ent.SubjectPacker != "" && !ent.Sync {
						return nil, fmt.Errorf("%s:%d: //roost:entity has subjectPacker=%s but sync is not true", filePath, m.line, ent.SubjectPacker)
					}

					ent.Components, ent.Daos = extractFields(structType, content, fset)
					ent.RemoteBase = hasRemoteEntityBase(structType)
					deriveAccessors(&ent)
					ent.Imports = collectEntityImports(ent, importMap)
					entities = append(entities, ent)
					break
				}
			}
		}
	}

	// A marker nothing claimed used to disappear without a trace: no wire
	// file, no error. Usually a doc comment pushed the struct more than two
	// lines away, or the marker sits above an interface or alias.
	for _, m := range markers {
		if !m.attached {
			return nil, fmt.Errorf("%s:%d: //roost:entity marker is not followed by a struct declaration (the `type X struct` line must be within two lines of the marker)", filePath, m.line)
		}
	}

	return entities, nil
}

// markerKeys is every parameter the entity marker understands. `id` is
// written by `roost add entity` and consumed by the registry generator.
var markerKeys = []string{"id", "entityKind", "category", "remote", "noPersist", "lifetime", "sync", "syncTopic", "syncPacker", "subjectPacker"}

// parseMarkerParams parses key=value pairs and refuses anything else: a typo
// in a key (`remot=managed`) or a bare flag (`noPersist`) used to be read as
// "parameter absent", which silently changes what gets generated.
func parseMarkerParams(s string) (map[string]string, error) {
	params := make(map[string]string)
	for _, p := range strings.Fields(s) {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return nil, fmt.Errorf("parameter %q must be key=value (known keys: %s)", p, strings.Join(markerKeys, ", "))
		}
		if !slices.Contains(markerKeys, kv[0]) {
			return nil, fmt.Errorf("unknown parameter %q (known keys: %s)", kv[0], strings.Join(markerKeys, ", "))
		}
		if _, dup := params[kv[0]]; dup {
			return nil, fmt.Errorf("parameter %q given twice", kv[0])
		}
		params[kv[0]] = kv[1]
	}
	return params, nil
}

// validateMarkerValues rejects values outside the spellings the parsers
// accept; each of them used to fall back to a default instead.
func validateMarkerValues(params map[string]string) error {
	if v, ok := params["remote"]; ok && parseRemoteParam(v) == "" {
		if parseBoolParam(v) || strings.EqualFold(strings.TrimSpace(v), "capable") {
			return fmt.Errorf(`remote=%q is no longer supported: lock order comes from the entity's category, so register the kind in a category (entity.EntityCategoryWorld / entity.EntityCategoryOther / ...) and use remote=none|managed|mirror`, v)
		}
		return fmt.Errorf(`remote=%q is not one of none|managed|mirror`, v)
	}
	if v, ok := params["category"]; ok && !validCategoryParam(v) {
		return fmt.Errorf(`category=%q is not a category constant expression (e.g. entity.EntityCategoryOther, view.EntityCategoryPlayer, EntityCategoryWorld)`, v)
	}
	if v, ok := params["syncTopic"]; ok {
		if err := validateSyncTopicParam(v); err != nil {
			return err
		}
	}
	if v, ok := params["lifetime"]; ok && !validLifetimeParam(v) {
		return fmt.Errorf(`lifetime=%q is not one of ephemeral|runtime_rebuild|persisted_hot_cold|resident|remote_managed|mirror_cache`, v)
	}
	for _, key := range []string{"noPersist", "sync"} {
		if v, ok := params[key]; ok && !validBoolParam(v) {
			return fmt.Errorf(`%s=%q is not a boolean (true/false, yes/no, on/off, 1/0)`, key, v)
		}
	}
	return nil
}

func validBoolParam(v string) bool {
	switch strings.ToLower(v) {
	case "1", "t", "true", "yes", "y", "on", "0", "f", "false", "no", "n", "off":
		return true
	}
	return false
}

func validLifetimeParam(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "ephemeral", "runtime_rebuild", "runtime-rebuild", "rebuild", "persisted_hot_cold", "persisted-hot-cold", "hotcold", "hot_cold", "resident", "remote_managed", "remote-managed", "mirror_cache", "mirror-cache":
		return true
	}
	return false
}

func hasRemoteEntityBase(st *ast.StructType) bool {
	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue
		}
		name := strings.TrimPrefix(exprToString(field.Type), "*")
		if name == "entity.RemoteEntityBase" || name == "RemoteEntityBase" {
			return true
		}
	}
	return false
}

func collectMethods(f *ast.File) map[string]map[string]bool {
	methods := make(map[string]map[string]bool)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name == nil {
			continue
		}
		recv := receiverTypeName(fn.Recv.List[0].Type)
		if recv == "" {
			continue
		}
		if methods[recv] == nil {
			methods[recv] = make(map[string]bool)
		}
		methods[recv][fn.Name.Name] = true
	}
	return methods
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	default:
		return ""
	}
}

func collectImports(f *ast.File) map[string]ImportDef {
	imports := make(map[string]ImportDef)
	for _, spec := range f.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		alias := filepath.Base(path)
		explicit := false
		if spec.Name != nil {
			alias = spec.Name.Name
			explicit = true
		}
		if alias == "_" || alias == "." {
			continue
		}
		imports[alias] = ImportDef{
			Alias:    alias,
			Path:     path,
			Explicit: explicit,
		}
	}
	return imports
}

func collectEntityImports(ent EntityDef, importMap map[string]ImportDef) []ImportDef {
	used := make(map[string]bool)
	add := func(expr string) {
		if alias := qualifier(expr); alias != "" {
			used[alias] = true
		}
	}

	add(ent.EntityKind)
	// The category constant may live in a business package that nothing else
	// in the generated file references. Go's import scope is per file, so the
	// source file importing it does not help; omitting it here generated a
	// file that used the package without importing it (RR-20260910-05).
	add(ent.Category)
	add(ent.SyncTopic)
	add(ent.SyncPacker)
	add(ent.SubjectPacker)
	for _, comp := range ent.Components {
		add(comp.CompType)
		add(comp.GetterType)
		if !comp.Cold {
			add(comp.TypeName)
		}
	}
	for _, dao := range ent.Daos {
		add(dao.TypeName)
	}

	out := make([]ImportDef, 0, len(used))
	for alias := range used {
		imp, ok := importMap[alias]
		if !ok {
			continue
		}
		if imp.Path == "github.com/tjbdwanghaibo/roost-core/entity" {
			continue
		}
		out = append(out, imp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Alias < out[j].Alias
	})
	return out
}

func qualifier(expr string) string {
	expr = strings.TrimLeft(expr, "*")
	if idx := strings.Index(expr, "."); idx > 0 {
		return expr[:idx]
	}
	return ""
}

func isGeneratedWireFile(name string) bool {
	return strings.HasPrefix(name, "gen_") || strings.HasSuffix(name, "_gen_wire.go")
}

// extractFields categorizes struct fields into components and DAOs.
func extractFields(st *ast.StructType, _ []byte, fset *token.FileSet) ([]ComponentField, []DaoField) {
	var comps []ComponentField
	var daos []DaoField

	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // embedded field, skip
		}

		fieldName := field.Names[0].Name

		// Check struct tags
		tag := ""
		if field.Tag != nil {
			tag = field.Tag.Value
		}

		// Parse tags for component/dao markers
		if strings.Contains(tag, `comp:`) {
			comp := parseCompTag(fieldName, field.Type, tag, fset)
			if comp != nil {
				comps = append(comps, *comp)
			}
		} else if strings.Contains(tag, `dao:`) {
			dao := parseDaoTag(fieldName, field.Type, tag, fset)
			if dao != nil {
				daos = append(daos, *dao)
			}
		}
	}

	return comps, daos
}

// parseCompTag extracts component info from struct tag.
// Format: `comp:"CompTypeBag"`
func parseCompTag(fieldName string, fieldType ast.Expr, tag string, _ *token.FileSet) *ComponentField {
	compType := extractTagValue(tag, "comp")
	if compType == "" {
		return nil
	}
	parts := tagParts(compType)
	compType = parts[0]

	typeName := exprToString(fieldType)

	return &ComponentField{
		FieldName: fieldName,
		TypeName:  typeName,
		CompType:  compType,
		Cold:      tagHasOption(parts, "cold"),
	}
}

// parseDaoTag extracts DAO info from struct tag.
// Format: `dao:"players"`
func parseDaoTag(fieldName string, fieldType ast.Expr, tag string, _ *token.FileSet) *DaoField {
	collName := extractTagValue(tag, "dao")
	if collName == "" {
		return nil
	}
	parts := tagParts(collName)
	collName = parts[0]

	typeName := exprToString(fieldType)

	return &DaoField{
		FieldName: fieldName,
		TypeName:  typeName,
		CollName:  collName,
		Cold:      tagHasOption(parts, "cold"),
	}
}

func deriveAccessors(ent *EntityDef) {
	for i := range ent.Components {
		comp := &ent.Components[i]
		compName := componentNameFromCompType(comp.CompType)
		if compName == "" {
			continue
		}
		comp.GetterName = compName + "Comp"
		if alias := qualifier(comp.CompType); alias != "" {
			comp.GetterType = alias + ".I" + compName + "Component"
		} else {
			comp.GetterType = comp.TypeName
		}
	}
	for i := range ent.Daos {
		dao := &ent.Daos[i]
		dao.GetterName = daoGetterName(ent.Name, dao.TypeName)
	}
}

func componentNameFromCompType(compType string) string {
	name := lastTypeSegment(compType)
	return strings.TrimPrefix(name, "CompType")
}

func daoGetterName(entityName, typeName string) string {
	name := lastTypeSegment(derefType(typeName))
	if entityName != "" {
		name = strings.TrimPrefix(name, entityName)
	}
	if name == "" {
		return "Dao"
	}
	return name
}

func lastTypeSegment(typeName string) string {
	typeName = strings.TrimLeft(typeName, "*")
	if idx := strings.LastIndex(typeName, "."); idx >= 0 {
		return typeName[idx+1:]
	}
	return typeName
}

func tagParts(value string) []string {
	raw := strings.Split(value, ",")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

func tagHasOption(parts []string, option string) bool {
	for _, part := range parts[1:] {
		if strings.EqualFold(part, option) {
			return true
		}
	}
	return false
}

// extractTagValue extracts a value from a struct tag string.
func extractTagValue(tag, key string) string {
	// tag is like `comp:"value" dao:"value"`
	tag = strings.Trim(tag, "`")
	re := regexp.MustCompile(key + `:"([^"]*)"`)
	matches := re.FindStringSubmatch(tag)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func parseBoolParam(v string) bool {
	switch strings.ToLower(v) {
	case "1", "t", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseRemoteParam(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "f", "false", "no", "n", "off", "none":
		return "entity.RemotePolicyNone"
	case "managed":
		return "entity.RemotePolicyManaged"
	case "mirror":
		return "entity.RemotePolicyMirror"
	default:
		// "capable" and the boolean true spellings used to mean
		// entity.RemotePolicyCapable, which said only "put this kind in the
		// first lock rank". Lock order is the kind's category now, so the
		// value said nothing the category does not and was removed from the
		// framework. Rejected rather than silently mapped to none, because
		// none would change an existing entity's lock rank.
		return ""
	}
}

// validCategoryParam accepts an exported identifier, optionally package
// qualified. The value is pasted into generated Go, so a literal, a quoted
// string or a stray space has to fail here rather than in the consumer's
// compiler — which is the whole point of moving the category into the marker.
func validCategoryParam(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	name := v
	if idx := strings.Index(v, "."); idx >= 0 {
		qualifier := v[:idx]
		name = v[idx+1:]
		if !isGoIdent(qualifier) {
			return false
		}
	}
	if !isGoIdent(name) {
		return false
	}
	first := name[0]
	return first >= 'A' && first <= 'Z'
}

func isGoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func parseLifetimeParam(v string, noPersist bool, remotePolicy string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "ephemeral":
		return "entity.EntityLifetimeEphemeral"
	case "runtime_rebuild", "runtime-rebuild", "rebuild":
		return "entity.EntityLifetimeRuntimeRebuild"
	case "persisted_hot_cold", "persisted-hot-cold", "hotcold", "hot_cold":
		return "entity.EntityLifetimePersistedHotCold"
	case "resident":
		return "entity.EntityLifetimeResident"
	case "remote_managed", "remote-managed":
		return "entity.EntityLifetimeRemoteManaged"
	case "mirror_cache", "mirror-cache":
		return "entity.EntityLifetimeMirrorCache"
	}
	switch remotePolicy {
	case "entity.RemotePolicyManaged":
		return "entity.EntityLifetimeRemoteManaged"
	case "entity.RemotePolicyMirror":
		return "entity.EntityLifetimeMirrorCache"
	}
	if noPersist {
		return "entity.EntityLifetimeEphemeral"
	}
	return "entity.EntityLifetimePersistedHotCold"
}

// exprToString converts an AST expression to its string representation.
func exprToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprToString(t.X)
	case *ast.SelectorExpr:
		return exprToString(t.X) + "." + t.Sel.Name
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// validateSyncTopicParam refuses the one spelling that used to be accepted and
// silently mean something else.
//
// syncTopic takes a topic NAME or a package-qualified constant. A bare
// identifier is neither: it looks like a Go constant and was written out as
// the string of its own name, so `syncTopic=SyncTopicPlayer` produced
// Topic: "SyncTopicPlayer" and the entity subscribed to a topic nobody chose.
// Nothing failed — not the generator, not the compiler — which is why this is
// a refusal and not a documentation note (RR-20260918-07).
//
// What still passes: anything that cannot be mistaken for a constant (a
// lower-case or quoted literal) and anything that unambiguously is one (a
// qualified name).
func validateSyncTopicParam(v string) error {
	value := strings.TrimSpace(v)
	if value == "" || strings.ContainsAny(value, "\"'`") || strings.Contains(value, ".") {
		return nil
	}
	first := rune(value[0])
	if first < 'A' || first > 'Z' {
		return nil
	}
	for _, r := range value {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return nil
		}
	}
	return fmt.Errorf(`syncTopic=%s is ambiguous: a bare identifier is written out as the literal %q, not as the constant's value. Write the topic in quotes (syncTopic="%s"), write it in lower case, or name a package-qualified constant (syncTopic=pkg.%s)`, value, value, value, value)
}
