package roost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
)

// 配置表导出给客户端（roost.yaml 的 client_export.config，维护者 2026-11-17，见
// docs/maintenance/FEATURE-2026-11-17-client-config-export.md）。声明了它的工程，roost generate 每次从
// configs/schema 与 configs/table 算出客户端投影（tablegen.ClientExport），整份写进 <dir>：
//
//	data/<表>.json        只含客户端字段的数据（普通表 {"rows":[...]}，object / kv 单个对象）
//	csharp/*.cs           C# 读取代码（netstandard2.1 纯 C#：自带 JSON 解析器，不用反射、不碰 UnityEngine）
//	manifest.json         kind roost-config-export：版本、数据版本、每张表的 schema / 数据哈希与行数、每个文件的哈希
//
// data/ 下的 .json、csharp/ 下的 .cs 与 manifest.json 归生成器所有：本轮不产出的删掉，别处的文件不动。
// 不需要 protoc。没声明 config 的工程不跑这一步，生成结果不变。

// DefaultClientConfigExportDir 是 client_export.config.dir 的缺省值。
const DefaultClientConfigExportDir = "shared/config"

// DefaultClientConfigNamespace 是 client_export.config.csharp.namespace 的缺省值。
const DefaultClientConfigNamespace = "Roost.Config"

// clientConfigExportKind 写在配置导出的 manifest.json 里，roost client sync 凭它认出配置导出清单。
const clientConfigExportKind = "roost-config-export"

const clientConfigExportSchema = 1

// ClientConfigExportSpec 是 roost.yaml 的 client_export.config。
type ClientConfigExportSpec struct {
	// Dir 相对工程根，缺省 shared/config；产物提交进仓库。
	Dir string `yaml:"dir,omitempty"`
	// Tables 列出要导出的表名；为空表示全部带客户端字段的表。
	Tables []string `yaml:"tables,omitempty"`
	// CSharp 是 C# 代码的选项。
	CSharp *ClientConfigCSharp `yaml:"csharp,omitempty"`
}

// ClientConfigCSharp 是 client_export.config.csharp。
type ClientConfigCSharp struct {
	// Namespace 缺省 Roost.Config。
	Namespace string `yaml:"namespace,omitempty"`
}

func (spec *ClientConfigExportSpec) dir() string {
	if spec == nil || strings.TrimSpace(spec.Dir) == "" {
		return DefaultClientConfigExportDir
	}
	return path.Clean(filepath.ToSlash(strings.TrimSpace(spec.Dir)))
}

func (spec *ClientConfigExportSpec) namespace() string {
	if spec != nil && spec.CSharp != nil && spec.CSharp.Namespace != "" {
		return spec.CSharp.Namespace
	}
	return DefaultClientConfigNamespace
}

// validate 只核对写法；tables 里的表是否存在、是否带客户端字段要读 schema，在生成与 doctor 时核对（selectConfigTables）。
func (spec *ClientConfigExportSpec) validate() error {
	if spec == nil {
		return nil
	}
	joined := validateExportDir("client_export.config.dir", spec.Dir, DefaultClientConfigExportDir)
	seen := map[string]bool{}
	for _, name := range spec.Tables {
		switch {
		case strings.TrimSpace(name) == "":
			joined = errors.Join(joined, errors.New("client_export.config.tables has an empty name"))
		case seen[name]:
			joined = errors.Join(joined, fmt.Errorf("client_export.config.tables repeats %q", name))
		}
		seen[name] = true
	}
	if spec.CSharp != nil {
		if ns := spec.CSharp.Namespace; ns != "" && !csharpNamespacePattern.MatchString(ns) {
			joined = errors.Join(joined, fmt.Errorf("client_export.config.csharp.namespace %q is not a C# namespace (dot-separated identifiers)", ns))
		}
	}
	return joined
}

// owns 判断工程相对路径 rel（斜杠分隔）是不是配置导出目录里归生成器所有的文件：manifest.json、data/*.json、
// csharp/*.cs（只认这两层，不认更深的子目录）。没声明 config 时为 false。
func (spec *ClientConfigExportSpec) owns(rel string) bool {
	if spec == nil {
		return false
	}
	inner, ok := strings.CutPrefix(filepath.ToSlash(rel), spec.dir()+"/")
	if !ok {
		return false
	}
	if inner == exportManifest {
		return true
	}
	if name, ok := strings.CutPrefix(inner, "data/"); ok {
		return !strings.Contains(name, "/") && strings.HasSuffix(name, ".json")
	}
	if name, ok := strings.CutPrefix(inner, "csharp/"); ok {
		return !strings.Contains(name, "/") && strings.HasSuffix(name, ".cs")
	}
	return false
}

