package configdata

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Auto registration: derive the table mapping (key extractor, secondary
// indexes, cross-table reference checks, table/file names) from `cfg` struct
// tags instead of hand-written TableDef plumbing.
//
//	type MonsterCfg struct {
//	    ID      int32  `json:"id"       cfg:"key"`
//	    Name    string `json:"name"`
//	    SceneID int32  `json:"scene_id" cfg:"index"`
//	    DropID  int32  `json:"drop_id"  cfg:"ref=drop"`
//	}
//	configdata.MustRegisterAutoTable[int32, MonsterCfg](reg)
//
// Tag directives (comma separated, combinable):
//
//	key         the primary key field; exactly one, its type must equal K
//	index       secondary index named after the field's json tag (or an
//	            explicit name via index=scene)
//	skipempty   with index: zero values stay out of the index (rows whose
//	            field is "" / 0 / false are not indexed)
//	ref=<name>  every non-zero value must exist as a key in table <name> —
//	            the Luban-style dangling-reference guard, run in-process on
//	            every load/reload. The target's existence and key-type
//	            compatibility are checked once per table (even when the
//	            table is empty), so a misspelled target fails immediately
//	            instead of hiding behind an all-zero column.
//	required    with ref: the zero value is an error too (a zeroed column —
//	            e.g. after a silent field rename — fails loudly)
//
// Fields promoted from embedded (non-pointer) structs participate exactly
// like encoding/json promotes them; a cfg tag on the embedded field itself
// is rejected.
//
// Reflection happens at registration and build time only (both cold paths);
// the read path is the ordinary typed Table.
//
// The table name defaults to the type name with a trailing Cfg/Config
// stripped and converted to snake_case (MonsterCfg -> monster); the file
// defaults to <name>.json. Override via AutoOption.

// AutoOption customizes RegisterAutoTable beyond what tags express.
type AutoOption func(*autoConfig)

type autoConfig struct {
	name          string
	file          string
	validate      any // func(*BuildContext, V) error, typed at use site
	validateTable any // func(*BuildContext, *Table[K, V]) error, typed at use site
}

// WithAutoName overrides the inferred table name.
func WithAutoName(name string) AutoOption {
	return func(c *autoConfig) { c.name = name }
}

// WithAutoFile overrides the inferred data file name.
func WithAutoFile(file string) AutoOption {
	return func(c *autoConfig) { c.file = file }
}

// WithAutoValidate adds a business validation callback (same contract as
// TableDef.Validate); it runs after the tag-derived reference checks.
func WithAutoValidate[V any](validate func(*BuildContext, V) error) AutoOption {
	return func(c *autoConfig) { c.validate = validate }
}

// WithAutoValidateTable adds a whole-table validation callback (same
// contract as TableDef.ValidateTable); it runs after the tag-derived
// reference checks.
func WithAutoValidateTable[K comparable, V any](validate func(*BuildContext, *Table[K, V]) error) AutoOption {
	return func(c *autoConfig) { c.validateTable = validate }
}

// autoScalarKind reports the reflect kinds usable as keys and ref fields:
// integers and strings, matching cfggen's schema whitelist. bool and floats
// are rejected for key/ref — a bool key has two possible rows and a float
// ref invites silent truncation. (Indexes additionally accept bool, see
// indexStringifier.)
func autoScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.String:
		return true
	default:
		return false
	}
}

