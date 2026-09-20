package dao

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var daoMarkerRe = marker.Regexp("dao", `\s+(.+)`)
var redisDaoMarkerRe = marker.Regexp("redisdao", `\s+(.+)`)

// Definitions holds all parsed DAO and nested struct definitions.
type Definitions struct {
	Daos      []DaoDef
	RedisDaos []RedisDaoDef
	Nested    []NestedDef
}

// DaoDef is a DAO struct definition.
type DaoDef struct {
	Name    string // struct name, e.g. "PlayerDao"
	Coll    string // collection name, e.g. "players"
	Db      string // logical database name, e.g. "game"
	DbScope string // global or sid
	// Schema is the DAO's schema version, from `schema=` (1 when omitted).
	// Raising it is what lets a project run a migration: the generated
	// Migrate hands the stored document's version and this one to
	// migration.MigrateDAO, and with a constant version those two could
	// never differ.
	Schema uint32
	Fields []FieldDef
}

// RedisDaoDef is a Redis DAO struct definition.
type RedisDaoDef struct {
	Name      string
	Mode      string
	Key       string
	KeyType   string
	Prefix    string
	Version   string
	TTL       string
	RedisName string
	Fields    []FieldDef
}

// NestedDef is a nested struct definition.
type NestedDef struct {
	Name   string
	Fields []FieldDef
}

// FieldDef describes a single field.
type FieldDef struct {
	Name      string
	TypeStr   string    // raw type string, e.g. "int32", "map[int64]*EquipInfo"
	Kind      FieldKind // classified type kind
	Tag       DaoTag    // parsed dao tag
	MapKey    string    // for map types: key type
	MapVal    string    // for map types: value type
	SliceElem string    // for slice types: element type
	IsPtr     bool      // whether the value type is pointer (map val or slice elem)
}

type FieldKind int

const (
	KindBasic  FieldKind = iota // 0: int, string, bool, float, etc.
	KindSlice                   // 1: []T
	KindMap                     // 2: map[K]V
	KindStruct                  // 3: nested struct value
)

type DaoTag struct {
	Persist bool
	Sync    bool
	Skip    bool // dao:"-"
	Map     string
}

func parseDefDir(dir string) (*Definitions, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	defs := &Definitions{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasPrefix(entry.Name(), "gen_") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}

		f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}

		if err := extractDefs(fset, f, defs); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
	}

	return defs, nil
}

