// cfggen generates roost-core/configdata bindings from a schema meta file —
// a deliberately small, Luban-like pipeline: the meta file is the single
// hand-written artifact, everything Go (row structs, registration, typed
// snapshot accessors) is generated.
//
//	roost-codegen cfggen -meta ./configs/schema/cfg.yaml -out ./cfg
//
// Meta format (YAML):
//
//	package: cfg                 # generated package name (-pkg overrides)
//	beans:                       # nested struct definitions
//	  - name: DropItem
//	    fields:
//	      - { name: item_id, type: int32 }
//	      - { name: weight,  type: int32 }
//	tables:
//	  - name: monster            # snake_case; data file defaults <name>.json
//	    key: id
//	    fields:
//	      - { name: id,       type: int32 }
//	      - { name: name,     type: string }
//	      - { name: scene_id, type: int32, index: true }
//	      - { name: drop_id,  type: int32, ref: drop }
//	      - { name: rewards,  type: "[]DropItem" }
//	globals:                     # 无主键的全局单例配置（单 JSON 文档）
//	  - name: world              # "objects" 为兼容别名
//	    fields:
//	      - { name: width, type: int32 }
//
// Field options: index: true (index named after the field) or index: <name>;
// ref: <table> (non-zero values must exist as keys of <table>, enforced at
// load time by configdata's auto-table reference check). Scalar types:
// int32/int64/uint32/uint64/float32/float64/string/bool; []scalar, bean and
// []bean compose from those.
package cfggen

import (
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const generatedFileName = "cfg_gen.go"

type Meta struct {
	Package string `yaml:"package"`
	// Groups declares export groups, Luban-style: every table, global and
	// field may name the groups it belongs to, and one generation run exports
	// one target set. A config shared by client and server keeps one meta;
	// the server binding simply never sees client-only fields.
	Groups GroupsMeta  `yaml:"groups"`
	Beans  []BeanMeta  `yaml:"beans"`
	Tables []TableMeta `yaml:"tables"`
	// Globals are keyless singleton configs: the data file is one JSON
	// document, the snapshot holds exactly one value (world size, global
	// switches, formula constants). "objects" is the deprecated alias.
	Globals []TableMeta `yaml:"globals"`
	Objects []TableMeta `yaml:"objects"`
}

// globalEntries merges the globals section with its deprecated objects alias.
func (m *Meta) globalEntries() []TableMeta {
	return append(append([]TableMeta(nil), m.Globals...), m.Objects...)
}

// GroupsMeta is the top-level export-group declaration.
type GroupsMeta struct {
	// Names lists every group a `group:` may reference (typo protection).
	Names []string `yaml:"names"`
	// Target is the set exported by default; the -groups flag overrides it.
	// Empty means "export everything", which keeps metas without groups
	// generating exactly what they did before.
	Target GroupList `yaml:"target"`
}

// GroupList accepts `group: s` as well as `group: [c, s]`.
type GroupList []string

func (g *GroupList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var one string
		if err := value.Decode(&one); err != nil {
			return err
		}
		*g = GroupList{one}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := value.Decode(&many); err != nil {
			return err
		}
		*g = GroupList(many)
		return nil
	default:
		return fmt.Errorf("group must be a name or a list of names")
	}
}

type BeanMeta struct {
	Name    string      `yaml:"name"`
	Comment string      `yaml:"comment"`
	Fields  []FieldMeta `yaml:"fields"`
}

type TableMeta struct {
	Name    string      `yaml:"name"`
	File    string      `yaml:"file"`
	Key     string      `yaml:"key"`
	Comment string      `yaml:"comment"`
	Fields  []FieldMeta `yaml:"fields"`
	// Group restricts the whole entry to these export groups; absent = all.
	Group GroupList `yaml:"group"`
}

