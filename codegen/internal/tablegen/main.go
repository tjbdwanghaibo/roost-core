// tool/tablegen builds Roost business config artifacts from Go table metadata.
//
// Usage:
//
//	go run ./tool/tablegen -meta ./configs/schema -out ./configs/generated -pkg generated
//	go run ./tool/tablegen -meta ./configs/schema -csv-template ./configs/table_template
//	go run ./tool/tablegen -meta ./configs/schema -csv ./configs/table -json ./configs/data
package tablegen

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/project"
	"github.com/tjbdwanghaibo/roost-core/framework/configdata/rules"
)

const ()

type TableKind string

const (
	KindTable  TableKind = "table"
	KindObject TableKind = "object"
)

type Meta struct {
	Kind       TableKind
	Name       string
	File       string
	JSON       string
	Key        string
	TypeName   string
	Package    string
	ImportPath string
	Alias      string
	Fields     []Field
}

type Field struct {
	Name     string
	Type     string
	CSV      string
	JSON     string
	Title    string
	Required bool
	Unique   bool
	Min      string
	Ref      string
	// Enum lists the allowed values (tag enum:"a|b|c").
	Enum   []string
	Parser string
}

// DefaultMetaDir is where a business project keeps its table meta files;
// `roost generate` refers to it (U-0118).
const DefaultMetaDir = "./configs/schema"

func Run(args []string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	flags := flag.NewFlagSet("tablegen", flag.ContinueOnError)
	flags.SetOutput(stdout)
	metaDir := flags.String("meta", DefaultMetaDir, "table meta root")
	outDir := flags.String("out", "", "generated Go output directory")
	outPkg := flags.String("pkg", "", "generated Go package name (default: detect from output directory)")
	csvTemplateDir := flags.String("csv-template", "", "CSV template output directory")
	csvDir := flags.String("csv", "", "CSV input directory")
	jsonDir := flags.String("json", "", "JSON output/check directory")
	check := flags.Bool("check", false, "check generated JSON files against current table metadata")
	force := flags.Bool("force", false, "overwrite generated files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	metas, err := parseMetaRoot(*metaDir)
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		_, _ = fmt.Fprintln(stdout, "tablegen: no table meta found")
		// Still emit an empty registry when -out is requested. A freshly
		// scaffolded project can then compile before its first business table is
		// defined, and the file is replaced deterministically once metas exist.
		if *outDir == "" && !(*csvDir != "" && *jsonDir != "") && !(*jsonDir != "" && *check) {
			return nil
		}
	}

	if *csvTemplateDir != "" {
		if err := writeCSVTemplates(metas, *csvTemplateDir, *force, stdout); err != nil {
			return err
		}
	}
	if *csvDir != "" && *jsonDir != "" {
		if err := convertCSVToJSON(metas, *csvDir, *jsonDir, *force, stdout); err != nil {
			return err
		}
	}
	if *jsonDir != "" && *check {
		if err := checkJSONFiles(metas, *jsonDir); err != nil {
			return err
		}
	}
	if *outDir != "" {
		if *outPkg == "" {
			info, err := project.Discover(*outDir)
			if err != nil {
				return fmt.Errorf("discover output project: %w", err)
			}
			*outPkg, err = info.PackageName(*outDir)
			if err != nil {
				return fmt.Errorf("detect output package: %w", err)
			}
		}
		if err := generateGo(metas, *outDir, *outPkg, *force, stdout); err != nil {
			return err
		}
	}
	return nil
}