// clientConfigManifest 是配置导出目录的 manifest.json。
type clientConfigManifest struct {
	Kind      string `json:"kind"`
	Schema    int    `json:"schema"`
	Core      string `json:"core"`
	Generator string `json:"generator"`
	// DataVersion 是导出表的数据版本（tablegen.DataVersion，完整的 sha256 十六进制）；DataVersionShort 是显示用的前 12 位。
	// 只记录、可查看，登录时不核对（维护者 2026-11-17 决定 4）。
	DataVersion      string `json:"data_version"`
	DataVersionShort string `json:"data_version_short"`
	Namespace        string `json:"csharp_namespace"`
	// Omitted 是客户端投影排除的表数（side=s 与没列在 tables 里的）与留下的表里排除的字段数（side:"s"）。
	Omitted clientConfigOmitted `json:"omitted"`
	Tables  []clientConfigTable `json:"tables"`
	// Files 是除 manifest.json 外每个导出文件的哈希（键相对 <dir>，按 LF 计算）。
	Files map[string]string `json:"files"`
}

type clientConfigOmitted struct {
	Tables int `json:"tables"`
	Fields int `json:"fields"`
}

type clientConfigTable struct {
	Name string `json:"name"`
	// Type 是 schema 结构体名，也是 C# 里的行类型 / 单例类型名。
	Type string `json:"type"`
	Kind string `json:"kind"`
	// Key 是主键的 JSON 名，只有普通表有。
	Key  string `json:"key,omitempty"`
	File string `json:"file"`
	Rows int    `json:"rows"`
	// SchemaHash、DataHash 是 tablegen 导出模型里的哈希（sha256 十六进制，数据按 LF）；C# 加载时核对的就是 DataHash。
	SchemaHash string              `json:"schema_hash"`
	DataHash   string              `json:"data_hash"`
	Fields     []clientConfigField `json:"fields"`
}

type clientConfigField struct {
	Name string `json:"name"` // JSON 键
	Type string `json:"type"` // schema 里的 Go 类型表达式
	// Parser 非空表示字段用自定义解析器：客户端拿到的是 CSV 原文字符串。
	Parser string `json:"parser,omitempty"`
}

// clientConfigExport 是一次配置导出的结果：文件（键相对 <dir>）与清单。
type clientConfigExport struct {
	files    map[string][]byte
	manifest clientConfigManifest
}

// clientConfigDataFile 是一张表在导出目录里的数据文件（相对 <dir>）；C# 的 readText 收到的是它的文件名部分。
func clientConfigDataFile(table tablegen.ExportTable) string {
	return "data/" + table.Name + ".json"
}

var clientConfigTableName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// selectConfigTables 按 client_export.config.tables 选表：为空取全部；列出的表必须在客户端投影里（存在且带客户端字段）。
// 返回选中的表与因此排除的表数。
func selectConfigTables(spec *ClientConfigExportSpec, model *tablegen.ExportModel) ([]tablegen.ExportTable, int, error) {
	if len(spec.Tables) == 0 {
		return model.Tables, 0, nil
	}
	byName := make(map[string]tablegen.ExportTable, len(model.Tables))
	for _, table := range model.Tables {
		byName[table.Name] = table
	}
	wanted := map[string]bool{}
	var missing []string
	for _, name := range spec.Tables {
		if _, ok := byName[name]; !ok {
			missing = append(missing, name)
		}
		wanted[name] = true
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(model.Tables))
		for _, table := range model.Tables {
			names = append(names, table.Name)
		}
		return nil, 0, fmt.Errorf("client_export.config.tables lists %s, which is not a table with client fields (no such //roost:table / //roost:object name, or it is side=s or all its fields are side:\"s\"); tables with client fields: %s",
			strings.Join(quoteAll(missing), ", "), strings.Join(names, ", "))
	}
	var selected []tablegen.ExportTable
	for _, table := range model.Tables {
		if wanted[table.Name] {
			selected = append(selected, table)
		}
	}
	return selected, len(model.Tables) - len(selected), nil
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fmt.Sprintf("%q", value)
	}
	return out
}

