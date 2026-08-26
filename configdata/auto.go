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
	name     string
	file     string
	validate any // func(*BuildContext, V) error, typed at use site
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

// autoScalarKinds are the reflect kinds usable as keys and ref fields:
// integers and strings, matching cfggen's schema whitelist. bool and floats
// are rejected — a bool key has two possible rows and a float ref invites
// silent truncation.
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
	return RegisterTable(r, TableDef[K, V]{
		Name:          Name(name),
		File:          file,
		Key:           spec.key,
		Indexes:       spec.indexes,
		ValidateTable: spec.validateRefTargets,
		Validate: func(ctx *BuildContext, row V) error {
			if err := spec.checkRefs(ctx, row); err != nil {
				return err
			}
			if userValidate != nil {
				return userValidate(ctx, row)
			}
			return nil
		},
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

// autoField is one struct field with its promotion path (embedded structs
// are flattened the way encoding/json flattens them).
type autoField struct {
	index []int
	field reflect.StructField
}

func collectAutoFields(t reflect.Type, prefix []int) ([]autoField, error) {
	var out []autoField
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		index := append(append([]int(nil), prefix...), i)
		if field.Anonymous {
			if _, tagged := field.Tag.Lookup("cfg"); tagged {
				return nil, fmt.Errorf("cfg tag on embedded field %s is not supported (tag the promoted fields instead)", field.Name)
			}
			if field.Type.Kind() == reflect.Struct {
				nested, err := collectAutoFields(field.Type, index)
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
		out = append(out, autoField{index: index, field: field})
	}
	return out, nil
}

func fieldsHaveCfgTag(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if _, ok := field.Tag.Lookup("cfg"); ok {
			return true
		}
		if field.Anonymous && field.Type.Kind() == reflect.Struct && fieldsHaveCfgTag(field.Type) {
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
	fields, err := collectAutoFields(valueType, nil)
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
		var indexName string
		hasIndex := false
		skipEmpty := false
		for _, directive := range strings.Split(tag, ",") {
			directive = strings.TrimSpace(directive)
			switch {
			case directive == "key":
				if keyPath != nil {
					return nil, fmt.Errorf("configdata: auto table %s: multiple cfg:\"key\" fields (%s and %s)", valueType.String(), keyName, field.Name)
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
				// consumed below together with ref
			case strings.HasPrefix(directive, "ref="):
				target := strings.TrimPrefix(directive, "ref=")
				if target == "" {
					return nil, fmt.Errorf("configdata: auto table %s: field %s: empty ref target", valueType.String(), field.Name)
				}
				if !autoScalarKind(field.Type.Kind()) {
					return nil, fmt.Errorf("configdata: auto table %s: ref field %s must be an integer or string, got %s", valueType.String(), field.Name, field.Type)
				}
				spec.refs = append(spec.refs, autoRef{
					fieldIndex: entry.index,
					fieldName:  field.Name,
					fieldType:  field.Type,
					table:      Name(target),
					required:   strings.Contains(tag, "required"),
				})
			default:
				return nil, fmt.Errorf("configdata: auto table %s: field %s: unknown cfg directive %q", valueType.String(), field.Name, directive)
			}
		}
		if skipEmpty && !hasIndex {
			return nil, fmt.Errorf("configdata: auto table %s: field %s: skipempty requires index", valueType.String(), field.Name)
		}
		if strings.Contains(tag, "required") && !strings.Contains(tag, "ref=") {
			return nil, fmt.Errorf("configdata: auto table %s: field %s: required requires ref", valueType.String(), field.Name)
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
		return nil, fmt.Errorf("configdata: auto table %s: no cfg:\"key\" field", valueType.String())
	}
	spec.key = func(v V) K {
		return reflect.ValueOf(v).FieldByIndex(keyPath).Interface().(K)
	}
	return spec, nil
}

// validateRefTargets runs once per table build: every ref target must exist
// as a registered table with a key type the ref field can address. This
// fires even for empty tables and all-zero columns, so a misspelled target
// name cannot hide until the first non-zero value shows up in production.
func (s *autoSpec[K, V]) validateRefTargets(ctx *BuildContext, _ *Table[K, V]) error {
	for _, ref := range s.refs {
		lookup, err := refTargetLookup(ctx, ref.table, ref.fieldName)
		if err != nil {
			return err
		}
		targetKey := lookup.refKeyType()
		if !refTypeCompatible(ref.fieldType, targetKey) {
			return fmt.Errorf("field %s: ref type %s is not compatible with %s key type %s", ref.fieldName, ref.fieldType, ref.table, targetKey)
		}
	}
	return nil
}

// checkRefs enforces membership per row: a non-zero value must exist as a
// key in the target table. Zero values mean "no reference" unless the ref
// is tagged required.
func (s *autoSpec[K, V]) checkRefs(ctx *BuildContext, row V) error {
	if len(s.refs) == 0 {
		return nil
	}
	value := reflect.ValueOf(row)
	for _, ref := range s.refs {
		field := value.FieldByIndex(ref.fieldIndex)
		if field.IsZero() {
			if ref.required {
				return fmt.Errorf("field %s: required reference to %s is zero", ref.fieldName, ref.table)
			}
			continue
		}
		lookup, err := refTargetLookup(ctx, ref.table, ref.fieldName)
		if err != nil {
			return err
		}
		if !lookup.containsKeyValue(field) {
			return fmt.Errorf("field %s references missing %s key %v", ref.fieldName, ref.table, field.Interface())
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
	typeName = strings.TrimSuffix(typeName, "Config")
	typeName = strings.TrimSuffix(typeName, "Cfg")
	return snakeCase(typeName)
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