// RegisterAutoTable registers a JSON table whose mapping is derived from
// `cfg` struct tags on V. All tag mistakes (missing key, key type mismatch,
// unsupported index/ref field kind, malformed directive, duplicate index
// name) fail here, at registration — never at load time.
func RegisterAutoTable[K comparable, V any](r *Registry, opts ...AutoOption) error {
	var cfg autoConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	spec, err := parseAutoSpec[K, V]()
	if err != nil {
		return err
	}
	name := cfg.name
	if name == "" {
		name = spec.defaultName
	}
	file := cfg.file
	if file == "" {
		file = name + ".json"
	}
	var userValidate func(*BuildContext, V) error
	if cfg.validate != nil {
		typed, ok := cfg.validate.(func(*BuildContext, V) error)
		if !ok {
			return fmt.Errorf("configdata: auto table %s: WithAutoValidate callback must be func(*BuildContext, %T) error", name, *new(V))
		}
		userValidate = typed
	}
	var userValidateTable func(*BuildContext, *Table[K, V]) error
	if cfg.validateTable != nil {
		typed, ok := cfg.validateTable.(func(*BuildContext, *Table[K, V]) error)
		if !ok {
			return fmt.Errorf("configdata: auto table %s: WithAutoValidateTable callback signature mismatch", name)
		}
		userValidateTable = typed
	}
	// Reference checks run in ValidateTable: targets are resolved once per
	// build (not once per row × ref) and the target check fires even for
	// empty tables. Validate stays nil unless the caller supplied one — no
	// per-row closure cost for tables without refs.
	var validateTable func(*BuildContext, *Table[K, V]) error
	if len(spec.refs) > 0 || userValidateTable != nil {
		validateTable = func(ctx *BuildContext, table *Table[K, V]) error {
			if err := spec.validateRefs(ctx, table); err != nil {
				return err
			}
			if userValidateTable != nil {
				return userValidateTable(ctx, table)
			}
			return nil
		}
	}
	return RegisterTable(r, TableDef[K, V]{
		Name:          Name(name),
		File:          file,
		Key:           spec.key,
		Indexes:       spec.indexes,
		ValidateTable: validateTable,
		Validate:      userValidate,
	})
}

// MustRegisterAutoTable is RegisterAutoTable panicking on error.
func MustRegisterAutoTable[K comparable, V any](r *Registry, opts ...AutoOption) {
	if err := RegisterAutoTable[K, V](r, opts...); err != nil {
		panic(err)
	}
}

type autoRef struct {
	fieldIndex []int
	fieldName  string
	fieldType  reflect.Type
	table      Name
	required   bool
}

type autoSpec[K comparable, V any] struct {
	defaultName string
	key         func(V) K
	indexes     []IndexDef[V]
	refs        []autoRef
}

// autoField is one struct field with its promotion path and depth (embedded
// structs are flattened the way encoding/json flattens them).
type autoField struct {
	index []int
	depth int
	field reflect.StructField
}

// jsonTagName returns the json tag's name part ("" when absent or empty).
func jsonTagName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

func collectAutoFields(t reflect.Type, prefix []int, depth int) ([]autoField, error) {
	var out []autoField
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		index := append(append([]int(nil), prefix...), i)
		if field.Anonymous {
			if _, tagged := field.Tag.Lookup("cfg"); tagged {
				return nil, fmt.Errorf("cfg tag on embedded field %s is not supported (tag the promoted fields instead)", field.Name)
			}
			// An embedded field with an explicit json name is NOT promoted
			// by encoding/json — it decodes as a nested document. Its inner
			// cfg tags would silently never see data, so reject them.
			if jsonTagName(field) != "" {
				inner := field.Type
				if inner.Kind() == reflect.Pointer {
					inner = inner.Elem()
				}
				if inner.Kind() == reflect.Struct && fieldsHaveCfgTag(inner) {
					return nil, fmt.Errorf("embedded field %s has a json name (not promoted by encoding/json) but carries cfg tags inside", field.Name)
				}
				out = append(out, autoField{index: index, depth: depth, field: field})
				continue
			}
			if field.Type.Kind() == reflect.Struct {
				nested, err := collectAutoFields(field.Type, index, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, nested...)
				continue
			}
			// Pointer embeds are not promoted: FieldByIndex would panic on a
			// nil embed mid-load. Reject tags reachable only through them.
			if field.Type.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Struct {
				if fieldsHaveCfgTag(field.Type.Elem()) {
					return nil, fmt.Errorf("cfg tags inside pointer embed %s are not supported (use a value embed)", field.Name)
				}
			}
			continue
		}
		out = append(out, autoField{index: index, depth: depth, field: field})
	}
	return out, nil
}