// buildClientConfigExport 算出配置导出目录的全部文件（键相对 <dir>）。只读 configs/schema 与 configs/table，不跑别的生成器。
func buildClientConfigExport(root string, m Manifest) (clientConfigExport, error) {
	spec := m.ClientExport.Config
	model, err := tablegen.ClientExport(under(root, tablegen.DefaultMetaDir), under(root, "configs/table"))
	if err != nil {
		return clientConfigExport{}, fmt.Errorf("client_export.config: %w", err)
	}
	tables, filtered, err := selectConfigTables(spec, model)
	if err != nil {
		return clientConfigExport{}, err
	}
	for _, table := range tables {
		if !clientConfigTableName.MatchString(table.Name) {
			return clientConfigExport{}, fmt.Errorf("client_export.config: table name %q cannot be a file name; rename it (marker name=)", table.Name)
		}
	}
	namespace := spec.namespace()
	version := tablegen.DataVersion(tables)
	sources, err := renderConfigCSharp(tables, namespace, version)
	if err != nil {
		return clientConfigExport{}, fmt.Errorf("client_export.config: %w", err)
	}
	files := map[string][]byte{}
	manifest := clientConfigManifest{
		Kind:             clientConfigExportKind,
		Schema:           clientConfigExportSchema,
		Core:             projectCoreVersion(root),
		Generator:        codegenVersion(),
		DataVersion:      version,
		DataVersionShort: shortVersion(version),
		Namespace:        namespace,
		Omitted:          clientConfigOmitted{Tables: model.OmittedTables + filtered, Fields: model.OmittedFields},
		Tables:           make([]clientConfigTable, 0, len(tables)),
		Files:            map[string]string{},
	}
	for _, table := range tables {
		file := clientConfigDataFile(table)
		files[file] = table.Data
		entry := clientConfigTable{
			Name: table.Name, Type: table.TypeName, Kind: string(table.Kind), File: file, Rows: table.Rows,
			SchemaHash: table.SchemaHash, DataHash: table.DataHash, Fields: make([]clientConfigField, 0, len(table.Fields)),
		}
		for _, field := range table.Fields {
			if field.Key {
				entry.Key = field.JSON
			}
			entry.Fields = append(entry.Fields, clientConfigField{Name: field.JSON, Type: field.Type, Parser: field.Parser})
		}
		manifest.Tables = append(manifest.Tables, entry)
	}
	for name, raw := range sources {
		files["csharp/"+name] = raw
	}
	for name, raw := range files {
		manifest.Files[name] = contentHash(raw)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return clientConfigExport{}, err
	}
	files[exportManifest] = append(raw, '\n')
	return clientConfigExport{files: files, manifest: manifest}, nil
}

func shortVersion(version string) string {
	if len(version) < 12 {
		return version
	}
	return version[:12]
}

// generateClientConfigExport 是 roost generate 的配置导出步骤：整份写出 <dir>，删掉本轮不产出的 data/*.json 与 csharp/*.cs，
// 打印排除的表数 / 字段数与数据版本。
func generateClientConfigExport(root string, m Manifest, w io.Writer) error {
	export, err := buildClientConfigExport(root, m)
	if err != nil {
		return err
	}
	spec := m.ClientExport.Config
	dir := under(root, spec.dir())
	for _, sub := range []string{"data", "csharp"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := sub + "/" + entry.Name()
			if _, keep := export.files[name]; keep || !entry.Type().IsRegular() || !spec.owns(spec.dir()+"/"+name) {
				continue
			}
			target := filepath.Join(dir, sub, entry.Name())
			if err := os.Remove(target); err != nil {
				return err
			}
			fmt.Fprintf(w, "removed: %s\n", target)
		}
	}
	names := make([]string, 0, len(export.files))
	for name := range export.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("client config export target is not a regular file: %s", target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		changed, err := writeFileIfChanged(target, export.files[name])
		if err != nil {
			return err
		}
		printGenerated(w, target, changed)
	}
	manifest := export.manifest
	fmt.Fprintf(w, "client config export: %d tables, omitted %d tables / %d fields (server only or not listed in client_export.config.tables)\n",
		len(manifest.Tables), manifest.Omitted.Tables, manifest.Omitted.Fields)
	fmt.Fprintf(w, "client config data version: %s (%s)\n", manifest.DataVersionShort, manifest.DataVersion)
	return nil
}

// readClientConfigManifest 读并认出配置导出清单。
func readClientConfigManifest(file string) (clientConfigManifest, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return clientConfigManifest{}, err
	}
	var manifest clientConfigManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return clientConfigManifest{}, fmt.Errorf("%s: %w", file, err)
	}
	if manifest.Kind != clientConfigExportKind {
		return clientConfigManifest{}, fmt.Errorf("%s is not a roost config export manifest (kind %q)", file, manifest.Kind)
	}
	if manifest.Schema != clientConfigExportSchema {
		return clientConfigManifest{}, fmt.Errorf("%s has config export schema %d; this roost reads schema %d", file, manifest.Schema, clientConfigExportSchema)
	}
	return manifest, nil
}

