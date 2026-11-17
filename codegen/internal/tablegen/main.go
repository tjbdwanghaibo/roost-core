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
	// KindKV 只出现在导出模型里：layout=kv 的 object（Meta.Kind 仍是 object）。
	KindKV TableKind = "kv"
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
	// Side 是整张表所属的端（标记 side=）：空表示两端都要，"c" 只给客户端（服务端不登记），
	// "s" 只给服务端（不导出给客户端）。
	Side string
	// Layout 是 CSV 写法：空是横排（一行一条），"kv" 是竖排键值（只用于 object，见 kv.go）。
	Layout string
	// Fields 是本端看到的字段。按端投影（projectSide）之后只剩本端字段；投影前与 AllFields 相同。
	Fields []Field
	// AllFields 是 schema 结构体的全部字段，也就是 CSV 表头可以出现的全部列。投影不改它：表头核对、
	// 标题 / 类型 / 规则行的识别都按它做，所以另一端的列出现在 CSV 里不会被当成拼错或数据。
	// 为 nil 时等于 Fields（单测直接构造的 Meta）。
	AllFields []Field
	// Omitted 是投影时被排除的本表字段的 Go 名（另一端专用），只用于生成代码的注释。
	Omitted []string
	// named 是 schema 包（本表所在目录）里声明的类型名 → 底层内建标量的规范名（底层不是内建标量的为 ""），
	// 见 named.go。生成服务端 Go 时据此给这些类型名加上导入别名。
	named map[string]string
}

// sheetFields 是 CSV 表头可以出现的全部字段（见 AllFields）。
func (m Meta) sheetFields() []Field {
	if m.AllFields != nil {
		return m.AllFields
	}
	return m.Fields
}

type Field struct {
	Name string
	// Type 是 schema 里写的 Go 类型表达式（CSV 模板的类型行、服务端生成的 Go 用它）。
	Type string
	// Resolved 是把 schema 包里底层是内建标量的具名类型换成底层类型后的表达式（Rank → int32，
	// []Rank → []int32），没有具名类型时与 Type 相同。CSV 转换、index、客户端导出都看它（named.go）。
	Resolved string
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
	// Side 是字段所属的端（tag side:"c" / side:"s"），空表示两端都要。
	Side string
	// Index 是二级索引名（tag index:"true" 取 Go 字段名，index:"<名字>" 取该名字），空表示没有索引。
	// 语义与 cfggen 的 index 相同：非唯一，查询返回全部匹配行（文件顺序）。
	Index string
	// Note 是 layout=kv 表里这个字段那一行的 note 列，生成 Go 时写进注释；其它表为空。
	Note string
}

// resolvedType 是 Resolved；直接构造、没有填 Resolved 的 Field（单测）取 Type。
func (f Field) resolvedType() string {
	if f.Resolved != "" {
		return f.Resolved
	}
	return f.Type
}