// resolveShadowing applies encoding/json's promotion conflict rules to the
// collected fields: for fields sharing a json name the shallowest wins, an
// equal-depth tie drops the whole group. A shadowed (or tie-dropped) field
// carrying a cfg tag is an error — json will never fill it, so its key/ref/
// index would silently operate on a permanently zero column.
func resolveShadowing(fields []autoField) ([]autoField, error) {
	type slot struct {
		winner autoField
		depth  int
		tie    bool
	}
	slots := make(map[string]*slot, len(fields))
	for _, entry := range fields {
		name := jsonFieldName(entry.field)
		current, ok := slots[name]
		if !ok {
			slots[name] = &slot{winner: entry, depth: entry.depth}
			continue
		}
		switch {
		case entry.depth < current.depth:
			if err := rejectShadowedCfgTag(current.winner, entry); err != nil {
				return nil, err
			}
			slots[name] = &slot{winner: entry, depth: entry.depth}
		case entry.depth == current.depth:
			current.tie = true
			if _, tagged := entry.field.Tag.Lookup("cfg"); tagged {
				return nil, fmt.Errorf("fields %s and %s tie on json name %q and are both dropped by encoding/json, but %s carries a cfg tag", current.winner.field.Name, entry.field.Name, name, entry.field.Name)
			}
			if _, tagged := current.winner.field.Tag.Lookup("cfg"); tagged {
				return nil, fmt.Errorf("fields %s and %s tie on json name %q and are both dropped by encoding/json, but %s carries a cfg tag", current.winner.field.Name, entry.field.Name, name, current.winner.field.Name)
			}
		default:
			if err := rejectShadowedCfgTag(entry, current.winner); err != nil {
				return nil, err
			}
		}
	}
	out := make([]autoField, 0, len(fields))
	for _, entry := range fields {
		name := jsonFieldName(entry.field)
		current := slots[name]
		if current.tie {
			continue
		}
		if current.winner.depth == entry.depth && current.winner.field.Name == entry.field.Name && equalIndex(current.winner.index, entry.index) {
			out = append(out, entry)
		}
	}
	return out, nil
}

func rejectShadowedCfgTag(shadowed, winner autoField) error {
	if _, tagged := shadowed.field.Tag.Lookup("cfg"); tagged {
		return fmt.Errorf("field %s is shadowed by %s for json name %q — its cfg tag would silently operate on a never-filled column", shadowed.field.Name, winner.field.Name, jsonFieldName(winner.field))
	}
	return nil
}

func equalIndex(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fieldsHaveCfgTag(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if _, ok := field.Tag.Lookup("cfg"); ok {
			return true
		}
		inner := field.Type
		if inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if field.Anonymous && inner.Kind() == reflect.Struct && fieldsHaveCfgTag(inner) {
			return true
		}
	}
	return false
}

