package tablegen

import (
	"fmt"
	"go/token"
	"strings"
)

// 按端分组（维护者 2026-11-17，配置表导出给客户端 决定 1）：
//
//   - 字段 tag side:"c" 只给客户端、side:"s" 只给服务端，不写两端都要；整张表在标记里写
//     side=c / side=s。
//   - 服务端的 configs/data、_manifest.json 与 Go 绑定只含服务端投影：side=c 的表不登记，
//     只给客户端的字段不进 JSON。schema 结构体不另生成一份，只给客户端的字段在服务端仍然存在，
//     值是零值。
//   - 客户端导出（ClientExport）只含客户端投影，只给服务端的字段绝不出现。
//   - 规则沿用 cfggen 的 group：主键不能被排除；ref 指向的表必须也在同一端；一张表的字段全被
//     排除时报错；生成时打印 "omitted N tables / M fields"。
const (
	sideClient = "c"
	sideServer = "s"
)

func validSide(side string) bool {
	return side == "" || side == sideClient || side == sideServer
}

func sideName(side string) string {
	if side == sideClient {
		return "client"
	}
	return "server"
}

// omittedBySide 是一次投影排除的表数与（留下的表里）字段数。
type omittedBySide struct {
	tables int
	fields int
}

// projectSide 返回 side 端看到的表：另一端专用的表整张去掉，留下的表只保留本端字段
// （Fields），AllFields 保持全部字段供 CSV 表头核对。主键被排除、字段全被排除、ref 指向
// 不在本端的表都报错。没有任何 side 声明时返回的 Fields 与输入相同。
func projectSide(metas []Meta, side string) ([]Meta, omittedBySide, error) {
	var omitted omittedBySide
	tables := make(map[string]bool, len(metas))
	for _, meta := range metas {
		if meta.Kind == KindTable {
			tables[meta.Name] = true
		}
	}
	out := make([]Meta, 0, len(metas))
	kept := make(map[string]bool, len(metas))
	for _, meta := range metas {
		if meta.Side != "" && meta.Side != side {
			omitted.tables++
			continue
		}
		projected := meta
		projected.AllFields = meta.sheetFields()
		projected.Fields = make([]Field, 0, len(projected.AllFields))
		projected.Omitted = nil
		key := ""
		if meta.Kind == KindTable {
			key = keyFieldInfo(meta).Name
		}
		for _, field := range projected.AllFields {
			if field.Side == "" || field.Side == side {
				projected.Fields = append(projected.Fields, field)
				continue
			}
			if field.Name == key {
				return nil, omittedBySide{}, fmt.Errorf("table %s: key field %s is side:%q, but the key identifies a row on both sides", meta.Name, field.Name, field.Side)
			}
			omitted.fields++
			projected.Omitted = append(projected.Omitted, field.Name)
		}
		if len(projected.Fields) == 0 && len(projected.AllFields) > 0 {
			return nil, omittedBySide{}, fmt.Errorf("%s %s: every field is excluded from the %s side; mark the %s side=%s instead", meta.Kind, meta.Name, sideName(side), meta.Kind, otherSide(side))
		}
		if meta.Kind == KindTable {
			kept[meta.Name] = true
		}
		out = append(out, projected)
	}
	for _, meta := range out {
		for _, field := range meta.Fields {
			if field.Ref == "" || !tables[field.Ref] || kept[field.Ref] {
				// 目标不是表的情况由 resolveRefs 报（与按端无关）。
				continue
			}
			return nil, omittedBySide{}, fmt.Errorf("table %s field %s: ref target %q is not on the %s side (a ref must point at a table on the same side)", meta.Name, field.Name, field.Ref, sideName(side))
		}
	}
	return out, omitted, nil
}

func otherSide(side string) string {
	if side == sideClient {
		return sideServer
	}
	return sideClient
}

// validateMetas 核对与本次产出哪一样无关的声明：两端的投影都成立（见 projectSide）、index
// 声明可用。Run 与 ClientExport 都先调它，所以一处声明错误在服务端生成时就报，不等到导出。
func validateMetas(metas []Meta) error {
	for _, side := range []string{sideServer, sideClient} {
		if _, _, err := projectSide(metas, side); err != nil {
			return err
		}
	}
	for _, meta := range metas {
		if err := validateIndexes(meta); err != nil {
			return err
		}
	}
	return nil
}