// DefaultMetaDir is where a business project keeps its table meta files;
// `roost generate` refers to it.
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

	// 两端的投影规则（主键不能排除、ref 同端、字段不能全被排除）与 index 声明每次都核对，
	// 不管这次产出哪一样；服务端产物只用服务端投影。
	if err := validateMetas(metas); err != nil {
		return err
	}
	server, omitted, err := projectSide(metas, sideServer)
	if err != nil {
		return err
	}
	if omitted.tables+omitted.fields > 0 {
		_, _ = fmt.Fprintf(stdout, "tablegen: server side omitted %d tables / %d fields (client only)\n", omitted.tables, omitted.fields)
	}

	// 模板给策划填，含两端的全部列与 side=c 的表。
	if *csvTemplateDir != "" {
		if err := writeCSVTemplates(metas, *csvTemplateDir, *force, stdout); err != nil {
			return err
		}
	}
	if *csvDir != "" && *jsonDir != "" {
		if err := convertCSVToJSON(server, *csvDir, *jsonDir, *force, stdout); err != nil {
			return err
		}
	}
	if *jsonDir != "" && *check {
		if err := checkJSONFiles(server, *jsonDir); err != nil {
			return err
		}
	}
	// -out 同时给了 -csv 时，layout=kv 表的 note 列写进生成代码的注释。
	if *outDir != "" && *csvDir != "" {
		if err := attachKVNotes(server, *csvDir); err != nil {
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
		if err := generateGo(server, *outDir, *outPkg, *force, stdout); err != nil {
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
	// refuse a misspelt option instead of using the default.
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
	named, err := packageTypes(filepath.Dir(path))
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
				Side:       opts["side"],
				Layout:     opts["layout"],
				named:      named,
			}
			if !validSide(meta.Side) {
				return nil, fmt.Errorf("%s: %s %s: side=%q must be c (client only) or s (server only)", path, meta.Kind, meta.TypeName, meta.Side)
			}
			if meta.Layout != "" && meta.Layout != layoutKV {
				return nil, fmt.Errorf("%s: object %s: layout=%q is unknown (the only layout is kv)", path, meta.TypeName, meta.Layout)
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
				side := tag.Get("side")
				if !validSide(side) {
					return nil, fmt.Errorf("%s: %s field %s: side:%q must be c (client only) or s (server only)", path, meta.TypeName, name, side)
				}
				if side != "" && meta.Side != "" && side != meta.Side {
					return nil, fmt.Errorf("%s: %s field %s: side:%q on a side=%s %s is exported nowhere", path, meta.TypeName, name, side, meta.Side, meta.Kind)
				}
				index, err := indexName(name, tag.Get("index"))
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", path, meta.TypeName, err)
				}
				meta.Fields = append(meta.Fields, Field{
					Name:     name,
					Type:     types.ExprString(field.Type),
					Resolved: resolveType(field.Type, named),
					CSV:      csvName,
					JSON:     jsonName,
					Title:    tag.Get("title"),
					Required: tag.Get("required") == "true",
					Unique:   tag.Get("unique") == "true",
					Min:      tag.Get("min"),
					Ref:      tag.Get("ref"),
					Enum:     splitEnum(tag.Get("enum")),
					Parser:   tag.Get("parser"),
					Side:     side,
					Index:    index,
				})
			}
			meta.AllFields = meta.Fields
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
		if meta.Layout == layoutKV {
			rows = kvTemplateRows(meta)
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
	previous, err := readTableManifest(jsonDir)
	if err != nil {
		return err
	}
	manifest := tableJSONManifest{Version: tableManifestVersion, Tables: make(map[string]string)}
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
		payload, _, err := readPayload(csvDir, meta)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		outputs = append(outputs, output{meta.JSON, raw})
		manifest.Tables[meta.JSON] = fmt.Sprintf("%x", sha256.Sum256(raw))
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

// tableManifestVersion 是 tablegen 读写的唯一 manifest 版本。v2 用 tables 记录每个生成文件的
// 名字与内容哈希，退役与“未登记即手写”都依赖这份归属记录。
const tableManifestVersion = 2

// readTableManifest 读 jsonDir/_manifest.json；文件不存在返回零值（从未生成过）。存在时版本必须是
// v2：v1 没有归属记录，无法判断目录里未登记的 JSON 是旧生成物还是手写数据，不再读取，报错并给出一次性清理办法。
func readTableManifest(jsonDir string) (tableJSONManifest, error) {
	path := filepath.Join(jsonDir, "_manifest.json")
	var manifest tableJSONManifest
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return manifest, nil
	}
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("read table manifest: %w", err)
	}
	switch manifest.Version {
	case tableManifestVersion:
		return manifest, nil
	case 1:
		return manifest, fmt.Errorf("table manifest %s is version 1, which is no longer read (it recorded no ownership): delete it and any JSON in %s that an earlier generation produced, keep hand-maintained JSON, then generate again (codegen/docs/CODEGEN_REFERENCE.zh-CN.md §9.1)", path, jsonDir)
	default:
		return manifest, fmt.Errorf("unsupported table manifest version %d in %s", manifest.Version, path)
	}
}