func parseMetaRoot(root string) ([]Meta, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	projectInfo, err := project.Discover(absRoot)
	if err != nil {
		return nil, err
	}
	module := projectInfo.ModulePath
	var metas []Meta
	err = filepath.Walk(absRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileMetas, err := parseMetaFile(projectInfo.Root, module, path)
		if err != nil {
			return err
		}
		metas = append(metas, fileMetas...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	for i := range metas {
		metas[i].Alias = fmt.Sprintf("meta%d", i)
	}
	return metas, nil
}

func parseMetaFile(root string, module string, path string) ([]Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if !marker.Has(text, "table") && !marker.Has(text, "object") {
		return nil, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	// RR-20261006-56: refuse a misspelt option instead of using the default.
	if err := marker.CheckFile(fset, file, marker.Table, marker.Object); err != nil {
		return nil, err
	}

	markers := make(map[int]map[string]string)
	kinds := make(map[int]TableKind)
	for _, group := range file.Comments {
		for _, c := range group.List {
			line := fset.Position(c.Pos()).Line
			txt := strings.TrimSpace(c.Text)
			switch {
			case hasMarker(txt, "table"):
				markers[line] = parseMarkerOptions(cutMarker(txt, "table"))
				kinds[line] = KindTable
			case hasMarker(txt, "object"):
				markers[line] = parseMarkerOptions(cutMarker(txt, "object"))
				kinds[line] = KindObject
			}
		}
	}

	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	importPath := filepath.ToSlash(filepath.Join(module, rel))
	var metas []Meta
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		line := fset.Position(gen.Pos()).Line
		opts, ok := markers[line-1]
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			meta := Meta{
				Kind:       kinds[line-1],
				Name:       optionOr(opts, "name", snake(typeSpec.Name.Name)),
				File:       optionOr(opts, "file", snake(typeSpec.Name.Name)+".csv"),
				JSON:       optionOr(opts, "json", snake(typeSpec.Name.Name)+".json"),
				Key:        opts["key"],
				TypeName:   typeSpec.Name.Name,
				Package:    file.Name.Name,
				ImportPath: importPath,
			}
			for _, field := range st.Fields.List {
				if len(field.Names) == 0 {
					continue
				}
				name := field.Names[0].Name
				if !ast.IsExported(name) {
					continue
				}
				tag := reflect.StructTag("")
				if field.Tag != nil {
					unquoted, _ := strconv.Unquote(field.Tag.Value)
					tag = reflect.StructTag(unquoted)
				}
				csvName := tag.Get("csv")
				if csvName == "" {
					csvName = snake(name)
				}
				jsonName := tag.Get("json")
				if idx := strings.Index(jsonName, ","); idx >= 0 {
					jsonName = jsonName[:idx]
				}
				if jsonName == "" {
					jsonName = name
				}
				meta.Fields = append(meta.Fields, Field{
					Name:     name,
					Type:     types.ExprString(field.Type),
					CSV:      csvName,
					JSON:     jsonName,
					Title:    tag.Get("title"),
					Required: tag.Get("required") == "true",
					Unique:   tag.Get("unique") == "true",
					Min:      tag.Get("min"),
					Ref:      tag.Get("ref"),
					Enum:     splitEnum(tag.Get("enum")),
					Parser:   tag.Get("parser"),
				})
			}
			if meta.Kind == KindTable && meta.Key == "" && len(meta.Fields) > 0 {
				meta.Key = meta.Fields[0].Name
			}
			if meta.Kind == KindTable {
				found := false
				for _, field := range meta.Fields {
					found = found || field.Name == meta.Key
				}
				if !found {
					return nil, fmt.Errorf("%s: table %s key %q is not an exported field", path, meta.TypeName, meta.Key)
				}
			}
			metas = append(metas, meta)
		}
	}
	return metas, nil
}

func parseMarkerOptions(raw string) map[string]string {
	ret := make(map[string]string)
	for _, part := range strings.Fields(raw) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		ret[k] = strings.Trim(v, `"`)
	}
	return ret
}

func writeCSVTemplates(metas []Meta, dir string, force bool, stdout io.Writer) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, meta := range metas {
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		rows := [][]string{
			fieldValues(meta.Fields, func(f Field) string { return f.CSV }),
			fieldValues(meta.Fields, func(f Field) string { return f.Title }),
			fieldValues(meta.Fields, func(f Field) string { return f.Type }),
			fieldValues(meta.Fields, fieldRule),
		}
		for _, row := range rows {
			if err := w.Write(row); err != nil {
				return err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return err
		}
		path := filepath.Join(dir, meta.File)
		if err := writeGenerated(path, buf.Bytes(), force); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "table template: %s\n", path)
	}
	return nil
}