func parseAutoSpec[K comparable, V any]() (*autoSpec[K, V], error) {
	valueType := reflect.TypeOf((*V)(nil)).Elem()
	if valueType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("configdata: auto table value type %s must be a struct", valueType.String())
	}
	keyType := reflect.TypeOf((*K)(nil)).Elem()
	if !autoScalarKind(keyType.Kind()) {
		return nil, fmt.Errorf("configdata: auto table %s: key type %s must be an integer or string", valueType.String(), keyType.String())
	}
	collected, err := collectAutoFields(valueType, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("configdata: auto table %s: %w", valueType.String(), err)
	}
	fields, err := resolveShadowing(collected)
	if err != nil {
		return nil, fmt.Errorf("configdata: auto table %s: %w", valueType.String(), err)
	}
	spec := &autoSpec[K, V]{defaultName: inferTableName(valueType.Name())}
	var keyPath []int
	keyName := ""
	indexNames := make(map[string]string) // index name -> field name
	for _, entry := range fields {
		field := entry.field
		tag, ok := field.Tag.Lookup("cfg")
		if !ok || tag == "" {
			continue
		}
		if !field.IsExported() {
			return nil, fmt.Errorf("configdata: auto table %s: cfg tag on unexported field %s", valueType.String(), field.Name)
		}
		var indexName, refTarget string
		hasIndex, hasRef, hasRequired, skipEmpty := false, false, false, false
		for _, directive := range strings.Split(tag, ",") {
			directive = strings.TrimSpace(directive)
			switch {
			case directive == "key":
				if keyPath != nil {
					return nil, fmt.Errorf("configdata: auto table %s: multiple cfg:%q fields (%s and %s)", valueType.String(), "key", keyName, field.Name)
				}
				if field.Type != keyType {
					return nil, fmt.Errorf("configdata: auto table %s: key field %s is %s, want %s", valueType.String(), field.Name, field.Type, keyType)
				}
				keyPath = entry.index
				keyName = field.Name
			case directive == "index" || strings.HasPrefix(directive, "index="):
				name := strings.TrimPrefix(directive, "index")
				name = strings.TrimPrefix(name, "=")
				if strings.HasPrefix(directive, "index=") && name == "" {
					return nil, fmt.Errorf("configdata: auto table %s: field %s: empty index name", valueType.String(), field.Name)
				}
				if name == "" {
					name = jsonFieldName(field)
				}
				indexName = name
				hasIndex = true
			case directive == "skipempty":
				skipEmpty = true
			case directive == "required":
				hasRequired = true
			case strings.HasPrefix(directive, "ref="):
				if hasRef {
					return nil, fmt.Errorf("configdata: auto table %s: field %s: multiple ref directives", valueType.String(), field.Name)
				}
				refTarget = strings.TrimPrefix(directive, "ref=")
				if refTarget == "" {
					return nil, fmt.Errorf("configdata: auto table %s: field %s: empty ref target", valueType.String(), field.Name)
				}
				if !autoScalarKind(field.Type.Kind()) {
					return nil, fmt.Errorf("configdata: auto table %s: ref field %s must be an integer or string, got %s", valueType.String(), field.Name, field.Type)
				}
				hasRef = true
			default:
				return nil, fmt.Errorf("configdata: auto table %s: field %s: unknown cfg directive %q", valueType.String(), field.Name, directive)
			}
		}
		if skipEmpty && !hasIndex {
			return nil, fmt.Errorf("configdata: auto table %s: field %s: skipempty requires index", valueType.String(), field.Name)
		}
		if hasRequired && !hasRef {
			return nil, fmt.Errorf("configdata: auto table %s: field %s: required requires ref", valueType.String(), field.Name)
		}
		if hasRef {
			spec.refs = append(spec.refs, autoRef{
				fieldIndex: entry.index,
				fieldName:  field.Name,
				fieldType:  field.Type,
				table:      Name(refTarget),
				required:   hasRequired,
			})
		}
		if hasIndex {
			if prev, dup := indexNames[indexName]; dup {
				return nil, fmt.Errorf("configdata: auto table %s: duplicate index name %q (fields %s and %s)", valueType.String(), indexName, prev, field.Name)
			}
			indexNames[indexName] = field.Name
			stringify, err := indexStringifier(field.Type)
			if err != nil {
				return nil, fmt.Errorf("configdata: auto table %s: index field %s: %w", valueType.String(), field.Name, err)
			}
			fieldIndex := entry.index
			skip := skipEmpty
			spec.indexes = append(spec.indexes, IndexDef[V]{
				Name:      indexName,
				SkipEmpty: skipEmpty,
				Key: func(v V) string {
					value := reflect.ValueOf(v).FieldByIndex(fieldIndex)
					if skip && value.IsZero() {
						return "" // SkipEmpty drops the empty string key
					}
					return stringify(value)
				},
			})
		}
	}
	if keyPath == nil {
		return nil, fmt.Errorf("configdata: auto table %s: no cfg key field", valueType.String())
	}
	spec.key = func(v V) K {
		return reflect.ValueOf(v).FieldByIndex(keyPath).Interface().(K)
	}
	return spec, nil
}

