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
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
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

	"github.com/tjbdwanghaibo/roost-codegen/internal/project"
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
	Parser   string
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
		if *outDir == "" {
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
					jsonName = csvName
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
					Parser:   tag.Get("parser"),
				})
			}
			if meta.Kind == KindTable && meta.Key == "" && len(meta.Fields) > 0 {
				meta.Key = meta.Fields[0].Name
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
	manifest := map[string]any{
		"version":      1,
		"generated_at": "",
		"tables":       map[string]any{},
	}
	for _, meta := range metas {
		rows, err := readCSVRecords(filepath.Join(csvDir, meta.File), meta)
		if err != nil {
			return err
		}
		var payload any = rows
		if meta.Kind == KindObject {
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
		out := filepath.Join(jsonDir, meta.JSON)
		if err := writeGenerated(out, raw, force); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "table json: %s\n", out)
	}
	manifestRaw, _ := json.MarshalIndent(manifest, "", "  ")
	manifestRaw = append(manifestRaw, '\n')
	return writeGenerated(filepath.Join(jsonDir, "_manifest.json"), manifestRaw, true)
}

func checkJSONFiles(metas []Meta, jsonDir string) error {
	for _, meta := range metas {
		path := filepath.Join(jsonDir, meta.JSON)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("check %s: %w", path, err)
		}
	}
	return nil
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
	if err := validateRows(filepath.Base(path), meta, out); err != nil {
		return nil, err
	}
	return out, nil
}

// validateRows enforces the constraints a table declares in its tags at
// generation time, where the CSV author can still fix them. Until it existed,
// `unique="true"` and `min=` were read from the tags and printed into the rule
// row of the CSV — and enforced by nothing: a duplicate id was written into
// the JSON and the generated loader silently kept the last row (U-0033).
// `ref=` names another table and is not checked here; that needs every
// table loaded and is the generated loader's job.
func validateRows(file string, meta Meta, rows []map[string]any) error {
	for _, field := range meta.Fields {
		unique := field.Unique || (meta.Kind == KindTable && field.Name == meta.Key)
		if unique {
			seen := make(map[string]int, len(rows))
			for index, row := range rows {
				value, ok := row[field.JSON]
				if !ok {
					continue
				}
				key := fmt.Sprint(value)
				if first, dup := seen[key]; dup {
					what := "unique"
					if field.Name == meta.Key {
						what = "key"
					}
					return fmt.Errorf("%s %s field %s repeats value %q in data rows %d and %d", file, what, field.Name, key, first, index+1)
				}
				seen[key] = index + 1
			}
		}
		if field.Min != "" {
			minimum, err := strconv.ParseFloat(field.Min, 64)
			if err != nil {
				return fmt.Errorf("%s field %s has non-numeric min=%q", file, field.Name, field.Min)
			}
			for index, row := range rows {
				value, ok := row[field.JSON]
				if !ok {
					continue
				}
				var number float64
				switch v := value.(type) {
				case int64:
					number = float64(v)
				case uint64:
					number = float64(v)
				case float64:
					number = v
				default:
					continue
				}
				if number < minimum {
					return fmt.Errorf("%s field %s value %v in data row %d is below min=%s", file, field.Name, value, index+1, field.Min)
				}
			}
		}
	}
	return nil
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
			return nil, fmt.Errorf("required field is empty")
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
	var buf bytes.Buffer
	tmpl := template.Must(template.New("go").Funcs(template.FuncMap{
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

	"github.com/tjbdwanghaibo/roost-core/configdata"
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
	}); err != nil {
		return err
	}
{{- else}}
	if err := configdata.RegisterObject(r, configdata.ObjectDef[{{.Alias}}.{{.TypeName}}]{
		Name: configdata.Name({{quote .Name}}),
		File: {{quote .JSON}},
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

func {{.TypeName}}By{{keyField .}}(id {{keyType .}}) ({{.Alias}}.{{.TypeName}}, bool) {
	table, ok := {{.TypeName}}TableFrom(configdata.ActiveSnapshot())
	if !ok {
		var zero {{.Alias}}.{{.TypeName}}
		return zero, false
	}
	return table.Get(id)
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
	var rules []string
	if field.Required {
		rules = append(rules, "required")
	}
	if field.Unique {
		rules = append(rules, "unique")
	}
	if field.Min != "" {
		rules = append(rules, "min="+field.Min)
	}
	if field.Ref != "" {
		rules = append(rules, "ref="+field.Ref)
	}
	if field.Parser != "" {
		rules = append(rules, "parser="+field.Parser)
	}
	return strings.Join(rules, ";")
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