// ManifestOwnsJSON reports whether jsonDir/_manifest.json still records
// generated JSON files. `roost generate` asks this when configs/table has no
// CSV: only a manifest that owns outputs has something to retire.
// A missing manifest and the scaffold's empty v2 manifest
// mean the project has simply not written its CSVs yet, and running tablegen
// would fail on the first meta's missing CSV. A manifest in
// any other version is an error, as in the conversion itself.
func ManifestOwnsJSON(jsonDir string) (bool, error) {
	manifest, err := readTableManifest(jsonDir)
	if err != nil {
		return false, err
	}
	return len(manifest.Tables) > 0, nil
}

func safeTableJSONName(name string) bool {
	return name != "_manifest.json" && filepath.Base(name) == name && strings.HasSuffix(name, ".json")
}

// checkJSONFiles is `-json <dir> -check`: the JSON a server will load is held
// to the rules the schema declares — the same []rules.Rule the generated
// loader hands configdata (metaRules), checked by the same rules.Check the
// loader runs on every load and reload. It is early feedback only: the
// loader enforces the rules whether or not anybody ran -check. ref needs the
// typed tables and is left to the loader. -check goes beyond "is valid JSON".
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
// tag edited in the schema changes all three on the next generate. The
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

// readPayload 读一张表的 CSV，返回写进 JSON 的载荷（表是行的列表，object / kv 是单个对象）
// 与行数（object / kv 记 1）。只含 meta.Fields（本端字段）。服务端转换与客户端导出共用它。
func readPayload(csvDir string, meta Meta) (any, int, error) {
	path := filepath.Join(csvDir, meta.File)
	if meta.Layout == layoutKV {
		object, err := readKVObject(path, meta)
		if err != nil {
			return nil, 0, err
		}
		return object.values, 1, nil
	}
	rows, err := readCSVRecords(path, meta)
	if err != nil {
		return nil, 0, err
	}
	if meta.Kind != KindObject {
		return rows, len(rows), nil
	}
	if len(rows) > 1 {
		return nil, 0, fmt.Errorf("%s: object %s requires at most one data row, got %d", meta.File, meta.TypeName, len(rows))
	}
	if len(rows) > 0 {
		return rows[0], 1, nil
	}
	return map[string]any{}, 1, nil
}

// readCSVFile 读一个 CSV 文件，返回全部记录与每条记录在文件里的行号（1 起）。以 # 开头的行
// 是注释（encoding/csv 的 Comment：# 必须是行首第一个字符，前面不能有空白；要以 # 开头的值
// 加引号），读取时忽略，所以行号与记录下标不再一一对应，报错一律用这里的行号。
func readCSVFile(path string, fieldsPerRecord int) ([][]string, []int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true
	reader.Comment = '#'
	reader.FieldsPerRecord = fieldsPerRecord
	var records [][]string
	var lines []int
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return records, lines, nil
		}
		if err != nil {
			return nil, nil, err
		}
		line, _ := reader.FieldPos(0)
		records = append(records, record)
		lines = append(lines, line)
	}
}