type FieldMeta struct {
	Name  string `yaml:"name"`
	Type  string `yaml:"type"`
	Index any    `yaml:"index"` // true or an explicit index name
	Ref   string `yaml:"ref"`
	// Required (with ref): the zero value is an error too — catches a whole
	// column silently zeroing after a data-side field rename.
	Required bool `yaml:"required"`
	// SkipEmpty (with index): zero values stay out of the index.
	SkipEmpty bool   `yaml:"skipempty"`
	Comment   string `yaml:"comment"`
	// Group restricts the field to these export groups; absent = all. A
	// client-only field (`group: c`) is left out of the server binding, the
	// way Luban's group attribute filters a target.
	Group GroupList `yaml:"group"`
}

var scalarTypes = map[string]bool{
	"int32": true, "int64": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "string": true, "bool": true,
}

var keyTypes = map[string]bool{
	"int32": true, "int64": true, "uint32": true, "uint64": true, "string": true,
}

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("cfggen", flag.ContinueOnError)
	flags.SetOutput(stdout)
	metaPath := flags.String("meta", "./configs/schema/cfg.yaml", "schema meta file")
	outDir := flags.String("out", "./cfg", "generated Go output directory")
	outPkg := flags.String("pkg", "", "generated package name (default: meta 'package', else output directory base)")
	groupsFlag := flags.String("groups", "", "comma-separated export groups to generate for (default: meta groups.target, else everything)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	raw, err := os.ReadFile(*metaPath)
	if err != nil {
		return fmt.Errorf("cfggen: read meta: %w", err)
	}
	var meta Meta
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true) // typos in the meta fail loudly, Luban-style
	if err := decoder.Decode(&meta); err != nil {
		return fmt.Errorf("cfggen: parse meta %s: %w", *metaPath, err)
	}
	if err := validateMeta(&meta); err != nil {
		return fmt.Errorf("cfggen: %s: %w", *metaPath, err)
	}
	exported, omitted, err := exportForGroups(&meta, splitGroups(*groupsFlag))
	if err != nil {
		return fmt.Errorf("cfggen: %s: %w", *metaPath, err)
	}
	meta = *exported

	pkg := *outPkg
	if pkg == "" {
		pkg = meta.Package
	}
	if pkg == "" {
		pkg = sanitizeIdentifier(filepath.Base(*outDir))
	}

	source, err := generate(&meta, pkg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(*outDir, generatedFileName)
	if err := os.WriteFile(target, source, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "cfggen: wrote %s (%d tables, %d globals, %d beans)\n",
		target, len(meta.Tables), len(meta.globalEntries()), len(meta.Beans))
	if omitted.entries+omitted.fields > 0 {
		_, _ = fmt.Fprintf(stdout, "cfggen: export groups %v: omitted %d entries and %d fields belonging to other groups\n",
			omitted.target, omitted.entries, omitted.fields)
	}
	return nil
}