func extractDefs(fset *token.FileSet, f *ast.File, defs *Definitions) error {
	// Collect all struct definitions in the file
	allStructs := make(map[string]*ast.StructType)

	// Collect DAO markers: line → params
	type daoMarker struct {
		line     int
		params   map[string]string
		consumed bool
	}
	var markers []daoMarker
	var redisMarkers []daoMarker

	for _, cg := range f.Comments {
		for _, c := range cg.List {
			// A marker prefix without valid parameters is a mistake the
			// author must hear about: a silently ignored marker means a DAO
			// that silently does not persist.
			if rest, ok := marker.Cut(c.Text, "dao"); ok && strings.TrimSpace(rest) == "" {
				return fmt.Errorf("line %d: //roost:dao marker is missing parameters (want e.g. //roost:dao coll=heroes db=game)", fset.Position(c.Pos()).Line)
			}
			if rest, ok := marker.Cut(c.Text, "redisdao"); ok && strings.TrimSpace(rest) == "" {
				return fmt.Errorf("line %d: //roost:redisdao marker is missing parameters (want e.g. //roost:redisdao mode=ref-hmap key=... prefix=...)", fset.Position(c.Pos()).Line)
			}
			if m := daoMarkerRe.FindStringSubmatch(c.Text); m != nil {
				markers = append(markers, daoMarker{
					line:   fset.Position(c.Pos()).Line,
					params: parseKV(m[1]),
				})
			}
			if m := redisDaoMarkerRe.FindStringSubmatch(c.Text); m != nil {
				redisMarkers = append(redisMarkers, daoMarker{
					line:   fset.Position(c.Pos()).Line,
					params: parseKV(m[1]),
				})
			}
		}
	}

	// First pass: collect all struct names
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
			allStructs[typeSpec.Name.Name] = structType
		}
	}

	// Second pass: find DAO structs by marker
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
			// A marker binds when directly adjacent (the historical 1-2 line
			// rule) or anywhere inside the declaration's doc comment group,
			// so ordinary doc comments between marker and struct are legal.
			docStart := structLine
			if typeSpec.Doc != nil {
				docStart = fset.Position(typeSpec.Doc.Pos()).Line
			} else if genDecl.Doc != nil {
				docStart = fset.Position(genDecl.Doc.Pos()).Line
			}
			binds := func(line int) bool {
				return line == structLine-1 || line == structLine-2 || (line >= docStart && line < structLine)
			}

			for i := range markers {
				m := &markers[i]
				if !binds(m.line) {
					continue
				}
				// A marker can never intend two structs: a grouped type
				// declaration with one marker on the group would silently
				// write every struct into the same collection.
				if m.consumed {
					return fmt.Errorf("line %d: //roost:dao marker binds to multiple structs; grouped type declarations need one marker directly above each struct", m.line)
				}
				m.consumed = true

				if m.params["coll"] == "" || m.params["db"] == "" {
					return fmt.Errorf("line %d: //roost:dao on %s requires coll= and db=", m.line, typeSpec.Name.Name)
				}
				fields, err := extractFieldDefs(structType, defs)
				if err != nil {
					return fmt.Errorf("dao %s: %w", typeSpec.Name.Name, err)
				}
				dbScope := m.params["dbscope"]
				if dbScope == "" {
					dbScope = "global"
				}
				schema, err := parseSchemaParam(m.params["schema"])
				if err != nil {
					return fmt.Errorf("line %d: //roost:dao on %s: %w", m.line, typeSpec.Name.Name, err)
				}
				defs.Daos = append(defs.Daos, DaoDef{
					Name:    typeSpec.Name.Name,
					Coll:    m.params["coll"],
					Db:      m.params["db"],
					DbScope: dbScope,
					Schema:  schema,
					Fields:  fields,
				})
				break
			}
			for i := range redisMarkers {
				m := &redisMarkers[i]
				if !binds(m.line) {
					continue
				}
				if m.consumed {
					return fmt.Errorf("line %d: //roost:redisdao marker binds to multiple structs; grouped type declarations need one marker directly above each struct", m.line)
				}
				m.consumed = true

				if m.params["key"] == "" || m.params["prefix"] == "" {
					return fmt.Errorf("line %d: //roost:redisdao on %s requires key= and prefix=", m.line, typeSpec.Name.Name)
				}
				fields, err := extractFieldDefs(structType, defs)
				if err != nil {
					return fmt.Errorf("redis dao %s: %w", typeSpec.Name.Name, err)
				}
				keyType := m.params["key_type"]
				if keyType == "" {
					keyType = resolveFieldPathType(structType, allStructs, m.params["key"])
				}
				mode := m.params["mode"]
				if mode == "" {
					mode = "ref-hmap"
				}
				defs.RedisDaos = append(defs.RedisDaos, RedisDaoDef{
					Name:      typeSpec.Name.Name,
					Mode:      mode,
					Key:       m.params["key"],
					KeyType:   keyType,
					Prefix:    m.params["prefix"],
					Version:   m.params["version"],
					TTL:       m.params["ttl"],
					RedisName: m.params["name"],
					Fields:    fields,
				})
				break
			}
		}
	}

	// An unbound marker is never intent-free: it marks a struct that will
	// silently not persist. Fail instead of generating a partial result.
	for _, m := range markers {
		if !m.consumed {
			return fmt.Errorf("line %d: //roost:dao marker is not attached to a struct (it must be adjacent to, or inside the doc comment of, a struct type declaration)", m.line)
		}
	}
	for _, m := range redisMarkers {
		if !m.consumed {
			return fmt.Errorf("line %d: //roost:redisdao marker is not attached to a struct (it must be adjacent to, or inside the doc comment of, a struct type declaration)", m.line)
		}
	}

	// Third pass: auto-detect nested structs from DAO field types recursively.
	nestedNames := make(map[string]bool)
	for _, nested := range defs.Nested {
		nestedNames[nested.Name] = true
	}
	var queue []string
	for _, dao := range defs.Daos {
		for _, field := range dao.Fields {
			if name := resolveNestedName(field); name != "" {
				queue = append(queue, name)
			}
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if nestedNames[name] {
			continue
		}
		st, ok := allStructs[name]
		if !ok {
			continue
		}
		nestedNames[name] = true
		nestedFields, err := extractFieldDefs(st, defs)
		if err != nil {
			return fmt.Errorf("nested struct %s: %w", name, err)
		}
		defs.Nested = append(defs.Nested, NestedDef{
			Name:   name,
			Fields: nestedFields,
		})
		for _, field := range nestedFields {
			if next := resolveNestedName(field); next != "" && !nestedNames[next] {
				queue = append(queue, next)
			}
		}
	}
	return nil
}

