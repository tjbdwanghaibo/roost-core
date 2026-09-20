package dao

import (
	"bytes"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/genutil"
	"go/format"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
)

func generateDao(dao DaoDef, defs *Definitions, pkg string, outFile string, force bool) (bool, error) {
	if err := validateDatabaseScope(dao); err != nil {
		return false, err
	}
	if err := validateGeneratedStorageFields(dao.Name, dao.Fields, "id", "tracker"); err != nil {
		return false, err
	}
	if err := validateGeneratedMapFields(dao.Fields); err != nil {
		return false, err
	}
	tmpl, err := template.New("dao").Funcs(funcMap(defs)).Parse(daoTemplate)
	if err != nil {
		return false, fmt.Errorf("template parse: %w", err)
	}

	data := daoTmplData{
		Package: pkg,
		Dao:     dao,
		Defs:    defs,
		HasMaps: hasMapFields(dao.Fields),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("template exec: %w", err)
	}

	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated dao: %w", err)
	}
	return writeIfChanged(content, outFile, force)
}

// generateBSONHelpers writes the package's shared container conversions
// (bsonHelpersFileName); it is only written when the package has nested
// structs, since only their wire forms use it.
func generateBSONHelpers(pkg string, outFile string, force bool) (bool, error) {
	tmpl, err := template.New("bsonhelpers").Parse(bsonHelpersTemplate)
	if err != nil {
		return false, fmt.Errorf("template parse: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Package string }{Package: pkg}); err != nil {
		return false, fmt.Errorf("template exec: %w", err)
	}
	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated bson helpers: %w", err)
	}
	return writeIfChanged(content, outFile, force)
}

// nestedFuncMap is the nested template's helper set; a test renders the
// template with it to pin the generated shape.
func nestedFuncMap(defs *Definitions) template.FuncMap {
	return template.FuncMap{
		"snakeCase":          toSnake,
		"lower1":             lower1,
		"bsonKey":            func(name string) string { return toSnake(name) },
		"fieldType":          fieldType,
		"fieldVar":           safeFieldVarName,
		"mapValType":         mapValType,
		"mapNewExpr":         mapNewExpr,
		"rawMapType":         rawMapType,
		"hasMaps":            hasMapFields,
		"isNested":           func(typeName string) bool { return isNestedType(defs, typeName) },
		"hasHook":            func(f FieldDef) bool { return fieldCarriesChildHook(defs, f) },
		"hasDetach":          func(f FieldDef) bool { return fieldDetachesOldChildren(defs, f) },
		"nestedFieldOrdinal": nestedFieldOrdinal,
		"wireType":           func(f FieldDef) string { return wireType(defs, f) },
		"toWire":             func(f FieldDef, expr string) string { return toWire(defs, f, expr) },
		"fromWire":           func(f FieldDef, expr string) string { return fromWire(defs, f, expr) },
	}
}

// --- wire form (U-0224 补修) ---
//
// A nested struct's fields are unexported, so the BSON codec cannot encode it
// by reflection. The first fix gave every nested type MarshalBSON, but the
// bson.Marshaler contract returns a fresh []byte per value that the parent
// then copies — about four allocations per nested value, three times the
// reflection floor for a document with a few of them. So the parent DAO does
// not encode nested values through Marshaler at all: it places their WIRE
// FORM — a generated struct with exported fields (`<name>BSONDoc`) — into its
// own document and lets reflection encode it inline. These helpers name the
// wire type of a field and the conversion expressions in each direction;
// non-nested fields pass through unchanged.

// wireDocName is the wire struct's type name for a nested type.
func wireDocName(typeName string) string {
	return lower1(strings.TrimPrefix(typeName, "*")) + "BSONDoc"
}

func wireType(defs *Definitions, f FieldDef) string {
	switch f.Kind {
	case KindStruct:
		if isNestedType(defs, f.TypeStr) {
			if strings.HasPrefix(f.TypeStr, "*") {
				return "*" + wireDocName(f.TypeStr)
			}
			return wireDocName(f.TypeStr)
		}
		return f.TypeStr
	case KindSlice:
		if isNestedType(defs, f.SliceElem) {
			return "[]" + ptrPrefix(f) + wireDocName(f.SliceElem)
		}
		return f.TypeStr
	case KindMap:
		if isNestedType(defs, f.MapVal) {
			return "map[" + f.MapKey + "]" + ptrPrefix(f) + wireDocName(f.MapVal)
		}
		return rawMapType(f)
	default:
		return f.TypeStr
	}
}

func ptrPrefix(f FieldDef) string {
	if f.IsPtr {
		return "*"
	}
	return ""
}

