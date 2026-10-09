package configdata

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"

	"github.com/tjbdwanghaibo/roost-core/framework/configdata/rules"
)

// checkKeySpelling 核对一个数据文件里每个键的拼写：键必须与行类型声明的 json 名逐字
// 一致（rules.MisspelledKey，大小写敏感），嵌套的结构体、切片、map 的值按各自的类型
// 逐层核对。encoding/json 匹配键时大小写不敏感，"Level" 会静默填进 level 字段、
// 一行里几种拼写按文档顺序最后一个生效；这里在解码成功之后拒绝它们，错误是点名
// 表、行、行主键、应有拼写的 *RuleError（Rule "case"）。
//
// 只看与某个声明名只差大小写的键；未声明的键维持原行为（宽松模式忽略，严格模式已由
// 解码拒绝）。自带 UnmarshalJSON 的类型、interface 与 map 的键由作者决定，不核对。
func checkKeySpelling[V any](table string, payload []byte, object bool, keyOf func(int) string) error {
	walker := spellingWalker{fields: make(map[reflect.Type]jsonFields)}
	rowType := reflect.TypeOf((*V)(nil)).Elem()
	if object {
		if miss, ok := walker.walk(payload, rowType, ""); ok {
			return rules.CaseError(table, 0, "", miss.field, miss.key, miss.want)
		}
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return err // 不可达：同一份载荷刚解码成功
	}
	for i, row := range rows {
		if miss, ok := walker.walk(row, rowType, ""); ok {
			return rules.CaseError(table, i+1, keyOf(i), miss.field, miss.key, miss.want)
		}
	}
	return nil
}

type misspelling struct{ field, key, want string }

// jsonFields 是一个结构体类型的 json 名（声明顺序）与各自的值类型。
type jsonFields struct {
	names []string
	types map[string]reflect.Type
}

// spellingWalker 在一次加载里缓存每个结构体类型的 json 名。
type spellingWalker struct {
	fields map[reflect.Type]jsonFields
}

var jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// walk 沿 typ 走 value，返回第一处拼写违反（结构体按键的字节序，切片按下标）。
func (w spellingWalker) walk(value json.RawMessage, typ reflect.Type, path string) (misspelling, bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Implements(jsonUnmarshalerType) || reflect.PointerTo(typ).Implements(jsonUnmarshalerType) {
		return misspelling{}, false
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return misspelling{}, false
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(trimmed, &object) != nil {
			return misspelling{}, false
		}
		fields := w.structFields(typ)
		keys := slices.Sorted(maps.Keys(object))
		if key, want, found := rules.MisspelledKey(keys, fields.names); found {
			return misspelling{field: joinPath(path, want), key: key, want: want}, true
		}
		for _, key := range keys {
			if fieldType, ok := fields.types[key]; ok {
				if miss, found := w.walk(object[key], fieldType, joinPath(path, key)); found {
					return miss, true
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return misspelling{}, false // []byte 是 base64 字符串
		}
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return misspelling{}, false
		}
		for i, item := range items {
			if miss, found := w.walk(item, typ.Elem(), path+"["+strconv.Itoa(i)+"]"); found {
				return miss, true
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if json.Unmarshal(trimmed, &object) != nil {
			return misspelling{}, false
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if miss, found := w.walk(object[key], typ.Elem(), path+"["+strconv.Quote(key)+"]"); found {
				return miss, true
			}
		}
	}
	return misspelling{}, false
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// structFields 列出 encoding/json 会从 JSON 对象里填的字段名：导出字段的 json 名（无
// 标签时是 Go 字段名，json:"-" 不算），没有 json 名的嵌入结构体（含指针嵌入）把内层
// 字段提升上来；同名时浅层优先。
func (w spellingWalker) structFields(typ reflect.Type) jsonFields {
	if cached, ok := w.fields[typ]; ok {
		return cached
	}
	fields := jsonFields{types: make(map[string]reflect.Type)}
	var embedded []reflect.Type
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, ok := decodedJSONName(field)
		if field.Anonymous && jsonTagName(field) == "" {
			inner := field.Type
			if inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				embedded = append(embedded, inner)
				continue
			}
		}
		if !ok || !field.IsExported() {
			continue
		}
		if _, dup := fields.types[name]; !dup {
			fields.names = append(fields.names, name)
			fields.types[name] = field.Type
		}
	}
	for _, inner := range embedded {
		promoted := w.structFields(inner)
		for _, name := range promoted.names {
			if _, dup := fields.types[name]; !dup {
				fields.names = append(fields.names, name)
				fields.types[name] = promoted.types[name]
			}
		}
	}
	w.fields[typ] = fields
	return fields
}
