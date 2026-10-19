package dao

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// 改字段告警（生成期，DESIGN-TODO D13，维护者 2026-10-19 决定：只告警，不阻断）。
//
// 生成器把输出目录里上一次的生成物当基线：DAO 的 <dao>PersistDoc（更早的生成物里是 Unmarshal 里的局部 rawDoc）、
// 嵌套类型的 <type>BSONDoc，按 bson 标签读出“键 -> 字段名、类型”，与这次的定义比较。键消失（改名、删字段、
// persist 改 nopersist）、新增、类型变化各打印一条 WARN，写明对已存数据的后果、迁移手册与 schema= 是否提高。
// 消失的键对应的旧值不再读取，下一次整文档写回会把它从文档里删掉（维护者 2026-10-19：旧字段不 load，靠业务约束与通知）。
// 基线只是工作树里的文件：同一份定义再生成一次不再告警，删掉生成物或换机器重新生成也看不到上一次的形状——
// 运行期的未知键告警（dataengine.StoredKeySet）是另一道。

// storedField 是持久文档里的一个键。
type storedField struct {
	Key, Field, Type string
}

type storedShape struct {
	fields    []storedField
	schema    uint32
	hasSchema bool
}

// readStoredShape 读 path 里名为 docType（或 legacyDocType）的结构体的键；文件不存在、不是本生成器的产物或没有
// 这个类型时 ok=false。schemaConst 非空时一并读出 schema 常量。
func readStoredShape(path, docType, legacyDocType, schemaConst string) (storedShape, bool) {
	content, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(content, []byte(generatedHeader)) {
		return storedShape{}, false
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, 0)
	if err != nil {
		return storedShape{}, false
	}
	var shape storedShape
	var current, legacy *ast.StructType
	ast.Inspect(file, func(node ast.Node) bool {
		switch spec := node.(type) {
		case *ast.TypeSpec:
			if st, ok := spec.Type.(*ast.StructType); ok {
				switch spec.Name.Name {
				case docType:
					current = st
				case legacyDocType:
					legacy = st
				}
			}
		case *ast.ValueSpec:
			if schemaConst != "" && len(spec.Names) == 1 && spec.Names[0].Name == schemaConst && len(spec.Values) == 1 {
				if lit, ok := spec.Values[0].(*ast.BasicLit); ok {
					if value, err := strconv.ParseUint(lit.Value, 10, 32); err == nil {
						shape.schema, shape.hasSchema = uint32(value), true
					}
				}
			}
		}
		return true
	})
	st := current
	if st == nil {
		st = legacy
	}
	if st == nil {
		return storedShape{}, false
	}
	for _, field := range st.Fields.List {
		if len(field.Names) != 1 || field.Tag == nil {
			continue
		}
		raw, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			continue
		}
		key, _, _ := strings.Cut(reflect.StructTag(raw).Get("bson"), ",")
		if key == "" || strings.HasPrefix(key, "_") {
			continue
		}
		shape.fields = append(shape.fields, storedField{Key: key, Field: field.Names[0].Name, Type: nodeString(fset, field.Type)})
	}
	return shape, true
}

// currentStoredFields 是这次定义要生成的持久文档的键（与模板里 {{wireType .}} 同一个类型）。
func currentStoredFields(defs *Definitions, fields []FieldDef) []storedField {
	out := make([]storedField, 0, len(fields))
	for _, field := range fields {
		out = append(out, storedField{Key: field.Key, Field: field.Name, Type: canonicalType(wireType(defs, field))})
	}
	return out
}

// storedKeyChanges 比较基线与这次的键，返回 WARN 行。owner 是 DAO 或嵌套类型名，previousFile 是基线文件名，
// schemaNote 说明 schema= 是否提高（嵌套类型给出“提高持有它的 DAO 的 schema=”）。
func storedKeyChanges(owner, previousFile string, before storedShape, after []storedField, schemaNote string) []string {
	now := make(map[string]storedField, len(after))
	for _, field := range after {
		now[field.Key] = field
	}
	was := make(map[string]storedField, len(before.fields))
	for _, field := range before.fields {
		was[field.Key] = field
	}
	var warnings []string
	for _, old := range before.fields {
		current, kept := now[old.Key]
		if !kept {
			warnings = append(warnings, fmt.Sprintf("WARN: %s: stored key %q (%s %s) is no longer generated (it was in the previous %s). "+
				"Documents already stored keep %q but it will not be read, and the next full write of each document deletes it; if this is a rename the new field starts at the zero value. "+
				"To rename the Go field and keep using the data, give it bson:%q; otherwise migrate the stored value offline before raising schema= (docs/framework/SCHEMA-MIGRATION.md). %s",
				owner, old.Key, old.Field, old.Type, previousFile, old.Key, old.Key, schemaNote))
			continue
		}
		if current.Type != old.Type {
			warnings = append(warnings, fmt.Sprintf("WARN: %s: stored key %q changes type %s -> %s (field %s). "+
				"Values already stored are decoded as the new type, and a value that cannot be decoded fails the load; "+
				"unless the types are compatible, migrate the stored documents offline (docs/framework/SCHEMA-MIGRATION.md). %s",
				owner, old.Key, old.Type, current.Type, current.Field, schemaNote))
		}
	}
	for _, field := range after {
		if _, existed := was[field.Key]; !existed {
			warnings = append(warnings, fmt.Sprintf("WARN: %s: new stored key %q (%s %s): documents already stored have no %q and load it as the zero value (adding a field needs no migration).",
				owner, field.Key, field.Field, field.Type, field.Key))
		}
	}
	return warnings
}

// daoStoredKeyWarnings 比较一个 Mongo DAO 与 outFile 里上一次的生成物。
func daoStoredKeyWarnings(dao DaoDef, defs *Definitions, outFile string) []string {
	before, ok := readStoredShape(outFile, persistDocName(dao.Name), "rawDoc", dao.Name+"SchemaVersion")
	if !ok {
		return nil
	}
	var after []storedField
	if !dao.NoCollection {
		after = currentStoredFields(defs, filterPersist(dao.Fields))
	}
	schema := dao.Schema
	if schema == 0 {
		schema = 1
	}
	note := fmt.Sprintf("schema= is %d.", schema)
	if before.hasSchema {
		if before.schema == schema {
			note = fmt.Sprintf("schema= is still %d: raise it with the migration so documents of the old shape are refused rather than half-read.", schema)
		} else {
			note = fmt.Sprintf("schema= was raised from %d to %d.", before.schema, schema)
		}
	}
	return storedKeyChanges(dao.Name, filepath.Base(outFile), before, after, note)
}

// nestedStoredKeyWarnings 比较一个嵌套类型的 wire 文档与上一次的生成物。
func nestedStoredKeyWarnings(nested NestedDef, defs *Definitions, outFile string) []string {
	before, ok := readStoredShape(outFile, wireDocName(nested.Name), "", "")
	if !ok {
		return nil
	}
	note := fmt.Sprintf("%s is stored inside its DAOs' documents: raise schema= on those DAOs with the migration.", nested.Name)
	return storedKeyChanges(nested.Name, filepath.Base(outFile), before, currentStoredFields(defs, nested.Fields), note)
}

func nodeString(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// canonicalType 把模板拼出的类型串按 gofmt 的写法规范化，与从生成物里读出的类型可比。
func canonicalType(typ string) string {
	expr, err := parser.ParseExpr(typ)
	if err != nil {
		return typ
	}
	return nodeString(token.NewFileSet(), expr)
}