func convertCSVToJSON(metas []Meta, csvDir string, jsonDir string, force bool, stdout io.Writer) error {
	if err := os.MkdirAll(jsonDir, 0755); err != nil {
		return err
	}
	manifestPath := filepath.Join(jsonDir, "_manifest.json")
	previous := tableJSONManifest{}
	if raw, err := os.ReadFile(manifestPath); err == nil {
		if err := json.Unmarshal(raw, &previous); err != nil {
			return fmt.Errorf("read table manifest: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	manifest := tableJSONManifest{Version: 2, Tables: make(map[string]string)}
	type output struct {
		name string
		raw  []byte
	}
	var outputs []output
	for _, meta := range metas {
		if !safeTableJSONName(meta.JSON) {
			return fmt.Errorf("invalid table JSON filename %q", meta.JSON)
		}
		if _, duplicate := manifest.Tables[meta.JSON]; duplicate {
			return fmt.Errorf("duplicate table JSON filename %q", meta.JSON)
		}
		rows, err := readCSVRecords(filepath.Join(csvDir, meta.File), meta)
		if err != nil {
			return err
		}
		var payload any = rows
		if meta.Kind == KindObject {
			if len(rows) > 1 {
				return fmt.Errorf("%s: object %s requires at most one data row, got %d", meta.File, meta.TypeName, len(rows))
			}
			if len(rows) > 0 {
				payload = rows[0]
			} else {
				payload = map[string]any{}
			}
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		outputs = append(outputs, output{meta.JSON, raw})
		manifest.Tables[meta.JSON] = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	// Version 1 did not record ownership. An unrecognized JSON file could be
	// either retired generated data or hand-maintained data; require an explicit
	// decision rather than silently retaining it or deleting it.
	if previous.Version == 1 {
		entries, err := os.ReadDir(jsonDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || name == "_manifest.json" || !strings.HasSuffix(name, ".json") || manifest.Tables[name] != "" {
				continue
			}
			// There is no migrate command: the owner decides. Say so here
			// because this is the only place a v1 project learns about it
			// (RR-20261001-04).
			return fmt.Errorf("untracked table JSON %s from legacy manifest (v1 recorded no ownership): delete it if an earlier generation produced it; if it is hand-maintained, move it out of %s, run generate once to upgrade _manifest.json to v2, then move it back (codegen/docs/CODEGEN_REFERENCE.zh-CN.md §9)", filepath.Join(jsonDir, name), jsonDir)
		}
	}
	if previous.Version != 0 && previous.Version != 1 && previous.Version != 2 {
		return fmt.Errorf("unsupported table manifest version %d", previous.Version)
	}
	for name, digest := range previous.Tables {
		if manifest.Tables[name] != "" {
			continue
		}
		if !safeTableJSONName(name) {
			return fmt.Errorf("invalid table manifest filename %q", name)
		}
		path := filepath.Join(jsonDir, name)
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != digest {
			return fmt.Errorf("retired table JSON %s was modified; resolve it explicitly", path)
		}
	}
	for _, item := range outputs {
		out := filepath.Join(jsonDir, item.name)
		if err := writeGenerated(out, item.raw, force); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "table json: %s\n", out)
	}
	for name := range previous.Tables {
		if manifest.Tables[name] != "" {
			continue
		}
		path := filepath.Join(jsonDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintf(stdout, "retired table json: %s\n", path)
	}
	manifestRaw, _ := json.MarshalIndent(manifest, "", "  ")
	manifestRaw = append(manifestRaw, '\n')
	return writeGenerated(manifestPath, manifestRaw, true)
}

type tableJSONManifest struct {
	Version     int               `json:"version"`
	GeneratedAt string            `json:"generated_at"`
	Tables      map[string]string `json:"tables"`
}

// ManifestOwnsJSON reports whether jsonDir/_manifest.json still records
// generated JSON files. `roost generate` asks this when configs/table has no
// CSV: only a manifest that owns outputs has something to retire
// (RR-20260930-09). A missing manifest, the scaffold's empty v2 manifest and a
// legacy v1 manifest (which never recorded ownership) all mean the project
// has simply not written its CSVs yet, and running tablegen would fail on the
// first meta's missing CSV (RR-20261001-03).
func ManifestOwnsJSON(jsonDir string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(jsonDir, "_manifest.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var manifest tableJSONManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false, fmt.Errorf("read table manifest: %w", err)
	}
	return len(manifest.Tables) > 0, nil
}

func safeTableJSONName(name string) bool {
	return name != "_manifest.json" && filepath.Base(name) == name && strings.HasSuffix(name, ".json")
}

// checkJSONFiles is `-json <dir> -check`: the JSON a server will load is held
// to the rules the schema declares — the same []rules.Rule the generated
// loader hands configdata (metaRules), checked by the same rules.Check the
// loader runs on every load and reload (B10). It is early feedback only: the
// loader enforces the rules whether or not anybody ran -check. ref needs the
// typed tables and is left to the loader. RR-20261005-NC-75: -check used to
// stop at "is valid JSON".
func checkJSONFiles(metas []Meta, jsonDir string) error {
	for _, meta := range metas {
		path := filepath.Join(jsonDir, meta.JSON)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := checkDocument(meta, raw); err != nil {
			return fmt.Errorf("check %s: %w", path, err)
		}
	}
	return nil
}

// checkDocument reads one data file the way configdata does (rules.Document:
// the rows, or the value of a single rows / records / data wrapper) and runs
// the meta's rules on it.
func checkDocument(meta Meta, raw []byte) error {
	payload, err := rules.Document(raw)
	if err != nil {
		return err
	}
	rows, err := rules.Rows(payload, meta.Kind != KindTable)
	if err != nil {
		if meta.Kind == KindTable {
			return fmt.Errorf("a table file must be a list of rows, or one object holding it under rows / records / data: %w", err)
		}
		return err
	}
	if meta.Kind != KindTable {
		if err := rules.CheckObjectKeys(meta.Name, rows[0], fieldValues(meta.Fields, jsonName)); err != nil {
			return err
		}
		return rules.CheckObject(meta.Name, rows[0], metaRules(meta))
	}
	if err := rules.CheckKeys(meta.Name, rows, fieldValues(meta.Fields, jsonName), rowKeyOf(meta, rows)); err != nil {
		return err
	}
	return checkRows(meta, rows)
}

func jsonName(field Field) string { return field.JSON }

// checkRows runs the meta's rules on table rows, naming a row by its key.
func checkRows(meta Meta, rows []map[string]json.RawMessage) error {
	return rules.Check(meta.Name, rows, metaRules(meta), rowKeyOf(meta, rows))
}

// rowKeyOf names a table row by its key column, as configdata does.
func rowKeyOf(meta Meta, rows []map[string]json.RawMessage) func(int) string {
	if meta.Kind != KindTable {
		return nil
	}
	keyJSON := keyFieldInfo(meta).JSON
	return func(i int) string {
		if value, ok := rules.Lookup(rows[i], keyJSON); ok {
			return rules.Canonical(value)
		}
		return ""
	}
}

// metaRules is the one translation of a meta's tags into rules. It feeds the
// CSV conversion, -check and the Rules literal of the generated loader, so a
// tag edited in the schema changes all three on the next generate (B10). The
// key of a table is unique even without the tag (configdata rejects a
// duplicate key anyway; saying so here reports it as a rule violation).
func metaRules(meta Meta) []rules.Rule {
	var out []rules.Rule
	for _, field := range meta.Fields {
		rule := rules.Rule{
			Field:    field.JSON,
			Required: field.Required,
			Min:      field.Min,
			Enum:     field.Enum,
		}
		if meta.Kind == KindTable {
			rule.Unique = field.Unique || field.Name == keyFieldInfo(meta).Name
			rule.Ref = field.Ref
		}
		if rule.Required || rule.Unique || rule.Min != "" || rule.Ref != "" || len(rule.Enum) > 0 {
			out = append(out, rule)
		}
	}
	return out
}

func splitEnum(tag string) []string {
	if tag == "" {
		return nil
	}
	return strings.Split(tag, "|")
}

func readCSVRecords(path string, meta Meta) ([]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	header := records[0]
	// Column names are case-sensitive, like the JSON keys they become: a
	// header that differs from a field's csv name only in case would
	// otherwise be skipped as an unknown column and its values lost.
	if column, want, found := rules.MisspelledKey(header, fieldValues(meta.Fields, func(f Field) string { return f.CSV })); found {
		return nil, fmt.Errorf("%s: header %q must be spelled %q (column names are case-sensitive)", filepath.Base(path), column, want)
	}
	fieldByCSV := make(map[string]Field, len(meta.Fields))
	for _, field := range meta.Fields {
		fieldByCSV[field.CSV] = field
	}
	start := csvDataStart(records, meta)
	out := make([]map[string]any, 0, len(records)-start)
	for rowIndex := start; rowIndex < len(records); rowIndex++ {
		record := records[rowIndex]
		if isEmptyRow(record) {
			continue
		}
		row := make(map[string]any, len(header))
		for colIndex, name := range header {
			field, ok := fieldByCSV[name]
			if !ok {
				continue
			}
			value := ""
			if colIndex < len(record) {
				value = strings.TrimSpace(record[colIndex])
			}
			parsed, err := parseCell(value, field)
			if err != nil {
				return nil, fmt.Errorf("%s row=%d col=%s field=%s value=%q: %w", filepath.Base(path), rowIndex+1, name, field.Name, value, err)
			}
			row[field.JSON] = parsed
		}
		out = append(out, row)
	}
	// The rows are checked as the JSON they are about to become, by the same
	// rules and checker the loader uses (an empty required cell is null).
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	rows, err := rules.Rows(raw, false)
	if err != nil {
		return nil, err
	}
	if err := checkRows(meta, rows); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return out, nil
}

func csvDataStart(records [][]string, meta Meta) int {
	start := 1
	if start < len(records) && sameRow(records[start], fieldValues(meta.Fields, func(f Field) string { return f.Title })) {
		start++
	}
	if start < len(records) && sameRow(records[start], fieldValues(meta.Fields, func(f Field) string { return f.Type })) {
		start++
	}
	if start < len(records) && sameRow(records[start], fieldValues(meta.Fields, fieldRule)) {
		start++
	}
	return start
}

func parseCell(value string, field Field) (any, error) {
	if value == "" {
		if field.Required {
			// null, so the shared rule check reports the required column
			// exactly as it would for an edited JSON file.
			return nil, nil
		}
		return zeroValue(field.Type), nil
	}
	if field.Parser != "" {
		return value, nil
	}
	switch strings.TrimPrefix(field.Type, "*") {
	case "string":
		return value, nil
	case "bool":
		return strconv.ParseBool(value)
	case "int", "int8", "int16", "int32", "int64":
		return strconv.ParseInt(value, 10, 64)
	case "uint", "uint8", "uint16", "uint32", "uint64":
		return strconv.ParseUint(value, 10, 64)
	case "float32", "float64":
		return strconv.ParseFloat(value, 64)
	default:
		var v any
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return value, nil
		}
		return v, nil
	}
}

func zeroValue(typeName string) any {
	switch strings.TrimPrefix(typeName, "*") {
	case "string":
		return ""
	case "bool":
		return false
	case "int", "int8", "int16", "int32", "int64":
		return int64(0)
	case "uint", "uint8", "uint16", "uint32", "uint64":
		return uint64(0)
	case "float32", "float64":
		return float64(0)
	default:
		return nil
	}
}

func generateGo(metas []Meta, outDir string, pkg string, force bool, stdout io.Writer) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	if err := resolveRefs(metas); err != nil {
		return err
	}
	for _, meta := range metas {
		for _, rule := range metaRules(meta) {
			if err := rule.Validate(); err != nil {
				return fmt.Errorf("%s %s: %w", meta.Kind, meta.Name, err)
			}
		}
	}
	var buf bytes.Buffer
	tmpl := template.Must(template.New("go").Funcs(template.FuncMap{
		"rules":      func(meta Meta) []string { return ruleLiterals(metaRules(meta)) },
		"upper":      firstUpper,
		"quote":      strconv.Quote,
		"keyType":    keyType,
		"keyField":   keyField,
		"lower":      firstLower,
		"needsJSON":  needsJSON,
		"parseExpr":  parseExpr,
		"fieldRules": fieldRule,
	}).Parse(goTemplate))
	if err := tmpl.Execute(&buf, map[string]any{
		"Package": pkg,
		"Metas":   metas,
	}); err != nil {
		return err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("format generated table config: %w\n%s", err, buf.String())
	}
	path := filepath.Join(outDir, "gen_table_config.go")
	if err := writeGenerated(path, formatted, force); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "table go: %s\n", path)
	return nil
}

var goTemplate = `// Code generated by tool/tablegen. DO NOT EDIT.
package {{.Package}}

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
{{- range .Metas}}
	{{.Alias}} "{{.ImportPath}}"
{{- end}}
)

var (
	_ = csv.NewReader
	_ = json.Unmarshal
	_ = fmt.Errorf
	_ io.Reader
	_ = time.ParseDuration
)

func RegisterGeneratedConfigData(r *configdata.Registry) error {
{{- range .Metas}}
{{- if eq .Kind "table"}}
	if err := configdata.RegisterTable(r, configdata.TableDef[{{keyType .}}, {{.Alias}}.{{.TypeName}}]{
		Name: configdata.Name({{quote .Name}}),
		File: {{quote .JSON}},
		Key: func(v {{.Alias}}.{{.TypeName}}) {{keyType .}} { return v.{{keyField .}} },
{{- with rules .}}
		// The schema tags' rules: configdata enforces them on every load and
		// reload (an edited JSON file included); generate checks the same ones.
		Rules: []configdata.FieldRule{
{{- range .}}
			{{.}},
{{- end}}
		},
{{- end}}
	}); err != nil {
		return err
	}
{{- else}}
	if err := configdata.RegisterObject(r, configdata.ObjectDef[{{.Alias}}.{{.TypeName}}]{
		Name: configdata.Name({{quote .Name}}),
		File: {{quote .JSON}},
{{- with rules .}}
		Rules: []configdata.FieldRule{
{{- range .}}
			{{.}},
{{- end}}
		},
{{- end}}
	}); err != nil {
		return err
	}
{{- end}}
{{- end}}
	return nil
}

// RegisterConfigData registers the generated definitions on the default
// registry. This is the entry point the generated aggregate in
// internal/registry calls; RegisterGeneratedConfigData stays exported and
// explicit for tests and for services that build their own registry.
//
//roost:register phase=config
func RegisterConfigData() error {
	return RegisterGeneratedConfigData(configdata.DefaultRegistry())
}

{{range .Metas}}
type {{.TypeName}}Cfg = {{.Alias}}.{{.TypeName}}

{{if eq .Kind "table"}}
func {{.TypeName}}TableFrom(snap *configdata.Snapshot) (*configdata.Table[{{keyType .}}, {{.Alias}}.{{.TypeName}}], bool) {
	return configdata.TableFrom[{{keyType .}}, {{.Alias}}.{{.TypeName}}](snap, configdata.Name({{quote .Name}}))
}

// {{.TypeName}}Table is the {{.Name}} table of the config snapshot pinned to the
// current request (configdata.ActiveSnapshot). A table that is not loaded is
// nil, and reading it is safe: Get reports false, Rows is empty.
func {{.TypeName}}Table() *configdata.Table[{{keyType .}}, {{.Alias}}.{{.TypeName}}] {
	table, _ := {{.TypeName}}TableFrom(configdata.ActiveSnapshot())
	return table
}

func {{.TypeName}}By{{keyField .}}(id {{keyType .}}) ({{.Alias}}.{{.TypeName}}, bool) {
	return {{.TypeName}}Table().Get(id)
}
{{else}}
func {{.TypeName}}ConfigFrom(snap *configdata.Snapshot) ({{.Alias}}.{{.TypeName}}, bool) {
	return configdata.ObjectFrom[{{.Alias}}.{{.TypeName}}](snap, configdata.Name({{quote .Name}}))
}
{{end}}

func Convert{{.TypeName}}CSV(r io.Reader) ({{if eq .Kind "table"}}[]{{.Alias}}.{{.TypeName}}{{else}}{{.Alias}}.{{.TypeName}}{{end}}, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		var zero {{if eq .Kind "table"}}[]{{.Alias}}.{{.TypeName}}{{else}}{{.Alias}}.{{.TypeName}}{{end}}
		return zero, err
	}
	rows, err := convert{{.TypeName}}Records(records)
	if err != nil {
		var zero {{if eq .Kind "table"}}[]{{.Alias}}.{{.TypeName}}{{else}}{{.Alias}}.{{.TypeName}}{{end}}
		return zero, err
	}
{{if eq .Kind "table"}}
	return rows, nil
{{else}}
	if len(rows) > 1 {
		var zero {{.Alias}}.{{.TypeName}}
		return zero, fmt.Errorf("{{.Name}}: object requires at most one data row, got %d", len(rows))
	}
	if len(rows) == 0 {
		var zero {{.Alias}}.{{.TypeName}}
		return zero, nil
	}
	return rows[0], nil
{{end}}
}

func convert{{.TypeName}}Records(records [][]string) ([]{{.Alias}}.{{.TypeName}}, error) {
	if len(records) == 0 {
		return nil, nil
	}
	header := records[0]
	if err := tablegenCheckHeader(header{{range .Fields}}, {{quote .CSV}}{{end}}); err != nil {
		return nil, err
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[name] = i
	}
	start := 1
	if len(records) > start && len(records[start]) > 0 && records[start][0] == {{quote (index .Fields 0).Title}} {
		start++
	}
	if len(records) > start && len(records[start]) > 0 && records[start][0] == {{quote (index .Fields 0).Type}} {
		start++
	}
	if len(records) > start && len(records[start]) > 0 && strings.Contains(records[start][0], "required") {
		start++
	}
	out := make([]{{.Alias}}.{{.TypeName}}, 0, len(records)-start)
	for rowIndex := start; rowIndex < len(records); rowIndex++ {
		record := records[rowIndex]
		if tablegenEmptyRecord(record) {
			continue
		}
		var item {{.Alias}}.{{.TypeName}}
{{- range .Fields}}
		if idx, ok := col[{{quote .CSV}}]; ok && idx < len(record) {
			raw := strings.TrimSpace(record[idx])
			if raw != "" {
				v, err := {{parseExpr . "raw"}}
				if err != nil {
					return nil, fmt.Errorf("row=%d field={{.Name}} value=%q: %w", rowIndex+1, raw, err)
				}
				item.{{.Name}} = v
			}
		}
{{- end}}
		out = append(out, item)
	}
	return out, nil
}
{{end}}

// tablegenCheckHeader rejects a column whose name differs from a field's csv
// name only in case: column names are case-sensitive, like the JSON keys
// configdata loads (a misspelled column would be skipped and its values lost).
func tablegenCheckHeader(header []string, names ...string) error {
	for _, column := range header {
		misspelled := ""
		for _, name := range names {
			if column == name {
				misspelled = ""
				break
			}
			if misspelled == "" && strings.EqualFold(column, name) {
				misspelled = name
			}
		}
		if misspelled != "" {
			return fmt.Errorf("header %q must be spelled %q (column names are case-sensitive)", column, misspelled)
		}
	}
	return nil
}

func tablegenEmptyRecord(record []string) bool {
	for _, cell := range record {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

func tablegenParseInt[T ~int | ~int8 | ~int16 | ~int32](raw string) (T, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	return T(v), err
}

func tablegenParseUint[T ~uint | ~uint8 | ~uint16 | ~uint32](raw string) (T, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	return T(v), err
}

func tablegenParseFloat[T ~float32](raw string) (T, error) {
	v, err := strconv.ParseFloat(raw, 64)
	return T(v), err
}

func tablegenParseJSON[T any](raw string) (T, error) {
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, err
	}
	return v, nil
}
`

func parseExpr(field Field, raw string) string {
	t := strings.TrimPrefix(field.Type, "*")
	if field.Parser != "" {
		return fmt.Sprintf("tablegenParseJSON[%s](%s)", field.Type, raw)
	}
	switch t {
	case "string":
		return raw + ", error(nil)"
	case "bool":
		return "strconv.ParseBool(" + raw + ")"
	case "int":
		return "tablegenParseInt[int](" + raw + ")"
	case "int8":
		return "tablegenParseInt[int8](" + raw + ")"
	case "int16":
		return "tablegenParseInt[int16](" + raw + ")"
	case "int32":
		return "tablegenParseInt[int32](" + raw + ")"
	case "int64":
		return "strconv.ParseInt(" + raw + ", 10, 64)"
	case "uint":
		return "tablegenParseUint[uint](" + raw + ")"
	case "uint8":
		return "tablegenParseUint[uint8](" + raw + ")"
	case "uint16":
		return "tablegenParseUint[uint16](" + raw + ")"
	case "uint32":
		return "tablegenParseUint[uint32](" + raw + ")"
	case "uint64":
		return "strconv.ParseUint(" + raw + ", 10, 64)"
	case "float32":
		return "tablegenParseFloat[float32](" + raw + ")"
	case "float64":
		return "strconv.ParseFloat(" + raw + ", 64)"
	case "time.Duration":
		return "time.ParseDuration(" + raw + ")"
	default:
		return fmt.Sprintf("tablegenParseJSON[%s](%s)", field.Type, raw)
	}
}

// resolveRefs checks every ref= declaration against the schema: the target
// must be a table of this run and the field's type (pointer or not) its key
// type. RR-20261005-NC-75: ref= used to be printed into the CSV rule row and
// enforced by nothing. The data itself is checked by configdata on every load
// (the Ref of the generated Rules).
func resolveRefs(metas []Meta) error {
	tables := make(map[string]Meta, len(metas))
	for _, meta := range metas {
		if meta.Kind == KindTable {
			tables[meta.Name] = meta
		}
	}
	for _, meta := range metas {
		for _, field := range meta.Fields {
			if field.Ref == "" {
				continue
			}
			if meta.Kind != KindTable {
				return fmt.Errorf("object %s field %s: ref is only checked on table rows", meta.Name, field.Name)
			}
			target, ok := tables[field.Ref]
			if !ok {
				return fmt.Errorf("table %s field %s: ref target %q is not a table in this schema", meta.Name, field.Name, field.Ref)
			}
			if fieldType := strings.TrimPrefix(field.Type, "*"); fieldType != keyType(target) {
				return fmt.Errorf("table %s field %s: ref type %s does not match %s key type %s", meta.Name, field.Name, field.Type, target.Name, keyType(target))
			}
		}
	}
	return nil
}

// ruleLiterals renders rules as FieldRule composite-literal elements for the
// generated loader, one per line: {Field: "level", Required: true, Min: "1"}.
func ruleLiterals(declared []rules.Rule) []string {
	out := make([]string, 0, len(declared))
	for _, rule := range declared {
		parts := []string{"Field: " + strconv.Quote(rule.Field)}
		if rule.Required {
			parts = append(parts, "Required: true")
		}
		if rule.Unique {
			parts = append(parts, "Unique: true")
		}
		if rule.Min != "" {
			parts = append(parts, "Min: "+strconv.Quote(rule.Min))
		}
		if rule.Ref != "" {
			parts = append(parts, "Ref: "+strconv.Quote(rule.Ref))
		}
		if len(rule.Enum) > 0 {
			quoted := make([]string, len(rule.Enum))
			for i, value := range rule.Enum {
				quoted[i] = strconv.Quote(value)
			}
			parts = append(parts, "Enum: []string{"+strings.Join(quoted, ", ")+"}")
		}
		out = append(out, "{"+strings.Join(parts, ", ")+"}")
	}
	return out
}

func keyType(meta Meta) string {
	field := keyFieldInfo(meta)
	if field.Type == "" {
		return "int64"
	}
	return field.Type
}

func keyField(meta Meta) string {
	field := keyFieldInfo(meta)
	if field.Name == "" {
		return "ID"
	}
	return field.Name
}

func keyFieldInfo(meta Meta) Field {
	for _, field := range meta.Fields {
		if field.Name == meta.Key {
			return field
		}
	}
	if len(meta.Fields) > 0 {
		return meta.Fields[0]
	}
	return Field{}
}

func needsJSON(meta Meta) bool {
	for _, field := range meta.Fields {
		if !isSimpleType(field.Type) {
			return true
		}
	}
	return false
}

func isSimpleType(t string) bool {
	switch strings.TrimPrefix(t, "*") {
	case "string", "bool", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "time.Duration":
		return true
	default:
		return false
	}
}

func fieldValues(fields []Field, value func(Field) string) []string {
	out := make([]string, len(fields))
	for i, field := range fields {
		out[i] = value(field)
	}
	return out
}

func fieldRule(field Field) string {
	var parts []string
	if field.Required {
		parts = append(parts, "required")
	}
	if field.Unique {
		parts = append(parts, "unique")
	}
	if field.Min != "" {
		parts = append(parts, "min="+field.Min)
	}
	if field.Ref != "" {
		parts = append(parts, "ref="+field.Ref)
	}
	if len(field.Enum) > 0 {
		parts = append(parts, "enum="+strings.Join(field.Enum, "|"))
	}
	if field.Parser != "" {
		parts = append(parts, "parser="+field.Parser)
	}
	return strings.Join(parts, ";")
}

func sameRow(a []string, b []string) bool {
	if len(a) < len(b) {
		return false
	}
	for i := range b {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func isEmptyRow(row []string) bool {
	for _, cell := range row {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

func writeGenerated(path string, data []byte, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s exists; pass -force to overwrite", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func optionOr(opts map[string]string, key string, fallback string) string {
	if v := opts[key]; v != "" {
		return v
	}
	return fallback
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

func firstUpper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstLower(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

var _ io.Reader

// hasMarker and cutMarker adapt marker.Cut to the line-oriented switch above.
func hasMarker(line, kind string) bool {
	_, ok := marker.Cut(line, kind)
	return ok
}

func cutMarker(line, kind string) string {
	body, _ := marker.Cut(line, kind)
	return strings.TrimSpace(body)
}