func readCSVRecords(path string, meta Meta) ([]map[string]any, error) {
	records, lines, err := readCSVFile(path, 0)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	header := records[0]
	sheet := meta.sheetFields()
	// Column names are case-sensitive, like the JSON keys they become: a
	// header that differs from a field's csv name only in case would
	// otherwise be skipped as an unknown column and its values lost. The
	// check covers the other side's columns too (AllFields).
	if column, want, found := rules.MisspelledKey(header, fieldValues(sheet, func(f Field) string { return f.CSV })); found {
		return nil, fmt.Errorf("%s: header %q must be spelled %q (column names are case-sensitive)", filepath.Base(path), column, want)
	}
	// 只读本端字段的列；另一端的列与未声明的列一样跳过。
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
				return nil, fmt.Errorf("%s row=%d col=%s field=%s value=%q: %w", filepath.Base(path), lines[rowIndex], name, field.Name, value, err)
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
	sheet := meta.sheetFields()
	start := 1
	if start < len(records) && sameRow(records[start], fieldValues(sheet, func(f Field) string { return f.Title })) {
		start++
	}
	if start < len(records) && sameRow(records[start], fieldValues(sheet, func(f Field) string { return f.Type })) {
		start++
	}
	if start < len(records) && sameRow(records[start], fieldValues(sheet, fieldRule)) {
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
		return zeroValue(field.resolvedType()), nil
	}
	if field.Parser != "" {
		return value, nil
	}
	switch strings.TrimPrefix(field.resolvedType(), "*") {
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
		"rules":         func(meta Meta) []string { return ruleLiterals(metaRules(meta)) },
		"upper":         firstUpper,
		"quote":         strconv.Quote,
		"keyType":       keyType,
		"keyField":      keyField,
		"lower":         firstLower,
		"needsJSON":     needsJSON,
		"parseExpr":     parseExpr,
		"fieldRules":    fieldRule,
		"cfgDoc":        cfgDoc,
		"indexed":       indexedFields,
		"indexKey":      indexKeyExpr,
		"goType":        qualifiedType,
		"indexAccessor": indexAccessor,
		"sheet":         func(meta Meta) []Field { return meta.sheetFields() },
		"hasKV":         hasKV,
		"omittedCSV":    omittedCSV,
		"requiredCSV":   requiredCSV,
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
{{- range $m := .Metas}}
{{- if eq .Kind "table"}}
	if err := configdata.RegisterTable(r, configdata.TableDef[{{keyType .}}, {{.Alias}}.{{.TypeName}}]{
		Name: configdata.Name({{quote .Name}}),
		File: {{quote .JSON}},
		Key: func(v {{.Alias}}.{{.TypeName}}) {{keyType .}} { return v.{{keyField .}} },
{{- with indexed .}}
		// Secondary indexes (tag index): not unique, a lookup returns every
		// matching row in file order.
		Indexes: []configdata.IndexDef[{{$m.Alias}}.{{$m.TypeName}}]{
{{- range .}}
			{Name: {{quote .Index}}, Key: func(v {{$m.Alias}}.{{$m.TypeName}}) string { return {{indexKey . (print "v." .Name)}} }},
{{- end}}
		},
{{- end}}
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

{{range $m := .Metas}}
{{cfgDoc .}}type {{.TypeName}}Cfg = {{.Alias}}.{{.TypeName}}

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
{{range indexed .}}
// {{indexAccessor $m .}} returns the {{$m.Name}} rows whose {{.Name}} equals v, in
// file order (index {{quote .Index}}; not unique, so several rows may match).
// Like {{$m.TypeName}}Table it reads the snapshot pinned to the current request.
func {{indexAccessor $m .}}(v {{goType $m .Type}}) []{{$m.Alias}}.{{$m.TypeName}} {
	return {{$m.TypeName}}Table().GetByIndex({{quote .Index}}, {{indexKey . "v"}})
}
{{end}}{{else}}
func {{.TypeName}}ConfigFrom(snap *configdata.Snapshot) ({{.Alias}}.{{.TypeName}}, bool) {
	return configdata.ObjectFrom[{{.Alias}}.{{.TypeName}}](snap, configdata.Name({{quote .Name}}))
}
{{end}}

{{if eq .Layout "kv"}}{{template "kvConverter" .}}{{else}}func Convert{{.TypeName}}CSV(r io.Reader) ({{if eq .Kind "table"}}[]{{.Alias}}.{{.TypeName}}{{else}}{{.Alias}}.{{.TypeName}}{{end}}, error) {
	records, lines, err := tablegenReadCSV(r)
	if err != nil {
		var zero {{if eq .Kind "table"}}[]{{.Alias}}.{{.TypeName}}{{else}}{{.Alias}}.{{.TypeName}}{{end}}
		return zero, err
	}
	rows, err := convert{{.TypeName}}Records(records, lines)
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

func convert{{.TypeName}}Records(records [][]string, lines []int) ([]{{.Alias}}.{{.TypeName}}, error) {
	if len(records) == 0 {
		return nil, nil
	}
	header := records[0]
	if err := tablegenCheckHeader(header{{range sheet .}}, {{quote .CSV}}{{end}}); err != nil {
		return nil, err
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[name] = i
	}
	start := 1
	if len(records) > start && len(records[start]) > 0 && records[start][0] == {{quote (index (sheet .) 0).Title}} {
		start++
	}
	if len(records) > start && len(records[start]) > 0 && records[start][0] == {{quote (index (sheet .) 0).Type}} {
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
				v, err := {{parseExpr $m . "raw"}}
				if err != nil {
					return nil, fmt.Errorf("row=%d field={{.Name}} value=%q: %w", lines[rowIndex], raw, err)
				}
				item.{{.Name}} = v
			}
		}
{{- end}}
		out = append(out, item)
	}
	return out, nil
}
{{end}}{{end}}

// tablegenReadCSV reads a whole sheet. Lines starting with # are comments
// and skipped, so each record comes back with the file line it starts on
// (a quoted cell may span lines; errors name the record's first line).
func tablegenReadCSV(r io.Reader) ([][]string, []int, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.Comment = '#'
	var records [][]string
	var lines []int
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return records, lines, nil
		}
		if err != nil {
			return nil, nil, err
		}
		line, _ := reader.FieldPos(0)
		records = append(records, record)
		lines = append(lines, line)
	}
}

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

func tablegenParseInt[T ~int | ~int8 | ~int16 | ~int32 | ~int64](raw string) (T, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	return T(v), err
}

func tablegenParseUint[T ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64](raw string) (T, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	return T(v), err
}

func tablegenParseFloat[T ~float32 | ~float64](raw string) (T, error) {
	v, err := strconv.ParseFloat(raw, 64)
	return T(v), err
}

func tablegenParseBool[T ~bool](raw string) (T, error) {
	v, err := strconv.ParseBool(raw)
	return T(v), err
}

func tablegenParseJSON[T any](raw string) (T, error) {
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, err
	}
	return v, nil
}
{{- if hasKV .Metas}}