// toWire is the Go expression converting the stored value expr into its wire
// form; fromWire is the inverse. Element conversions are the per-type
// functions the nested template generates (`X.bsonDoc`, `xPtrBSONDoc`,
// `xFromBSONDoc`, `xPtrFromBSONDoc`), mapped over containers by the generic
// daoMapDocs / daoSliceDocs helpers written once per package.
func toWire(defs *Definitions, f FieldDef, expr string) string {
	switch f.Kind {
	case KindStruct:
		if isNestedType(defs, f.TypeStr) {
			if strings.HasPrefix(f.TypeStr, "*") {
				return lower1(strings.TrimPrefix(f.TypeStr, "*")) + "PtrBSONDoc(" + expr + ")"
			}
			return expr + ".bsonDoc()"
		}
	case KindSlice:
		if isNestedType(defs, f.SliceElem) {
			return "daoSliceDocs(" + expr + ", " + elemToWire(f.SliceElem, f.IsPtr) + ")"
		}
	case KindMap:
		if isNestedType(defs, f.MapVal) {
			return "daoMapDocs(" + expr + ", " + elemToWire(f.MapVal, f.IsPtr) + ")"
		}
	}
	return expr
}

func fromWire(defs *Definitions, f FieldDef, expr string) string {
	switch f.Kind {
	case KindStruct:
		if isNestedType(defs, f.TypeStr) {
			if strings.HasPrefix(f.TypeStr, "*") {
				return lower1(strings.TrimPrefix(f.TypeStr, "*")) + "PtrFromBSONDoc(" + expr + ")"
			}
			return lower1(f.TypeStr) + "FromBSONDoc(" + expr + ")"
		}
	case KindSlice:
		if isNestedType(defs, f.SliceElem) {
			return "daoSliceDocs(" + expr + ", " + elemFromWire(f.SliceElem, f.IsPtr) + ")"
		}
	case KindMap:
		if isNestedType(defs, f.MapVal) {
			return "daoMapDocs(" + expr + ", " + elemFromWire(f.MapVal, f.IsPtr) + ")"
		}
	}
	return expr
}

func elemToWire(typeName string, ptr bool) string {
	if ptr {
		return lower1(typeName) + "PtrBSONDoc"
	}
	return typeName + ".bsonDoc"
}

func elemFromWire(typeName string, ptr bool) string {
	if ptr {
		return lower1(typeName) + "PtrFromBSONDoc"
	}
	return lower1(typeName) + "FromBSONDoc"
}

func generateNested(nested NestedDef, defs *Definitions, pkg string, outFile string, force bool) (bool, error) {
	if err := validateGeneratedStorageFields(nested.Name, nested.Fields); err != nil {
		return false, err
	}
	if err := validateGeneratedMapFields(nested.Fields); err != nil {
		return false, err
	}
	tmpl, err := template.New("nested").Funcs(nestedFuncMap(defs)).Parse(nestedTemplate)
	if err != nil {
		return false, fmt.Errorf("template parse: %w", err)
	}

	data := nestedTmplData{
		Package: pkg,
		Nested:  nested,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("template exec: %w", err)
	}

	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated nested: %w", err)
	}
	return writeIfChanged(content, outFile, force)
}

func generateRedisDao(dao RedisDaoDef, pkg string, outFile string, force bool) (bool, error) {
	if dao.Mode == "" {
		dao.Mode = "ref-hmap"
	}
	if dao.Mode != "ref-hmap" && dao.Mode != "raw" {
		return false, fmt.Errorf("redis dao %s has unsupported mode %q", dao.Name, dao.Mode)
	}
	if dao.Key == "" {
		return false, fmt.Errorf("redis dao %s missing key", dao.Name)
	}
	if dao.KeyType == "" {
		return false, fmt.Errorf("redis dao %s missing key type for key %s", dao.Name, dao.Key)
	}
	defaultTTL, err := redisDaoDefaultTTL(dao)
	if err != nil {
		return false, err
	}
	tmpl, err := template.New("redis_dao").Funcs(template.FuncMap{
		"lower1":   lower1,
		"goString": strconv.Quote,
		"isRaw":    func(mode string) bool { return mode == "raw" },
		"isRefMap": func(mode string) bool { return mode == "" || mode == "ref-hmap" },
	}).Parse(redisDaoTemplate)
	if err != nil {
		return false, fmt.Errorf("template parse: %w", err)
	}

	data := redisDaoTmplData{
		Package:       pkg,
		Dao:           dao,
		DefaultPrefix: redisDaoDefaultPrefix(dao),
		RedisName:     redisDaoName(dao),
		DefaultTTL:    defaultTTL,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("template exec: %w", err)
	}

	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format generated redis dao: %w", err)
	}
	return writeIfChanged(content, outFile, force)
}

func writeIfChanged(content []byte, outFile string, force bool) (bool, error) {
	return genutil.WriteIfChanged(outFile, content, force)
}

