package tablegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/framework/configdata/rules"
)

// 竖排键值表（维护者 2026-11-17，配置表导出给客户端 决定 2）：//roost:object layout=kv。
//
// 字段与类型仍由 Go 结构体定义；CSV 竖排三列 key,value,note，一行对应一个字段。key 是字段的
// csv 名，value 按字段类型解析，note 写进生成代码的注释。生成的 JSON 与普通 object 相同（单个
// 对象）。key 未知、重复，或必填字段没有对应行，都报错并写明文件、行与 key。# 开头的行是注释。
const layoutKV = "kv"

// kvObject 是读出的一张竖排键值表：values 是本端字段的 JSON 值（只含 CSV 里出现了的 key），
// notes 是每个字段（全部字段，按 csv 名）的 note。
type kvObject struct {
	values map[string]any
	notes  map[string]string
}

// readKVObject 读竖排键值 CSV。key 的核对按表的全部字段（AllFields）做：另一端字段的行是合法
// 的 key，只是不进本端的 values；必填按全部字段核对，所以同一张表两端看到的是同一份检查。
func readKVObject(path string, meta Meta) (kvObject, error) {
	name := filepath.Base(path)
	records, lines, err := readCSVFile(path, -1)
	if err != nil {
		return kvObject{}, err
	}
	sheet := meta.sheetFields()
	byCSV := make(map[string]Field, len(sheet))
	csvNames := fieldValues(sheet, func(f Field) string { return f.CSV })
	for _, field := range sheet {
		byCSV[field.CSV] = field
	}
	local := make(map[string]bool, len(meta.Fields))
	for _, field := range meta.Fields {
		local[field.CSV] = true
	}
	out := kvObject{values: make(map[string]any), notes: make(map[string]string)}
	seen := make(map[string]int)
	header := false
	for i, record := range records {
		line := lines[i]
		if isEmptyRow(record) {
			continue
		}
		if !header {
			if err := checkKVHeader(record); err != nil {
				return kvObject{}, fmt.Errorf("%s:%d: %w", name, line, err)
			}
			header = true
			continue
		}
		key := strings.TrimSpace(record[0])
		field, ok := byCSV[key]
		if !ok {
			if _, want, misspelled := rules.MisspelledKey([]string{key}, csvNames); misspelled {
				return kvObject{}, fmt.Errorf("%s:%d: key %q must be spelled %q (keys are case-sensitive)", name, line, key, want)
			}
			return kvObject{}, fmt.Errorf("%s:%d: unknown key %q (object %s has no field with that csv name)", name, line, key, meta.Name)
		}
		if first, dup := seen[key]; dup {
			return kvObject{}, fmt.Errorf("%s:%d: duplicate key %q (first at line %d)", name, line, key, first)
		}
		seen[key] = line
		value, note := cell(record, 1), cell(record, 2)
		if note != "" {
			out.notes[key] = note
		}
		if !local[key] {
			continue
		}
		parsed, err := parseCell(value, field)
		if err != nil {
			return kvObject{}, fmt.Errorf("%s:%d: key %q field=%s value=%q: %w", name, line, key, field.Name, value, err)
		}
		out.values[field.JSON] = parsed
	}
	for _, field := range sheet {
		if _, ok := seen[field.CSV]; field.Required && !ok {
			return kvObject{}, fmt.Errorf("%s: required key %q is missing (object %s field %s)", name, field.CSV, meta.Name, field.Name)
		}
	}
	// 与横排 object 相同：按 JSON 由同一组规则、同一个检查器核对（空的必填 value 是 null）。
	row, err := jsonRow(out.values)
	if err != nil {
		return kvObject{}, err
	}
	if err := rules.CheckObject(meta.Name, row, metaRules(meta)); err != nil {
		return kvObject{}, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// checkKVHeader 要求第一条非空记录是表头 key,value,note（note 可省），其后不能再有非空列。
func checkKVHeader(record []string) error {
	want := []string{"key", "value", "note"}
	for i, value := range record {
		value = strings.TrimSpace(value)
		if i < len(want) && (value == want[i] || (i == 2 && value == "")) {
			continue
		}
		if i >= len(want) && value == "" {
			continue
		}
		return fmt.Errorf("a layout=kv sheet starts with the header key,value,note, got %q", strings.Join(record, ","))
	}
	if len(record) < 2 {
		return fmt.Errorf("a layout=kv sheet starts with the header key,value,note, got %q", strings.Join(record, ","))
	}
	return nil
}

func cell(record []string, i int) string {
	if i < len(record) {
		return strings.TrimSpace(record[i])
	}
	return ""
}

// kvTemplateRows 是 layout=kv 表的 CSV 模板：表头加每个字段一行（value 留空，note 预填标题）。
func kvTemplateRows(meta Meta) [][]string {
	rows := [][]string{{"key", "value", "note"}}
	for _, field := range meta.Fields {
		rows = append(rows, []string{field.CSV, "", field.Title})
	}
	return rows
}

// attachKVNotes 把 layout=kv 表的 note 填进 Field.Note，供生成 Go 时写注释。CSV 还不存在时
// 跳过（新工程先有 schema 后有 CSV；缺 CSV 由数据转换那一步报）。
func attachKVNotes(metas []Meta, csvDir string) error {
	for i := range metas {
		meta := &metas[i]
		if meta.Layout != layoutKV {
			continue
		}
		path := filepath.Join(csvDir, meta.File)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		object, err := readKVObject(path, *meta)
		if err != nil {
			return err
		}
		fields := make([]Field, len(meta.Fields))
		for j, field := range meta.Fields {
			field.Note = object.notes[field.CSV]
			fields[j] = field
		}
		meta.Fields = fields
	}
	return nil
}

// jsonRow 把一个对象的值按 JSON 往返一次，得到规则检查器读的原始行。
func jsonRow(values map[string]any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	rows, err := rules.Rows(raw, true)
	if err != nil {
		return nil, err
	}
	return rows[0], nil
}

// cfgDoc 是生成的 <Type>Cfg 别名上的注释：layout=kv 表写出 CSV 的 note 列；有只给客户端的
// 字段时写明它们在服务端是零值。两样都没有时为空，生成结果与以前逐字节相同。
func cfgDoc(meta Meta) string {
	var b strings.Builder
	alias := meta.TypeName + "Cfg"
	if meta.Layout == layoutKV {
		fmt.Fprintf(&b, "// %s is the %s object, written as key,value,note rows in %s (layout=kv).\n", alias, meta.Name, meta.File)
		var notes []string
		for _, field := range meta.Fields {
			if field.Note != "" {
				notes = append(notes, fmt.Sprintf("//   - %s: %s\n", field.CSV, oneLine(field.Note)))
			}
		}
		if len(notes) > 0 {
			b.WriteString("//\n// Notes from the sheet:\n//\n")
			b.WriteString(strings.Join(notes, ""))
		}
	}
	if len(meta.Omitted) > 0 {
		if b.Len() == 0 {
			fmt.Fprintf(&b, "// %s is one %s %s.\n", alias, meta.Name, map[bool]string{true: "row", false: "object"}[meta.Kind == KindTable])
		}
		fmt.Fprintf(&b, "//\n// Client-only fields (side:\"c\") stay zero on the server: configs/data\n// carries server fields only. They are %s.\n", strings.Join(meta.Omitted, ", "))
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func hasKV(metas []Meta) bool {
	for _, meta := range metas {
		if meta.Layout == layoutKV {
			return true
		}
	}
	return false
}

// omittedCSV 是 kv 表里另一端字段的 csv 名，渲染成 case 子句的值列表（服务端接受这些 key，值留零值）。
func omittedCSV(meta Meta) string {
	local := make(map[string]bool, len(meta.Fields))
	for _, field := range meta.Fields {
		local[field.Name] = true
	}
	var quoted []string
	for _, field := range meta.sheetFields() {
		if !local[field.Name] {
			quoted = append(quoted, strconv.Quote(field.CSV))
		}
	}
	return strings.Join(quoted, ", ")
}

// requiredCSV 是 kv 表全部必填字段的 csv 名（两端共用一张表，必填按全部字段核对，与生成时一致）。
func requiredCSV(meta Meta) []string {
	var out []string
	for _, field := range meta.sheetFields() {
		if field.Required {
			out = append(out, field.CSV)
		}
	}
	return out
}