func splitGroups(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type omittedByGroups struct {
	target  []string
	entries int
	fields  int
}

// exportForGroups returns the meta restricted to one target group set. Group
// names are checked against the declaration, a key field can never be
// excluded (the table would have no identity), and a ref must point at a
// table that is exported too — otherwise the generated binding would refer
// to a table the runtime never registers and every load would fail.
func exportForGroups(meta *Meta, target []string) (*Meta, omittedByGroups, error) {
	if len(target) == 0 {
		target = []string(meta.Groups.Target)
	}
	declared := make(map[string]bool, len(meta.Groups.Names))
	for _, name := range meta.Groups.Names {
		if err := validName("group", name); err != nil {
			return nil, omittedByGroups{}, err
		}
		if declared[name] {
			return nil, omittedByGroups{}, fmt.Errorf("group %q declared twice", name)
		}
		declared[name] = true
	}
	check := func(where string, groups GroupList) error {
		for _, name := range groups {
			if !declared[name] {
				return fmt.Errorf("%s: group %q is not declared in groups.names %v", where, name, meta.Groups.Names)
			}
		}
		return nil
	}
	if err := check("groups.target", GroupList(target)); err != nil {
		return nil, omittedByGroups{}, err
	}
	targetSet := make(map[string]bool, len(target))
	for _, name := range target {
		targetSet[name] = true
	}
	// No target means every group is exported; the checks still run so a
	// typo in a group name fails even before anyone selects a target.
	exported := func(groups GroupList) bool {
		if len(targetSet) == 0 || len(groups) == 0 {
			return true
		}
		for _, name := range groups {
			if targetSet[name] {
				return true
			}
		}
		return false
	}
	omitted := omittedByGroups{target: target}
	filterFields := func(where string, fields []FieldMeta, key string) ([]FieldMeta, error) {
		out := make([]FieldMeta, 0, len(fields))
		for _, field := range fields {
			if err := check(where+" field "+field.Name, field.Group); err != nil {
				return nil, err
			}
			if !exported(field.Group) {
				if key != "" && field.Name == key {
					return nil, fmt.Errorf("%s: key field %s cannot be excluded from target groups %v", where, field.Name, target)
				}
				omitted.fields++
				continue
			}
			out = append(out, field)
		}
		return out, nil
	}
	result := &Meta{Package: meta.Package, Groups: meta.Groups}
	for _, bean := range meta.Beans {
		fields, err := filterFields("bean "+bean.Name, bean.Fields, "")
		if err != nil {
			return nil, omittedByGroups{}, err
		}
		if len(fields) == 0 {
			return nil, omittedByGroups{}, fmt.Errorf("bean %s: every field is excluded from target groups %v", bean.Name, target)
		}
		bean.Fields = fields
		result.Beans = append(result.Beans, bean)
	}
	exportedTables := make(map[string]bool, len(meta.Tables))
	for _, table := range meta.Tables {
		if err := check("table "+table.Name, table.Group); err != nil {
			return nil, omittedByGroups{}, err
		}
		if !exported(table.Group) {
			omitted.entries++
			continue
		}
		fields, err := filterFields("table "+table.Name, table.Fields, table.Key)
		if err != nil {
			return nil, omittedByGroups{}, err
		}
		table.Fields = fields
		exportedTables[table.Name] = true
		result.Tables = append(result.Tables, table)
	}
	for _, table := range result.Tables {
		for _, field := range table.Fields {
			if field.Ref != "" && !exportedTables[field.Ref] {
				return nil, omittedByGroups{}, fmt.Errorf("table %s field %s: ref target %q is excluded from target groups %v", table.Name, field.Name, field.Ref, target)
			}
		}
	}
	for _, global := range meta.globalEntries() {
		if err := check("global "+global.Name, global.Group); err != nil {
			return nil, omittedByGroups{}, err
		}
		if !exported(global.Group) {
			omitted.entries++
			continue
		}
		fields, err := filterFields("global "+global.Name, global.Fields, "")
		if err != nil {
			return nil, omittedByGroups{}, err
		}
		if len(fields) == 0 {
			return nil, omittedByGroups{}, fmt.Errorf("global %s: every field is excluded from target groups %v", global.Name, target)
		}
		global.Fields = fields
		result.Globals = append(result.Globals, global)
	}
	return result, omitted, nil
}

func validateMeta(meta *Meta) error {
	// identifiers is the registry of every top-level Go identifier the
	// generator will emit; any collision (my_table vs myTable, bean vs row
	// struct, table accessor vs global accessor) is caught here instead of
	// surfacing as a duplicate-declaration compile error downstream.
	identifiers := map[string]string{
		"RegisterGeneratedConfigData":     "generated function",
		"MustRegisterGeneratedConfigData": "generated function",
	}
	claim := func(id, source string) error {
		if prev, exists := identifiers[id]; exists {
			return fmt.Errorf("generated identifier %s from %s collides with %s", id, source, prev)
		}
		identifiers[id] = source
		return nil
	}

	beanNames := make(map[string]bool, len(meta.Beans))
	for _, bean := range meta.Beans {
		if err := validName("bean", bean.Name); err != nil {
			return err
		}
		// Bean names are emitted verbatim as Go type names.
		if goKeywords[bean.Name] {
			return fmt.Errorf("bean name %q is a Go keyword", bean.Name)
		}
		if goPredeclared[bean.Name] {
			return fmt.Errorf("bean name %q shadows a predeclared Go identifier", bean.Name)
		}
		if err := claim(bean.Name, "bean "+bean.Name); err != nil {
			return err
		}
		beanNames[bean.Name] = true
	}
	tableKeyType := make(map[string]string, len(meta.Tables))
	seen := make(map[string]bool)
	for _, table := range meta.Tables {
		if err := validateEntry("table", table, beanNames, seen); err != nil {
			return err
		}
		if table.Key == "" {
			return fmt.Errorf("table %s: key is required", table.Name)
		}
		keyField, ok := fieldByName(table.Fields, table.Key)
		if !ok {
			return fmt.Errorf("table %s: key field %q not declared", table.Name, table.Key)
		}
		if !keyTypes[keyField.Type] {
			return fmt.Errorf("table %s: key field %s type %s not usable as key (integer or string)", table.Name, table.Key, keyField.Type)
		}
		tableKeyType[table.Name] = keyField.Type
		source := "table " + table.Name
		if err := claim(typeName(table.Name), source); err != nil {
			return err
		}
		if err := claim(exportName(table.Name)+"TableFrom", source); err != nil {
			return err
		}
		for _, field := range table.Fields {
			enabled, name, err := indexSpec(field)
			if err != nil {
				return fmt.Errorf("table %s: %w", table.Name, err)
			}
			if !enabled {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if err := validName("index", name); err != nil {
				return fmt.Errorf("table %s: %w", table.Name, err)
			}
			if err := claim(exportName(table.Name)+"By"+exportName(name), source+" index "+name); err != nil {
				return err
			}
		}
	}
	for _, global := range meta.globalEntries() {
		if err := validateEntry("global", global, beanNames, seen); err != nil {
			return err
		}
		if global.Key != "" {
			return fmt.Errorf("global %s: singleton configs have no key", global.Name)
		}
		for _, field := range global.Fields {
			// index/ref on a global would generate cfg tags that the object
			// registration path never evaluates — a validation the user
			// believes in but that never runs. Reject instead of ignoring.
			if field.Index != nil {
				return fmt.Errorf("global %s field %s: singleton configs do not support index", global.Name, field.Name)
			}
			if field.Ref != "" {
				return fmt.Errorf("global %s field %s: singleton configs do not support ref", global.Name, field.Name)
			}
		}
		source := "global " + global.Name
		if err := claim(typeName(global.Name), source); err != nil {
			return err
		}
		if err := claim(exportName(global.Name)+"From", source); err != nil {
			return err
		}
	}
	for _, bean := range meta.Beans {
		fieldSeen := make(map[string]bool, len(bean.Fields))
		exportSeen := make(map[string]string, len(bean.Fields))
		for _, field := range bean.Fields {
			if err := validName("field", field.Name); err != nil {
				return fmt.Errorf("bean %s: %w", bean.Name, err)
			}
			if fieldSeen[field.Name] {
				return fmt.Errorf("bean %s: duplicate field %s", bean.Name, field.Name)
			}
			fieldSeen[field.Name] = true
			if prev, dup := exportSeen[exportName(field.Name)]; dup {
				return fmt.Errorf("bean %s: fields %s and %s map to the same Go field %s", bean.Name, prev, field.Name, exportName(field.Name))
			}
			exportSeen[exportName(field.Name)] = field.Name
			if err := validateFieldType(field, beanNames); err != nil {
				return fmt.Errorf("bean %s: %w", bean.Name, err)
			}
			if field.Ref != "" || field.Index != nil {
				return fmt.Errorf("bean %s field %s: ref/index only apply to table fields", bean.Name, field.Name)
			}
		}
	}
	if err := checkBeanCycles(meta.Beans); err != nil {
		return err
	}
	// Cross-table refs: target exists and the field type matches its key.
	for _, table := range meta.Tables {
		for _, field := range table.Fields {
			if field.Ref == "" {
				continue
			}
			targetKey, ok := tableKeyType[field.Ref]
			if !ok {
				return fmt.Errorf("table %s field %s: ref target %q is not a declared table", table.Name, field.Name, field.Ref)
			}
			if field.Type != targetKey {
				return fmt.Errorf("table %s field %s: ref type %s does not match %s key type %s", table.Name, field.Name, field.Type, field.Ref, targetKey)
			}
		}
	}
	return nil
}

// checkBeanCycles rejects bean graphs that would generate invalid recursive
// Go types. Only non-slice edges count: `type Node struct{ Children []Node }`
// is legal Go, a direct `child Node` field is not.
func checkBeanCycles(beans []BeanMeta) error {
	edges := make(map[string][]string, len(beans))
	byName := make(map[string]bool, len(beans))
	for _, bean := range beans {
		byName[bean.Name] = true
	}
	for _, bean := range beans {
		for _, field := range bean.Fields {
			if strings.HasPrefix(field.Type, "[]") {
				continue
			}
			if byName[field.Type] {
				edges[bean.Name] = append(edges[bean.Name], field.Type)
			}
		}
	}
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(beans))
	var visit func(string, []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("bean cycle through non-slice fields: %s -> %s (use a slice or restructure)", strings.Join(path, " -> "), name)
		}
		state[name] = visiting
		for _, next := range edges[name] {
			if err := visit(next, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for _, bean := range beans {
		if err := visit(bean.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

func validateEntry(kind string, entry TableMeta, beans map[string]bool, seen map[string]bool) error {
	if err := validName(kind, entry.Name); err != nil {
		return err
	}
	if seen[entry.Name] {
		return fmt.Errorf("duplicate %s name %s", kind, entry.Name)
	}
	seen[entry.Name] = true
	if len(entry.Fields) == 0 {
		return fmt.Errorf("%s %s: no fields", kind, entry.Name)
	}
	if entry.File != "" && (filepath.IsAbs(entry.File) || !filepath.IsLocal(entry.File)) {
		return fmt.Errorf("%s %s: file %q escapes the data directory", kind, entry.Name, entry.File)
	}
	fieldSeen := make(map[string]bool, len(entry.Fields))
	exportSeen := make(map[string]string, len(entry.Fields))
	for _, field := range entry.Fields {
		if err := validName("field", field.Name); err != nil {
			return fmt.Errorf("%s %s: %w", kind, entry.Name, err)
		}
		if fieldSeen[field.Name] {
			return fmt.Errorf("%s %s: duplicate field %s", kind, entry.Name, field.Name)
		}
		fieldSeen[field.Name] = true
		if prev, dup := exportSeen[exportName(field.Name)]; dup {
			return fmt.Errorf("%s %s: fields %s and %s map to the same Go field %s", kind, entry.Name, prev, field.Name, exportName(field.Name))
		}
		exportSeen[exportName(field.Name)] = field.Name
		if err := validateFieldType(field, beans); err != nil {
			return fmt.Errorf("%s %s: %w", kind, entry.Name, err)
		}
		enabled, _, err := indexSpec(field)
		if err != nil {
			return fmt.Errorf("%s %s: %w", kind, entry.Name, err)
		}
		if enabled {
			if !scalarTypes[field.Type] || field.Type == "float32" || field.Type == "float64" {
				return fmt.Errorf("%s %s field %s: index requires a string/integer/bool field", kind, entry.Name, field.Name)
			}
		}
		if field.Ref != "" && !scalarTypes[field.Type] {
			return fmt.Errorf("%s %s field %s: ref requires a scalar field", kind, entry.Name, field.Name)
		}
		if field.Required && field.Ref == "" {
			return fmt.Errorf("%s %s field %s: required requires ref", kind, entry.Name, field.Name)
		}
		if field.SkipEmpty && !enabled {
			return fmt.Errorf("%s %s field %s: skipempty requires index", kind, entry.Name, field.Name)
		}
	}
	return nil
}

func validateFieldType(field FieldMeta, beans map[string]bool) error {
	base := strings.TrimPrefix(field.Type, "[]")
	if scalarTypes[base] || beans[base] {
		return nil
	}
	return fmt.Errorf("field %s: unknown type %q (scalars, []scalar, declared beans, []bean)", field.Name, field.Type)
}

func fieldByName(fields []FieldMeta, name string) (FieldMeta, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return FieldMeta{}, false
}

func generate(meta *Meta, pkg string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("// Code generated by roost-codegen cfggen. DO NOT EDIT.\n")
	b.WriteString("//\n// Source: the cfggen schema meta file. Regenerate with:\n")
	b.WriteString("//\n//\tcfggen -meta <schema> -out <this directory>\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	needStrconv := false
	for _, table := range meta.Tables {
		for _, field := range table.Fields {
			if field.Index != nil && field.Type != "string" {
				needStrconv = true
			}
		}
	}
	if needStrconv {
		b.WriteString("import (\n\t\"strconv\"\n\n\t\"github.com/tjbdwanghaibo/roost-core/configdata\"\n)\n\n")
	} else {
		b.WriteString("import (\n\t\"github.com/tjbdwanghaibo/roost-core/configdata\"\n)\n\n")
	}

	for _, bean := range meta.Beans {
		writeStruct(&b, bean.Name, bean.Comment, bean.Fields, "", nil)
	}
	for _, table := range meta.Tables {
		writeStruct(&b, typeName(table.Name), commentOr(table.Comment, "one row of table "+table.Name), table.Fields, table.Key, &table)
	}
	for _, global := range meta.globalEntries() {
		writeStruct(&b, typeName(global.Name), commentOr(global.Comment, "the "+global.Name+" global singleton config"), global.Fields, "", nil)
	}

	// Registration: one call wires every generated definition.
	b.WriteString("// RegisterGeneratedConfigData registers every table and object defined in\n")
	b.WriteString("// the schema meta file on the given registry.\n")
	b.WriteString("func RegisterGeneratedConfigData(r *configdata.Registry) error {\n")
	for _, table := range meta.Tables {
		keyField, _ := fieldByName(table.Fields, table.Key)
		fmt.Fprintf(&b, "\tif err := configdata.RegisterAutoTable[%s, %s](r, configdata.WithAutoName(%q), configdata.WithAutoFile(%q)); err != nil {\n\t\treturn err\n\t}\n",
			keyField.Type, typeName(table.Name), table.Name, fileName(table))
	}
	for _, global := range meta.globalEntries() {
		fmt.Fprintf(&b, "\tif err := configdata.RegisterObject(r, configdata.ObjectDef[%s]{Name: %q, File: %q}); err != nil {\n\t\treturn err\n\t}\n",
			typeName(global.Name), global.Name, fileName(global))
	}
	b.WriteString("\treturn nil\n}\n\n")
	b.WriteString("// MustRegisterGeneratedConfigData is RegisterGeneratedConfigData panicking on error.\n")
	b.WriteString("func MustRegisterGeneratedConfigData(r *configdata.Registry) {\n")
	b.WriteString("\tif err := RegisterGeneratedConfigData(r); err != nil {\n\t\tpanic(err)\n\t}\n}\n\n")

	// The aggregate in internal/registry calls registrations without
	// arguments, so the marked entry point is this wrapper over the default
	// registry. RegisterGeneratedConfigData stays exported and explicit for
	// tests and for services that build their own registry.
	b.WriteString("// RegisterConfigData registers the generated definitions on the default\n")
	b.WriteString("// registry. This is the entry point the generated aggregate calls.\n")
	b.WriteString("//\n")
	b.WriteString("//roost:register phase=config\n")
	b.WriteString("func RegisterConfigData() error {\n")
	b.WriteString("\treturn RegisterGeneratedConfigData(configdata.DefaultRegistry())\n}\n\n")

	// Typed snapshot accessors.
	for _, table := range meta.Tables {
		keyField, _ := fieldByName(table.Fields, table.Key)
		fmt.Fprintf(&b, "// %sTableFrom returns table %s from a snapshot.\n", exportName(table.Name), table.Name)
		fmt.Fprintf(&b, "func %sTableFrom(snap *configdata.Snapshot) (*configdata.Table[%s, %s], bool) {\n\treturn configdata.TableFrom[%s, %s](snap, %q)\n}\n\n",
			exportName(table.Name), keyField.Type, typeName(table.Name), keyField.Type, typeName(table.Name), table.Name)
		for _, field := range table.Fields {
			enabled, explicit, _ := indexSpec(field)
			if !enabled {
				continue
			}
			indexName := explicit
			if indexName == "" {
				indexName = field.Name
			}
			accessor := exportName(table.Name) + "By" + exportName(indexName)
			param := paramName(field.Name)
			fmt.Fprintf(&b, "// %s returns the %s rows whose %s equals %s.\n", accessor, table.Name, field.Name, param)
			fmt.Fprintf(&b, "func %s(snap *configdata.Snapshot, %s %s) []%s {\n", accessor, param, field.Type, typeName(table.Name))
			fmt.Fprintf(&b, "\ttable, ok := %sTableFrom(snap)\n\tif !ok {\n\t\treturn nil\n\t}\n", exportName(table.Name))
			fmt.Fprintf(&b, "\treturn table.GetByIndex(%q, %s)\n}\n\n", indexName, indexValueExpr(field.Type, param))
		}
	}
	for _, global := range meta.globalEntries() {
		fmt.Fprintf(&b, "// %sFrom returns the %s global singleton config from a snapshot.\n", exportName(global.Name), global.Name)
		fmt.Fprintf(&b, "func %sFrom(snap *configdata.Snapshot) (%s, bool) {\n\treturn configdata.ObjectFrom[%s](snap, %q)\n}\n\n",
			exportName(global.Name), typeName(global.Name), typeName(global.Name), global.Name)
	}

	source, err := format.Source([]byte(b.String()))
	if err != nil {
		// With the meta fully validated this should be unreachable; if it
		// happens anyway, hand back the parse error plus the raw source so
		// the offending construct can actually be found.
		return nil, fmt.Errorf("cfggen: generated code does not parse: %w\n----- unformatted source -----\n%s", err, b.String())
	}
	return source, nil
}

func writeStruct(b *strings.Builder, name, comment string, fields []FieldMeta, keyField string, table *TableMeta) {
	if comment = sanitizeComment(comment); comment != "" {
		fmt.Fprintf(b, "// %s is %s.\n", name, comment)
	}
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, field := range fields {
		var cfgDirectives []string
		if table != nil && field.Name == keyField {
			cfgDirectives = append(cfgDirectives, "key")
		}
		if enabled, explicit, _ := indexSpec(field); enabled {
			if explicit != "" {
				cfgDirectives = append(cfgDirectives, "index="+explicit)
			} else {
				cfgDirectives = append(cfgDirectives, "index")
			}
			if field.SkipEmpty {
				cfgDirectives = append(cfgDirectives, "skipempty")
			}
		}
		if field.Ref != "" {
			cfgDirectives = append(cfgDirectives, "ref="+field.Ref)
			if field.Required {
				cfgDirectives = append(cfgDirectives, "required")
			}
		}
		tag := fmt.Sprintf("`json:%q", field.Name)
		if len(cfgDirectives) > 0 {
			tag += fmt.Sprintf(" cfg:%q", strings.Join(cfgDirectives, ","))
		}
		tag += "`"
		line := fmt.Sprintf("\t%s %s %s", exportName(field.Name), goType(field.Type), tag)
		if fieldComment := sanitizeComment(field.Comment); fieldComment != "" {
			line += " // " + fieldComment
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("}\n\n")
}

// indexValueExpr stringifies a typed index parameter exactly the way the
// runtime auto-table index does, so generated lookups always hit.
func indexValueExpr(fieldType, param string) string {
	switch fieldType {
	case "string":
		return param
	case "bool":
		return "strconv.FormatBool(" + param + ")"
	case "uint32", "uint64":
		return "strconv.FormatUint(uint64(" + param + "), 10)"
	default: // int32/int64 (validate 已限定索引为 string/整数/bool)
		return "strconv.FormatInt(int64(" + param + "), 10)"
	}
}

// paramName converts a snake_case field name to a lowerCamel parameter name
// (scene_id -> sceneID, id -> id). Names that would collide with Go keywords
// or identifiers the generated accessor body uses (snap, table, ok, strconv)
// get an Arg suffix — a field named "type" is one of the most common names
// in game config tables and must generate compilable code.
func paramName(name string) string {
	parts := strings.Split(name, "_")
	var b strings.Builder
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			b.WriteString(part)
			continue
		}
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	out := b.String()
	if goKeywords[out] || reservedParamNames[out] {
		out += "Arg"
	}
	return out
}

// reservedParamNames are identifiers the generated accessor bodies bind.
var reservedParamNames = map[string]bool{
	"snap": true, "table": true, "ok": true, "strconv": true, "configdata": true,
}

func fileName(entry TableMeta) string {
	if entry.File != "" {
		return entry.File
	}
	return entry.Name + ".json"
}

func commentOr(comment, fallback string) string {
	if comment != "" {
		return comment
	}
	return fallback
}

func goType(metaType string) string {
	return metaType // scalar names are Go names; beans are generated types
}

// typeName maps a table/object name to its row struct name: monster ->
// MonsterCfg.
func typeName(name string) string {
	return exportName(name) + "Cfg"
}

// exportName converts snake_case to Go-style PascalCase with an ID
// initialism (scene_id -> SceneID).
func exportName(name string) string {
	parts := strings.Split(name, "_")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// goPredeclared are shadowable predeclared identifiers a generated type name
// must not collide with (type string struct{...} compiles but silently
// shadows the builtin for the whole package).
var goPredeclared = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "any": true, "true": true, "false": true, "nil": true, "iota": true,
}

// validName enforces the character set for every schema name (tables,
// globals, fields, index names): a plain Go identifier. Without this, a
// name like "my-table" or "名字" reaches go/format and the failure blames
// the generator instead of the meta file.
func validName(kind, name string) error {
	if !isGoIdentifier(name) {
		return fmt.Errorf("%s name %q must be a plain Go identifier ([A-Za-z_][A-Za-z0-9_]*)", kind, name)
	}
	return nil
}

// sanitizeComment folds a comment onto one line: a multi-line YAML comment
// would otherwise be spliced verbatim into the generated file, where its
// tail lines land as top-level Go code.
func sanitizeComment(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

// indexSpec resolves the index option: disabled, enabled with the default
// name, or enabled with an explicit name. Any other YAML value is an error —
// in particular `index: false` must disable the index, not enable it.
func indexSpec(field FieldMeta) (enabled bool, name string, err error) {
	switch v := field.Index.(type) {
	case nil:
		return false, "", nil
	case bool:
		return v, "", nil
	case string:
		if v == "" {
			return false, "", fmt.Errorf("field %s: empty index name", field.Name)
		}
		return true, v, nil
	default:
		return false, "", fmt.Errorf("field %s: index must be true/false or a name, got %T", field.Name, field.Index)
	}
}

func isGoIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if i == 0 && !alpha {
			return false
		}
		if !alpha && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "cfg"
	}
	return b.String()
}
