package tablegen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/framework/configdata/rules"
)

// ExportModel 是导出给客户端的配置表模型（配置表导出给客户端 §8 第 1 组交给第 2 组）：只含客户端
// 投影——side=s 的表与 side:"s" 的字段不在里面。表按 Name 排序；同样的输入（schema 与 CSV）
// 得到逐字节相同的模型。
type ExportModel struct {
	Tables []ExportTable
	// OmittedTables / OmittedFields 是客户端投影排除的表数与（留下的表里）字段数，
	// 供导出步骤打印 "omitted N tables / M fields"。
	OmittedTables int
	OmittedFields int
	// DataVersion 是客户端数据版本（决定 4：记录、可查看，登录时不核对）：表按表名排序，每张表写成
	// name + "\x00" + DataHash + "\n" 依次拼接，再取 sha256 的十六进制。表名算在里面，所以只改表名
	// 版本号也变；只改只给服务端的字段不会改变它。
	DataVersion string
}

// ShortDataVersion 是 DataVersion 的前 12 位，供显示。
func (m *ExportModel) ShortDataVersion() string {
	if len(m.DataVersion) < 12 {
		return m.DataVersion
	}
	return m.DataVersion[:12]
}

// ExportTable 是一张导出给客户端的表。
type ExportTable struct {
	// Name 是表名（标记 name=，缺省是类型名的 snake_case）；TypeName 是 schema 结构体名。
	Name     string
	TypeName string
	// Kind 是 table（多行）、object（横排单例）或 kv（layout=kv 的单例）。
	Kind TableKind
	// File 是 configs/table 下的 CSV 文件名，JSONFile 是数据文件名（标记 json=）。
	File     string
	JSONFile string
	// Key 是主键字段的 Go 名，只有 table 有。
	Key     string
	Fields  []ExportField
	Indexes []ExportIndex
	// Rules 是客户端字段上的规则，与服务端生成的 loader 是同一份翻译（metaRules）：table 的主键
	// 隐含 unique；ref 的目标保证也在客户端（projectSide 已核对）。Field 是 JSON 名。
	Rules []rules.Rule
	// Data 是只含客户端字段的 JSON：table 是 {"rows":[...]}，object / kv 是单个对象。两空格缩进、
	// 键按字典序、LF 换行、末尾一个换行。
	Data []byte
	// DataHash 是 Data 把 CRLF 统一成 LF 后的 sha256（十六进制）；SchemaHash 是客户端字段声明
	// （名字、JSON 名、类型、主键、规则、index、解析器）的 sha256。Rows 是行数，object / kv 记 1。
	DataHash   string
	SchemaHash string
	Rows       int
}

// ExportField 是一个客户端字段。
type ExportField struct {
	Name string // Go 字段名
	JSON string // JSON 键（schema 里的 json 名原样）
	CSV  string // CSV 列名 / kv 的 key
	// Type 是按底层类型展开后的 Go 类型表达式（Field.Resolved），如 int32、[]string、map[string]int64：
	// schema 包里的具名标量（type Rank int32）在这里是底层类型，客户端按它生成 C#。
	Type string
	Key  bool // 是表的主键
	// Title 是 title tag；Note 是 kv 表里这一行的 note 列（其它表为空）。
	Title string
	Note  string
	// Index 是二级索引名，空表示没有索引。
	Index string
	// Parser 非空表示字段用自定义解析器：JSON 里是 CSV 原始字符串，客户端按 string 处理、自己解析。
	Parser string
}

// ExportIndex 是一个二级索引：非唯一，按 Field 的值查全部匹配行（文件顺序）。
type ExportIndex struct {
	Name  string // 索引名（index:"true" 时是 Go 字段名）
	Field string // Go 字段名
	JSON  string // 该字段的 JSON 名
	Type  string // 底层类型，同 ExportField.Type
}