// resolveNestedName returns the struct type name if the field references a nested struct.
func resolveNestedName(f FieldDef) string {
	switch f.Kind {
	case KindStruct:
		return stripPtr(f.TypeStr)
	case KindMap:
		if f.IsPtr {
			return f.MapVal
		}
		if !isBasicType(f.MapVal) {
			return f.MapVal
		}
	case KindSlice:
		if f.IsPtr {
			return f.SliceElem
		}
		if !isBasicType(f.SliceElem) {
			return f.SliceElem
		}
	}
	return ""
}

func stripPtr(s string) string {
	if len(s) > 0 && s[0] == '*' {
		return s[1:]
	}
	return s
}

func extractFieldDefs(st *ast.StructType, defs *Definitions) ([]FieldDef, error) {
	var fields []FieldDef

	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // embedded (e.g. dataengine.DirtyHook), skip
		}

		name := field.Names[0].Name
		// Skip unexported and internal fields
		if !ast.IsExported(name) {
			continue
		}

		typeStr := exprString(field.Type)
		tag, err := parseDaoTag(field.Tag)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}

		if tag.Skip {
			continue
		}

		fd := FieldDef{
			Name:    name,
			TypeStr: typeStr,
			Tag:     tag,
		}

		if err := classifyField(&fd, field.Type); err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		if fd.Kind == KindMap && fd.Tag.Map == "" {
			fd.Tag.Map = "small"
		}
		fields = append(fields, fd)
	}

	return fields, nil
}

// classifyField maps the field's syntax to a generation kind. Shapes the
// templates cannot generate correct code for are rejected here: format.Source
// only guarantees syntax, so anything that slips through would surface as a
// compile error in the downstream project instead of at generation time.
func classifyField(fd *FieldDef, expr ast.Expr) error {
	switch t := expr.(type) {
	case *ast.MapType:
		fd.Kind = KindMap
		fd.MapKey = exprString(t.Key)
		val := t.Value
		if star, ok := val.(*ast.StarExpr); ok {
			fd.IsPtr = true
			fd.MapVal = exprString(star.X)
		} else {
			fd.MapVal = exprString(val)
		}
	case *ast.ArrayType:
		if t.Len != nil {
			return fmt.Errorf("unsupported type %s: fixed-size arrays are not supported, use a slice or exclude the field with dao:\"-\"", exprString(expr))
		}
		fd.Kind = KindSlice
		elem := t.Elt
		if star, ok := elem.(*ast.StarExpr); ok {
			fd.IsPtr = true
			fd.SliceElem = exprString(star.X)
		} else {
			fd.SliceElem = exprString(elem)
		}
	case *ast.Ident:
		if isBasicType(t.Name) {
			fd.Kind = KindBasic
		} else {
			fd.Kind = KindStruct
		}
	case *ast.StarExpr:
		fd.IsPtr = true
		ident, ok := t.X.(*ast.Ident)
		if !ok {
			return fmt.Errorf("unsupported type %s: only pointers to named local types are supported, exclude the field with dao:\"-\" if it must stay", exprString(expr))
		}
		if isBasicType(ident.Name) {
			fd.Kind = KindBasic
		} else {
			fd.Kind = KindStruct
		}
	case *ast.SelectorExpr:
		fd.Kind = KindStruct
	default:
		return fmt.Errorf("unsupported type %s: exclude the field with dao:\"-\" if it must stay", exprString(expr))
	}
	return nil
}

var daoTagRe = regexp.MustCompile(`dao:"([^"]*)"`)