func funcMap(defs *Definitions) template.FuncMap {
	return template.FuncMap{
		"snakeCase":     toSnake,
		"lower1":        lower1,
		"daoDBConst":    daoDBConstName,
		"daoCollConst":  daoCollectionConstName,
		"dbScope":       databaseScopeExpr,
		"isNested":      func(typeName string) bool { return isNestedType(defs, typeName) },
		"wireType":      func(f FieldDef) string { return wireType(defs, f) },
		"toWire":        func(f FieldDef, expr string) string { return toWire(defs, f, expr) },
		"fromWire":      func(f FieldDef, expr string) string { return fromWire(defs, f, expr) },
		"persistFields": func(fields []FieldDef) []FieldDef { return filterPersist(fields) },
		"syncFields":    func(fields []FieldDef) []FieldDef { return filterSync(fields) },
		"dirtyFields":   func(fields []FieldDef) []FieldDef { return filterDirty(fields) },
		"bsonKey":       func(name string) string { return toSnake(name) },
		"fieldMaskName": fieldMaskName,
		"fieldType":     fieldType,
		"fieldVar":      safeFieldVarName,
		"mapValType":    mapValType,
		"mapNewExpr":    mapNewExpr,
		"rawMapType":    rawMapType,
		"mapHelperName": mapHelperName,
		"hasMaps":       hasMapFields,
		"schemaVersion": func(d DaoDef) uint32 {
			if d.Schema == 0 {
				return 1
			}
			return d.Schema
		},
	}
}

func validateDatabaseScope(dao DaoDef) error {
	if dao.DbScope == "" || dao.DbScope == "global" || dao.DbScope == "sid" {
		return nil
	}
	return fmt.Errorf("dao %s has unsupported dbscope %q (want global or sid)", dao.Name, dao.DbScope)
}

func databaseScopeExpr(dao DaoDef) string {
	if dao.DbScope == "sid" {
		return "dataengine.DatabaseServer"
	}
	return "dataengine.DatabaseGlobal"
}

func fieldMaskName(daoName, fieldName string) string {
	return lower1(daoName) + "Field" + fieldName
}

func daoDBConstName(daoName string) string {
	return derefDaoTypeName(daoName) + "DBName"
}

func daoCollectionConstName(daoName string) string {
	return derefDaoTypeName(daoName) + "Collection"
}

func derefDaoTypeName(typeName string) string {
	typeName = strings.TrimPrefix(typeName, "*")
	if idx := strings.LastIndex(typeName, "."); idx >= 0 {
		typeName = typeName[idx+1:]
	}
	return typeName
}

func fieldType(f FieldDef) string {
	if f.Kind == KindMap {
		switch f.Tag.Map {
		case "fast":
			return "*fmap.FastMap[" + f.MapKey + ", " + mapValType(f) + "]"
		case "sharded":
			return "*fmap.ShardedSafeMap[" + f.MapKey + ", " + mapValType(f) + "]"
		default:
			return "*fmap.SmallSafeMap[" + f.MapKey + ", " + mapValType(f) + "]"
		}
	}
	return f.TypeStr
}

func mapNewExpr(f FieldDef, capExpr string) string {
	keyType := f.MapKey
	valType := mapValType(f)
	switch f.Tag.Map {
	case "fast":
		return "fmap.NewFastMap[" + keyType + ", " + valType + "](" + capExpr + ", " + mapHashExpr(keyType) + ")"
	case "sharded":
		return "fmap.NewShardedSafeMap[" + keyType + ", " + valType + "](" + shardCountExpr(capExpr) + ", " + mapHashExpr(keyType) + ")"
	default:
		return "fmap.NewSmallSafeMap[" + keyType + ", " + valType + "](" + capExpr + ")"
	}
}

func mapHashExpr(keyType string) string {
	if keyType == "string" {
		return "fmap.HashString"
	}
	if isIntegerType(keyType) {
		return "fmap.HashInteger[" + keyType + "]"
	}
	return "nil"
}

func validateGeneratedMapFields(fields []FieldDef) error {
	for _, f := range fields {
		if f.Kind != KindMap {
			continue
		}
		switch f.Tag.Map {
		case "", "small":
			continue
		case "fast", "sharded":
			if f.MapKey == "string" || isIntegerType(f.MapKey) {
				continue
			}
			return fmt.Errorf("dao map field %s uses map=%s with unsupported key type %s", f.Name, f.Tag.Map, f.MapKey)
		default:
			return fmt.Errorf("dao map field %s has unknown map kind %q", f.Name, f.Tag.Map)
		}
	}
	return nil
}