// indexName 读字段的 index tag，写法同 cfggen：index:"true" 是以 Go 字段名命名的索引，
// index:"<名字>" 指定索引名，不写或 index:"false" 没有索引。
func indexName(field, tag string) (string, error) {
	switch tag {
	case "", "false":
		return "", nil
	case "true":
		return field, nil
	}
	if !token.IsIdentifier(tag) {
		return "", fmt.Errorf("field %s: index:%q must be true, false or a Go identifier naming the index", field, tag)
	}
	return tag, nil
}

// indexTypes 是可以建索引的字段类型：索引键是值的十进制 / 原文字符串（configdata.IndexDef）。
// 按字段的底层类型（Field.Resolved）查：schema 包里底层是这些类型的具名类型可以建索引；浮点、指针、
// 复合类型、别的包里的具名类型、底层不是内建标量的具名类型不支持（named.go）。
var indexTypes = map[string]bool{
	"string": true, "bool": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
}

// validateIndexes 核对一张表的 index 声明：只用于 //roost:table、字段类型可用、不在主键上
// （主键已有 <Type>By<Key>）、没有自定义解析器、索引名不重复，生成的访问器不与主键访问器同名。
func validateIndexes(meta Meta) error {
	seen := make(map[string]string)
	keyAccessor := ""
	if meta.Kind == KindTable {
		keyAccessor = meta.TypeName + "By" + keyField(meta)
	}
	for _, field := range meta.sheetFields() {
		if field.Index == "" {
			continue
		}
		if meta.Kind != KindTable {
			return fmt.Errorf("object %s field %s: index only applies to //roost:table rows", meta.Name, field.Name)
		}
		if field.Name == keyFieldInfo(meta).Name {
			return fmt.Errorf("table %s field %s: the key already has %s; index is for non-key fields", meta.Name, field.Name, keyAccessor)
		}
		if field.Parser != "" || !indexTypes[field.resolvedType()] {
			err := fmt.Errorf("table %s field %s: index needs a string, integer or bool field, got %s", meta.Name, field.Name, fieldTypeLabel(field))
			if reason := NamedTypeReason(field.resolvedType()); field.Parser == "" && reason != "" {
				err = fmt.Errorf("%w: %s", err, reason)
			}
			return err
		}
		if prev, dup := seen[field.Index]; dup {
			return fmt.Errorf("table %s: index %q is declared on both %s and %s", meta.Name, field.Index, prev, field.Name)
		}
		seen[field.Index] = field.Name
		if accessor := indexAccessor(meta, field); accessor == keyAccessor {
			return fmt.Errorf("table %s field %s: index accessor %s collides with the key accessor", meta.Name, field.Name, accessor)
		}
	}
	return nil
}

// fieldTypeLabel 是报错里的字段类型：schema 里的写法，具名标量后面括上底层类型（Rate (float64)）。
func fieldTypeLabel(field Field) string {
	if field.Parser != "" {
		return field.Type + " with parser:" + field.Parser
	}
	if resolved := field.resolvedType(); resolved != field.Type {
		return field.Type + " (" + resolved + ")"
	}
	return field.Type
}

// indexAccessor 是索引的服务端访问器名：<Type>By<Index>。
func indexAccessor(meta Meta, field Field) string {
	return meta.TypeName + "By" + firstUpper(field.Index)
}

// indexedFields 是本端字段里带索引的那些（生成模板用）。
func indexedFields(meta Meta) []Field {
	var out []Field
	for _, field := range meta.Fields {
		if field.Index != "" {
			out = append(out, field)
		}
	}
	return out
}

// indexKeyExpr 是生成代码里把字段值 v 变成索引键字符串的表达式，与 configdata 自动表
// （cfggen）的索引键一致：整数取十进制，bool 取 true / false，字符串原样。按底层类型选写法，
// 具名类型先转换成底层类型（string(v)、bool(v)），所以具名与内建类型的同一个值键相同。
func indexKeyExpr(field Field, v string) string {
	resolved := field.resolvedType()
	switch {
	case resolved == "string" && field.Type == "string":
		return v
	case resolved == "string":
		return "string(" + v + ")"
	case resolved == "bool" && field.Type == "bool":
		return "strconv.FormatBool(" + v + ")"
	case resolved == "bool":
		return "strconv.FormatBool(bool(" + v + "))"
	case strings.HasPrefix(resolved, "uint"):
		return "strconv.FormatUint(uint64(" + v + "), 10)"
	default:
		return "strconv.FormatInt(int64(" + v + "), 10)"
	}
}