// parseDaoTag interprets the field's dao tag. No tag keeps the safe default
// (persist and sync both on). A tag that is present must state persist/sync
// intent explicitly: the historical behavior of zeroing both when the tag
// only carried auxiliary options (e.g. dao:"map=fast") silently dropped
// persistence, which is a data-loss trap, so it is a hard error now.
func parseDaoTag(tag *ast.BasicLit) (DaoTag, error) {
	if tag == nil {
		return DaoTag{Persist: true, Sync: true}, nil // default
	}

	raw := strings.Trim(tag.Value, "`")
	m := daoTagRe.FindStringSubmatch(raw)
	if m == nil {
		return DaoTag{Persist: true, Sync: true}, nil // no dao tag = default
	}

	val := m[1]
	if val == "-" {
		return DaoTag{Skip: true}, nil
	}
	if strings.TrimSpace(val) == "" {
		return DaoTag{}, fmt.Errorf("dao tag %q does not state persist/sync intent; add persist/sync (or nopersist,nosync to disable both explicitly)", val)
	}

	parts := strings.Split(val, ",")
	dt := DaoTag{}
	var noPersist, noSync bool
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case p == "persist":
			dt.Persist = true
		case p == "nopersist":
			noPersist = true
		case p == "sync":
			dt.Sync = true
		case p == "nosync":
			noSync = true
		case p == "map=small" || p == "map=fast" || p == "map=sharded":
			dt.Map = strings.TrimPrefix(p, "map=")
		case strings.HasPrefix(p, "map="):
			return DaoTag{}, fmt.Errorf("dao tag has unknown map mode %q (valid: small, fast, sharded)", p)
		default:
			return DaoTag{}, fmt.Errorf("dao tag has unknown option %q (valid: persist, nopersist, sync, nosync, map=small|fast|sharded, -)", p)
		}
	}
	if dt.Persist && noPersist {
		return DaoTag{}, fmt.Errorf("dao tag states both persist and nopersist")
	}
	if dt.Sync && noSync {
		return DaoTag{}, fmt.Errorf("dao tag states both sync and nosync")
	}
	if !dt.Persist && !noPersist && !dt.Sync && !noSync {
		return DaoTag{}, fmt.Errorf("dao tag %q does not state persist/sync intent; add persist/sync (or nopersist,nosync to disable both explicitly)", val)
	}
	return dt, nil
}

func parseKV(s string) map[string]string {
	params := make(map[string]string)
	for _, p := range strings.Fields(s) {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = kv[1]
		}
	}
	return params
}

func resolveFieldPathType(root *ast.StructType, allStructs map[string]*ast.StructType, path string) string {
	if root == nil || path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	current := root
	for i, part := range parts {
		field := findStructField(current, part)
		if field == nil {
			return ""
		}
		if i == len(parts)-1 {
			return exprString(field.Type)
		}
		nextName := stripPtr(exprString(field.Type))
		if idx := strings.LastIndex(nextName, "."); idx >= 0 {
			return ""
		}
		next, ok := allStructs[nextName]
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

func findStructField(st *ast.StructType, name string) *ast.Field {
	if st == nil {
		return nil
	}
	for _, field := range st.Fields.List {
		for _, fieldName := range field.Names {
			if fieldName.Name == name {
				return field
			}
		}
	}
	return nil
}

func isBasicType(name string) bool {
	switch name {
	case "bool", "byte", "rune",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64",
		"string":
		return true
	}
	return false
}

func isIntegerType(name string) bool {
	switch name {
	case "byte", "rune",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return true
	}
	return false
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprString(t.Elt)
		}
		return fmt.Sprintf("[%s]%s", exprString(t.Len), exprString(t.Elt))
	case *ast.BasicLit:
		return t.Value
	default:
		return "any"
	}
}

// parseSchemaParam reads `schema=`. Absent means 1 — the version every
// definition has always had, so omitting it changes nothing.
//
// Zero is refused rather than defaulted: a DAO at version 0 would make every
// stored document look newer than the code, and the migration runner would
// have nothing to run toward. A non-number is refused because silently
// ignoring a typo here means the migration nobody notices is not running.
func parseSchemaParam(value string) (uint32, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("schema=%q is not a positive version number", value)
	}
	return uint32(parsed), nil
}