// validateRefs runs once per table build: every ref target must exist as a
// registered table with a compatible key type (this fires even for empty
// tables and all-zero columns, so a misspelled target cannot hide until the
// first non-zero value ships), then row membership is checked with the
// lookups resolved exactly once — not once per row × ref.
func (s *autoSpec[K, V]) validateRefs(ctx *BuildContext, table *Table[K, V]) error {
	lookups := make([]refKeyLookup, len(s.refs))
	for i, ref := range s.refs {
		lookup, err := refTargetLookup(ctx, ref.table, ref.fieldName)
		if err != nil {
			return err
		}
		targetKey := lookup.refKeyType()
		if !refTypeCompatible(ref.fieldType, targetKey) {
			return fmt.Errorf("field %s: ref type %s is not compatible with %s key type %s", ref.fieldName, ref.fieldType, ref.table, targetKey)
		}
		lookups[i] = lookup
	}
	for _, row := range table.rows {
		value := reflect.ValueOf(row)
		for i, ref := range s.refs {
			field := value.FieldByIndex(ref.fieldIndex)
			if field.IsZero() {
				if ref.required {
					return fmt.Errorf("field %s: required reference to %s is zero (row key %v)", ref.fieldName, ref.table, s.key(row))
				}
				continue
			}
			if !lookups[i].containsKeyValue(field) {
				return fmt.Errorf("field %s references missing %s key %v", ref.fieldName, ref.table, field.Interface())
			}
		}
	}
	return nil
}

func refTargetLookup(ctx *BuildContext, table Name, fieldName string) (refKeyLookup, error) {
	raw, ok := ctx.Snapshot.table(table)
	if !ok {
		return nil, fmt.Errorf("field %s references unknown table %s", fieldName, table)
	}
	lookup, ok := raw.(refKeyLookup)
	if !ok {
		return nil, fmt.Errorf("field %s: table %s does not support reference lookup", fieldName, table)
	}
	return lookup, nil
}

// refTypeCompatible allows only identical types or named/unnamed pairs of
// the same kind (MyID <-> int32). Cross-kind conversions (int64 -> int32,
// int -> string) silently truncate or rune-convert and would let dangling
// references pass — they are schema errors, not data errors.
func refTypeCompatible(fieldType, keyType reflect.Type) bool {
	if fieldType == keyType {
		return true
	}
	return fieldType.Kind() == keyType.Kind() && fieldType.ConvertibleTo(keyType)
}

// refKeyLookup is the untyped membership probe every *Table implements, so
// reference checks work without knowing the target table's type parameters.
type refKeyLookup interface {
	containsKeyValue(reflect.Value) bool
	refKeyType() reflect.Type
}

func (t *Table[K, V]) refKeyType() reflect.Type {
	return reflect.TypeOf((*K)(nil)).Elem()
}

func (t *Table[K, V]) containsKeyValue(value reflect.Value) bool {
	if t == nil {
		return false
	}
	keyType := reflect.TypeOf((*K)(nil)).Elem()
	if !refTypeCompatible(value.Type(), keyType) {
		return false
	}
	if value.Type() != keyType {
		value = value.Convert(keyType)
	}
	key, ok := value.Interface().(K)
	if !ok {
		return false
	}
	_, ok = t.byKey[key]
	return ok
}

func indexStringifier(t reflect.Type) (func(reflect.Value) string, error) {
	switch t.Kind() {
	case reflect.String:
		return func(v reflect.Value) string { return v.String() }, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(v reflect.Value) string { return strconv.FormatInt(v.Int(), 10) }, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return func(v reflect.Value) string { return strconv.FormatUint(v.Uint(), 10) }, nil
	case reflect.Bool:
		return func(v reflect.Value) string { return strconv.FormatBool(v.Bool()) }, nil
	default:
		return nil, fmt.Errorf("unsupported index field kind %s (string/integer/bool only)", t.Kind())
	}
}

func jsonFieldName(field reflect.StructField) string {
	if tag, ok := field.Tag.Lookup("json"); ok {
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			return name
		}
	}
	return snakeCase(field.Name)
}

// inferTableName maps a config struct type name to its table name:
// MonsterCfg / MonsterConfig -> monster, DropGroup -> drop_group.
func inferTableName(typeName string) string {
	stripped := strings.TrimSuffix(typeName, "Config")
	stripped = strings.TrimSuffix(stripped, "Cfg")
	if stripped == "" {
		// A type literally named Cfg/Config: fall back to the raw name
		// rather than reporting a confusing "table name is empty".
		stripped = typeName
	}
	return snakeCase(stripped)
}

func snakeCase(s string) string {
	var b strings.Builder
	previousLower := false
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			if previousLower {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			previousLower = false
			continue
		}
		b.WriteRune(r)
		previousLower = true
	}
	return b.String()
}