// tablegenCheckKVHeader requires the header key,value,note of a layout=kv
// sheet (note may be left out); no further non-empty column follows it.
func tablegenCheckKVHeader(record []string) error {
	want := []string{"key", "value", "note"}
	for i, cell := range record {
		cell = strings.TrimSpace(cell)
		if (i < len(want) && cell == want[i]) || (i >= 2 && cell == "") {
			continue
		}
		return fmt.Errorf("a layout=kv sheet starts with the header key,value,note, got %q", strings.Join(record, ","))
	}
	if len(record) < 2 {
		return fmt.Errorf("a layout=kv sheet starts with the header key,value,note, got %q", strings.Join(record, ","))
	}
	return nil
}
{{- end}}
{{define "kvConverter"}}// Convert{{.TypeName}}CSV reads the layout=kv sheet of {{.Name}}: a header
// key,value,note, then one row per field (key is the field's csv name). Lines
// starting with # are comments. An unknown or repeated key, or a missing
// required key, is an error naming the line and the key.
{{- with .Omitted}}
// Client-only fields (side:"c") are accepted as keys and stay zero here.
{{- end}}
func Convert{{.TypeName}}CSV(r io.Reader) ({{.Alias}}.{{.TypeName}}, error) {
	var item {{.Alias}}.{{.TypeName}}
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.Comment = '#'
	reader.FieldsPerRecord = -1
	seen := make(map[string]int)
	header := false
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return {{.Alias}}.{{.TypeName}}{}, err
		}
		line, _ := reader.FieldPos(0)
		if tablegenEmptyRecord(record) {
			continue
		}
		if !header {
			if err := tablegenCheckKVHeader(record); err != nil {
				return {{.Alias}}.{{.TypeName}}{}, fmt.Errorf("line=%d: %w", line, err)
			}
			header = true
			continue
		}
		key := strings.TrimSpace(record[0])
		raw := ""
		if len(record) > 1 {
			raw = strings.TrimSpace(record[1])
		}
		if first, dup := seen[key]; dup {
			return {{.Alias}}.{{.TypeName}}{}, fmt.Errorf("line=%d key=%q: duplicate key (first at line %d)", line, key, first)
		}
		seen[key] = line
		switch key {
{{- range .Fields}}
		case {{quote .CSV}}:
			if raw != "" {
				v, err := {{parseExpr $ . "raw"}}
				if err != nil {
					return {{$.Alias}}.{{$.TypeName}}{}, fmt.Errorf("line=%d key=%q field={{.Name}} value=%q: %w", line, key, raw, err)
				}
				item.{{.Name}} = v
			}
{{- end}}
{{- with omittedCSV .}}
		case {{.}}:
			// client-only: stays zero on the server
{{- end}}
		default:
			return {{.Alias}}.{{.TypeName}}{}, fmt.Errorf("line=%d key=%q: unknown key", line, key)
		}
	}
{{- range requiredCSV .}}
	if _, ok := seen[{{quote .}}]; !ok {
		return {{$.Alias}}.{{$.TypeName}}{}, fmt.Errorf("key %q is required but missing", {{quote .}})
	}
{{- end}}
	return item, nil
}
{{end}}`

// parseExpr 是生成的 Convert<Type>CSV 里把单元格原文 raw 解析成字段值的表达式（值, error）。
// schema 包里的具名标量按底层类型解析，再转换成具名类型本身（tablegenParseInt[meta0.Rank]）。
func parseExpr(meta Meta, field Field, raw string) string {
	goType := qualifiedType(meta, field.Type)
	if field.Parser != "" {
		return fmt.Sprintf("tablegenParseJSON[%s](%s)", goType, raw)
	}
	t := strings.TrimPrefix(field.resolvedType(), "*")
	if named := strings.TrimPrefix(goType, "*"); named != t {
		switch t {
		case "string":
			return named + "(" + raw + "), error(nil)"
		case "bool":
			return "tablegenParseBool[" + named + "](" + raw + ")"
		case "int", "int8", "int16", "int32", "int64":
			return "tablegenParseInt[" + named + "](" + raw + ")"
		case "uint", "uint8", "uint16", "uint32", "uint64":
			return "tablegenParseUint[" + named + "](" + raw + ")"
		case "float32", "float64":
			return "tablegenParseFloat[" + named + "](" + raw + ")"
		}
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
		return fmt.Sprintf("tablegenParseJSON[%s](%s)", goType, raw)
	}
}

// resolveRefs checks every ref= declaration against the schema: the target
// must be a table of this run and the field's type (pointer or not) its key
// type, so ref= is enforced rather than only printed into the CSV rule row. The data itself is checked by configdata on every load
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
			// schema 包里声明的类型名带上各自的导入路径再比较：同一个包里的同名类型相同，不同包里的同名类型不同。
			fieldType := strings.TrimPrefix(qualifyType(meta, field.Type, meta.ImportPath), "*")
			if targetKey := keyFieldInfo(target); fieldType != qualifyType(target, targetKey.Type, target.ImportPath) {
				return fmt.Errorf("table %s field %s: ref type %s does not match %s key type %s", meta.Name, field.Name, field.Type, target.Name, keyFieldInfo(target).Type)
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

// keyType 是生成的服务端 Go 里主键的类型（schema 包的具名类型带导入别名）。
func keyType(meta Meta) string {
	field := keyFieldInfo(meta)
	if field.Type == "" {
		return "int64"
	}
	return qualifiedType(meta, field.Type)
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
	if field.Index != "" {
		parts = append(parts, "index="+field.Index)
	}
	if field.Side != "" {
		parts = append(parts, "side="+field.Side)
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