// ClientExport 读 metaDir 下的 schema 与 csvDir 下的 CSV，返回客户端投影的导出模型。声明错误
// （按端规则、index）与数据错误（CSV 解析、规则、kv 的 key）都在这里报，与服务端生成同一套核对。
func ClientExport(metaDir, csvDir string) (*ExportModel, error) {
	metas, err := parseMetaRoot(metaDir)
	if err != nil {
		return nil, err
	}
	if err := validateMetas(metas); err != nil {
		return nil, err
	}
	if err := resolveRefs(metas); err != nil {
		return nil, err
	}
	client, omitted, err := projectSide(metas, sideClient)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(client, func(i, j int) bool { return client[i].Name < client[j].Name })
	model := &ExportModel{OmittedTables: omitted.tables, OmittedFields: omitted.fields}
	names := make(map[string]bool, len(client))
	for _, meta := range client {
		if names[meta.Name] {
			return nil, fmt.Errorf("duplicate table name %q", meta.Name)
		}
		names[meta.Name] = true
		table, err := exportTable(csvDir, meta)
		if err != nil {
			return nil, err
		}
		model.Tables = append(model.Tables, table)
	}
	model.DataVersion = DataVersion(model.Tables)
	return model, nil
}

// DataVersion 是一组导出表的数据版本：表按表名排序，每张表写成 name + "\x00" + DataHash + "\n" 依次
// 拼接，再取 sha256 的十六进制。ClientExport 与只导出部分表（client_export.config.tables）的导出步骤
// 共用这一个算法。
func DataVersion(tables []ExportTable) string {
	sorted := append([]ExportTable(nil), tables...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	input := make([]byte, 0, len(sorted)*96)
	for _, table := range sorted {
		input = fmt.Appendf(input, "%s\x00%s\n", table.Name, table.DataHash)
	}
	return sha256Hex(input)
}

func exportTable(csvDir string, meta Meta) (ExportTable, error) {
	for _, rule := range metaRules(meta) {
		if err := rule.Validate(); err != nil {
			return ExportTable{}, fmt.Errorf("%s %s: %w", meta.Kind, meta.Name, err)
		}
	}
	table := ExportTable{
		Name:     meta.Name,
		TypeName: meta.TypeName,
		Kind:     meta.Kind,
		File:     meta.File,
		JSONFile: meta.JSON,
		Rules:    metaRules(meta),
	}
	if meta.Layout == layoutKV {
		table.Kind = KindKV
	}
	key := ""
	if meta.Kind == KindTable {
		key = keyFieldInfo(meta).Name
		table.Key = key
	}
	var notes map[string]string
	if meta.Layout == layoutKV {
		object, err := readKVObject(csvPath(csvDir, meta), meta)
		if err != nil {
			return ExportTable{}, err
		}
		notes = object.notes
	}
	for _, field := range meta.Fields {
		table.Fields = append(table.Fields, ExportField{
			Name: field.Name, JSON: field.JSON, CSV: field.CSV, Type: field.resolvedType(), Key: field.Name == key,
			Title: field.Title, Note: notes[field.CSV], Index: field.Index, Parser: field.Parser,
		})
		if field.Index != "" {
			table.Indexes = append(table.Indexes, ExportIndex{Name: field.Index, Field: field.Name, JSON: field.JSON, Type: field.resolvedType()})
		}
	}
	payload, rows, err := readPayload(csvDir, meta)
	if err != nil {
		return ExportTable{}, err
	}
	if meta.Kind == KindTable {
		list, _ := payload.([]map[string]any)
		if list == nil {
			list = []map[string]any{}
		}
		payload = map[string]any{"rows": list}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ExportTable{}, err
	}
	table.Data = append(data, '\n')
	table.DataHash = DataHash(table.Data)
	table.Rows = rows
	schema, err := json.Marshal(struct {
		Kind   TableKind
		Key    string
		Fields []ExportField
		Rules  []rules.Rule
	}{table.Kind, table.Key, schemaFields(table.Fields), table.Rules})
	if err != nil {
		return ExportTable{}, err
	}
	table.SchemaHash = sha256Hex(schema)
	return table, nil
}

// schemaFields 去掉不属于声明的 Note / Title（它们是说明文字，改了不算 schema 变化）。
func schemaFields(fields []ExportField) []ExportField {
	out := make([]ExportField, len(fields))
	for i, field := range fields {
		field.Note, field.Title = "", ""
		out[i] = field
	}
	return out
}

// DataHash 是数据文件的哈希：先把 CRLF 统一成 LF（防 Windows 上 git 自动转换换行后对不上），
// 再取 sha256 的十六进制。导出步骤与客户端核对用同一个算法。
func DataHash(data []byte) string {
	return sha256Hex(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func csvPath(csvDir string, meta Meta) string {
	return filepath.Join(csvDir, meta.File)
}