// verifyConfigExportFiles 核对配置导出目录里每个登记文件的哈希：手改或拷了一半的目录在这里被拒。
func verifyConfigExportFiles(dir string, manifest clientConfigManifest) []string {
	var problems []string
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !validConfigExportFileName(name) {
			problems = append(problems, fmt.Sprintf("manifest lists unexpected file %q", name))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s is missing", name))
			continue
		}
		if contentHash(raw) != manifest.Files[name] {
			problems = append(problems, fmt.Sprintf("%s does not match manifest.json (edited by hand?)", name))
		}
	}
	return problems
}

// validConfigExportFileName 只认 data/<名字>.json 与 csharp/<名字>.cs：清单是从别处读来的，路径不能逃出导出目录。
func validConfigExportFileName(name string) bool {
	dir, base := path.Split(name)
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, `\:`) {
		return false
	}
	return dir == "data/" && strings.HasSuffix(base, ".json") || dir == "csharp/" && strings.HasSuffix(base, ".cs")
}

// clientExportChecks 是 doctor 里前后端公共目录的两项：导出协议时 client-export:fresh，声明了 config 时 config-export:fresh。
func clientExportChecks(root string, m Manifest) []CheckItem {
	var items []CheckItem
	if m.ClientExport.protocol() {
		items = append(items, checkClientExport(root, m))
	}
	if m.ClientExport != nil && m.ClientExport.Config != nil {
		items = append(items, checkClientConfigExport(root, m))
	}
	return items
}

// checkClientConfigExport 是 doctor 的 config-export:fresh：不重新生成（不写文件、不渲染 C#），只核对：
//   - manifest.json 存在、每个登记文件的哈希一致（没有手改）；
//   - 按 configs/schema 与 configs/table 现在的内容算出的客户端投影（tablegen.ClientExport，选表规则同生成），
//     表的集合、每张表的 schema 哈希与数据哈希、数据版本都与 manifest 一致；
//   - C# 命名空间与 roost.yaml 一致。
//
// 导出整体是否最新（含 C# 模板与生成器版本）由 roost generate --check 负责。
func checkClientConfigExport(root string, m Manifest) CheckItem {
	const name = "config-export:fresh"
	spec := m.ClientExport.Config
	dir := under(root, spec.dir())
	fail := func(detail string) CheckItem {
		return CheckItem{Name: name, Status: StatusFail, Detail: detail + "; run roost generate and commit " + spec.dir()}
	}
	manifest, err := readClientConfigManifest(filepath.Join(dir, exportManifest))
	if os.IsNotExist(err) {
		return fail(spec.dir() + "/manifest.json is missing")
	}
	if err != nil {
		return fail(err.Error())
	}
	problems := verifyConfigExportFiles(dir, manifest)
	model, err := tablegen.ClientExport(under(root, tablegen.DefaultMetaDir), under(root, "configs/table"))
	if err != nil {
		return fail("configs/schema or configs/table: " + err.Error())
	}
	tables, _, err := selectConfigTables(spec, model)
	if err != nil {
		return fail(err.Error())
	}
	exported := map[string]clientConfigTable{}
	for _, table := range manifest.Tables {
		exported[table.Name] = table
	}
	current := map[string]bool{}
	for _, table := range tables {
		current[table.Name] = true
		old, ok := exported[table.Name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("table %s is not in the export", table.Name))
		case old.SchemaHash != table.SchemaHash:
			problems = append(problems, fmt.Sprintf("table %s: schema (configs/schema) changed since the export was generated", table.Name))
		case old.DataHash != table.DataHash:
			problems = append(problems, fmt.Sprintf("table %s: data (configs/table/%s) changed since the export was generated", table.Name, table.File))
		}
	}
	for _, table := range manifest.Tables {
		if !current[table.Name] {
			problems = append(problems, fmt.Sprintf("table %s is exported but no longer has client fields or is not selected", table.Name))
		}
	}
	version := tablegen.DataVersion(tables)
	if version != manifest.DataVersion {
		problems = append(problems, fmt.Sprintf("data version %s in manifest.json, %s from configs/table", shortVersion(manifest.DataVersion), shortVersion(version)))
	}
	if manifest.Namespace != spec.namespace() {
		problems = append(problems, fmt.Sprintf("C# namespace %s in manifest.json, %s in roost.yaml", manifest.Namespace, spec.namespace()))
	}
	if len(problems) > 0 {
		return fail(spec.dir() + " is stale: " + strings.Join(problems, "; "))
	}
	return CheckItem{Name: name, Status: StatusOK, Detail: fmt.Sprintf("%s matches configs/schema and configs/table: %d table(s), data version %s", spec.dir(), len(tables), manifest.DataVersionShort)}
}