func validateGeneratedStorageFields(typeName string, fields []FieldDef, reserved ...string) error {
	owners := make(map[string]string, len(fields)+len(reserved))
	for _, name := range reserved {
		owners[name] = "framework field " + name
	}
	for _, field := range fields {
		name := fieldVarName(field.Name)
		if owner, exists := owners[name]; exists {
			return fmt.Errorf("dao %s field %s generates private storage name %q, which conflicts with %s", typeName, field.Name, name, owner)
		}
		owners[name] = "field " + field.Name
	}
	return nil
}

func shardCountExpr(capExpr string) string {
	if capExpr == "0" {
		return "32"
	}
	return "32"
}

func mapValType(f FieldDef) string {
	if f.IsPtr {
		return "*" + f.MapVal
	}
	return f.MapVal
}

func rawMapType(f FieldDef) string {
	return "map[" + f.MapKey + "]" + mapValType(f)
}

func mapHelperName(daoName, fieldName string) string {
	return lower1(daoName) + fieldName + "RawMap"
}

func hasMapFields(fields []FieldDef) bool {
	for _, f := range fields {
		if f.Kind == KindMap {
			return true
		}
	}
	return false
}

func filterPersist(fields []FieldDef) []FieldDef {
	var out []FieldDef
	for _, f := range fields {
		if f.Tag.Persist {
			out = append(out, f)
		}
	}
	return out
}

func filterSync(fields []FieldDef) []FieldDef {
	var out []FieldDef
	for _, f := range fields {
		if f.Tag.Sync {
			out = append(out, f)
		}
	}
	return out
}

func filterDirty(fields []FieldDef) []FieldDef {
	var out []FieldDef
	for _, f := range fields {
		if f.Tag.Persist || f.Tag.Sync {
			out = append(out, f)
		}
	}
	return out
}

func isNestedType(defs *Definitions, typeName string) bool {
	typeName = strings.TrimPrefix(typeName, "*")
	for _, n := range defs.Nested {
		if n.Name == typeName {
			return true
		}
	}
	return false
}

// nestedFieldOrdinal numbers a nested struct's fields from 1. It identifies
// which FIELD of a parent holds a child, which is half of the child's owner
// token; a nested struct has no dirty-mask constants of its own, so the
// position in the definition is the stable name. It is stable because
// generation is a function of the definition file.
func nestedFieldOrdinal(nested NestedDef, fieldName string) uint64 {
	for i, field := range nested.Fields {
		if field.Name == fieldName {
			return uint64(i + 1)
		}
	}
	return 0
}

func lower1(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// goKeywords are the words a private field name must not be. They are the
// only names that BREAK — a struct field called `string` or `len` is legal Go
// and merely shadows a predeclared identifier inside its own scope, while
// `type` or `range` does not parse (U-0246). The failure was invisible until
// a definition used one, and then it surfaced as `expected '}', found 'type'`
// pointing at a generated temporary file.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// safeFieldVarName is fieldVarName with the keyword collision resolved. The
// suffix goes on the PRIVATE name only: accessors, BSON keys and dirty-mask
// constants keep the field's own spelling, so nothing a caller writes changes.
func safeFieldVarName(name string) string {
	varName := fieldVarName(name)
	if goKeywords[varName] {
		return varName + "Value"
	}
	return varName
}

func fieldVarName(name string) string {
	if name == "" {
		return ""
	}
	runes := []rune(name)
	end := 1
	for end < len(runes) && unicode.IsUpper(runes[end]) {
		if end+1 < len(runes) && unicode.IsLower(runes[end+1]) {
			break
		}
		end++
	}
	for i := 0; i < end; i++ {
		runes[i] = unicode.ToLower(runes[i])
	}
	return string(runes)
}

type daoTmplData struct {
	Package string
	Dao     DaoDef
	Defs    *Definitions
	HasMaps bool
}

type nestedTmplData struct {
	Package string
	Nested  NestedDef
}

type redisDaoTmplData struct {
	Package       string
	Dao           RedisDaoDef
	DefaultPrefix string
	RedisName     string
	DefaultTTL    string
}

func redisDaoDefaultPrefix(dao RedisDaoDef) string {
	if dao.Prefix != "" {
		return dao.Prefix
	}
	return "roost:redisdao"
}

func redisDaoName(dao RedisDaoDef) string {
	if dao.RedisName != "" {
		return dao.RedisName
	}
	return toSnake(dao.Name)
}

func redisDaoDefaultTTL(dao RedisDaoDef) (string, error) {
	if dao.TTL == "" {
		return "0", nil
	}
	ttl, err := time.ParseDuration(dao.TTL)
	if err != nil {
		return "", fmt.Errorf("redis dao %s has invalid ttl %q: %w", dao.Name, dao.TTL, err)
	}
	return "time.Duration(" + strconv.FormatInt(int64(ttl), 10) + ")", nil
}
