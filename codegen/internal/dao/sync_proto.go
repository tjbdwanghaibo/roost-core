package dao

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
)

// 同步的类型系统与持久 BSON 分开：顶层字段包装表示 patch 的存在性，
// 容器里的可空对象另用 Optional 包装，保留 nil 元素及其位置。
type syncShape struct{ stored, protobuf, encode, decode string }
type syncSchema struct {
	defs     *Definitions
	messages []protocol.StructDef
	helpers  strings.Builder
	shapes   map[string]syncShape
}

func syncField(name, typ string, number int) protocol.FieldDef {
	return protocol.FieldDef{Name: name, ProtoName: toSnake(name), TypeStr: typ, Number: number}
}
func (s *syncSchema) shape(typeName string) (syncShape, error) {
	if shape, ok := s.shapes[typeName]; ok {
		return shape, nil
	}
	expr, err := parser.ParseExpr(typeName)
	if err != nil {
		return syncShape{}, err
	}
	shape, err := s.expression(expr)
	if err == nil {
		if s.shapes == nil {
			s.shapes = make(map[string]syncShape)
		}
		s.shapes[typeName] = shape
	}
	return shape, err
}
func identityShape(typ string) syncShape { return syncShape{typ, typ, "%s", "%s"} }
func (s *syncSchema) expression(expr ast.Expr) (syncShape, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		if isNestedType(s.defs, t.Name) {
			return syncShape{wireDocName(t.Name), "Sync" + t.Name, "sync" + t.Name + "ToPB(%s)", "sync" + t.Name + "FromPB(%s)"}, nil
		}
		if !isBasicType(t.Name) {
			return syncShape{}, fmt.Errorf("unsupported sync type %s", t.Name)
		}
		if t.Name == "int" {
			return syncShape{"int", "int64", "int64(%s)", "int(%s)"}, nil
		}
		if t.Name == "uint" {
			return syncShape{"uint", "uint64", "uint64(%s)", "uint(%s)"}, nil
		}
		return identityShape(t.Name), nil
	case *ast.StarExpr:
		child, err := s.expression(t.X)
		if err != nil {
			return syncShape{}, err
		}
		name, ok := t.X.(*ast.Ident)
		if !ok || !isNestedType(s.defs, name.Name) {
			return syncShape{}, fmt.Errorf("unsupported sync pointer")
		}
		return syncShape{"*" + child.stored, "SyncOptional" + name.Name, "syncOptional" + name.Name + "ToPB(%s)", "syncOptional" + name.Name + "FromPB(%s)"}, nil
	case *ast.ArrayType:
		if t.Len != nil {
			return syncShape{}, fmt.Errorf("fixed sync array is not supported")
		}
		child, err := s.expression(t.Elt)
		if err != nil {
			return syncShape{}, err
		}
		if child.stored == "byte" || child.stored == "uint8" {
			return identityShape("[]byte"), nil
		}
		return syncShape{"[]" + child.stored, "[]" + child.protobuf,
			fmt.Sprintf("syncSliceValues(%%s, func(value %s) %s { return %s })", child.stored, child.protobuf, fmt.Sprintf(child.encode, "value")),
			fmt.Sprintf("syncSliceValues(%%s, func(value %s) %s { return %s })", child.protobuf, child.stored, fmt.Sprintf(child.decode, "value"))}, nil
	case *ast.MapType:
		key, err := s.expression(t.Key)
		if err != nil {
			return syncShape{}, err
		}
		value, err := s.expression(t.Value)
		if err != nil {
			return syncShape{}, err
		}
		// Protobuf map 不能直接存 repeated，生成显式列表消息；bytes 是合法 map value。
		if strings.HasPrefix(value.protobuf, "[]") && value.protobuf != "[]byte" {
			wrapper := fmt.Sprintf("SyncList%d", len(s.messages))
			s.messages = append(s.messages, protocol.StructDef{Name: wrapper, Fields: []protocol.FieldDef{syncField("Values", value.protobuf, 1)}})
			value.encode = wrapper + "{Values:" + value.encode + "}"
			value.decode = fmt.Sprintf(value.decode, "(%s).Values")
			value.protobuf = wrapper
		}
		stored := "map[" + key.stored + "]" + value.stored
		pb := "map[" + key.protobuf + "]" + value.protobuf
		return syncShape{stored, pb,
			fmt.Sprintf("syncMapValues(%%s, func(key %s, value %s) (%s,%s) { return %s,%s })", key.stored, value.stored, key.protobuf, value.protobuf, fmt.Sprintf(key.encode, "key"), fmt.Sprintf(value.encode, "value")),
			fmt.Sprintf("syncMapValues(%%s, func(key %s, value %s) (%s,%s) { return %s,%s })", key.protobuf, value.protobuf, key.stored, value.stored, fmt.Sprintf(key.decode, "key"), fmt.Sprintf(value.decode, "value"))}, nil
	default:
		return syncShape{}, fmt.Errorf("unsupported sync expression %T", expr)
	}
}

func buildSyncSchema(defs *Definitions, pkg string) ([]byte, []byte, error) {
	anySync := false
	for _, d := range defs.Daos {
		anySync = anySync || len(filterSync(d.Fields)) > 0
	}
	if !anySync {
		return nil, nil, nil
	}
	s := &syncSchema{defs: defs}
	// 嵌套 wireDoc 只作为内存快照使用；这里从不调用 BSON marshal。
	for _, nested := range defs.Nested {
		message := protocol.StructDef{Name: "Sync" + nested.Name}
		var to, from strings.Builder
		fmt.Fprintf(&to, "func sync%sToPB(value %s) Sync%s { return Sync%s{\n", nested.Name, wireDocName(nested.Name), nested.Name, nested.Name)
		fmt.Fprintf(&from, "func sync%sFromPB(value Sync%s) %s { return %s{\n", nested.Name, nested.Name, wireDocName(nested.Name), wireDocName(nested.Name))
		for i, field := range nested.Fields {
			shape, err := s.shape(field.TypeStr)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.%s: %w", nested.Name, field.Name, err)
			}
			message.Fields = append(message.Fields, syncField(field.Name, shape.protobuf, i+1))
			fmt.Fprintf(&to, "%s:%s,\n", field.Name, fmt.Sprintf(shape.encode, "value."+field.Name))
			fmt.Fprintf(&from, "%s:%s,\n", field.Name, fmt.Sprintf(shape.decode, "value."+field.Name))
		}
		to.WriteString("} }\n")
		from.WriteString("} }\n")
		s.helpers.WriteString(to.String())
		s.helpers.WriteString(from.String())
		s.messages = append(s.messages, message, protocol.StructDef{Name: "SyncOptional" + nested.Name, Fields: []protocol.FieldDef{syncField("Value", "*Sync"+nested.Name, 1)}})
		fmt.Fprintf(&s.helpers, "func syncOptional%sToPB(value *%s) SyncOptional%s { if value==nil{return SyncOptional%s{}}; converted:=sync%sToPB(*value);return SyncOptional%s{Value:&converted} }\n", nested.Name, wireDocName(nested.Name), nested.Name, nested.Name, nested.Name, nested.Name)
		fmt.Fprintf(&s.helpers, "func syncOptional%sFromPB(value SyncOptional%s) *%s { if value.Value==nil{return nil};converted:=sync%sFromPB(*value.Value);return &converted }\n", nested.Name, nested.Name, wireDocName(nested.Name), nested.Name)
	}
	if err := s.buildNestedPatches(); err != nil {
		return nil, nil, err
	}
	for _, dao := range defs.Daos {
		fields := filterSync(dao.Fields)
		if len(fields) == 0 {
			continue
		}
		message := protocol.StructDef{Name: "Sync" + dao.Name, Fields: []protocol.FieldDef{syncField("EntityID", "int64", 1), syncField("SyncFormat", "uint32", 2047)}}
		var marshal, apply strings.Builder
		shapes := make(map[string]syncShape)
		var values strings.Builder
		fmt.Fprintf(&values, "// Values 读取字段替换或已应用快照；集合补丁必须先通过 Apply 合并，避免丢失删除与列表操作。\nfunc(patch *Sync%s) Values() (map[string]any,error) { if patch==nil || patch.SyncFormat!=3{return nil,fmt.Errorf(\"unsupported DAO sync format\")};out:=map[string]any{}\n// 增量补丁不带实体 ID（沿用基线的），这时没有 _id。\nif patch.EntityID!=0{out[\"_id\"]=patch.EntityID}\n", dao.Name)
		fmt.Fprintf(&marshal, "func(d *%[1]s) marshalSyncPB(mask uint64) []byte { return d.marshalSyncPatch(mask,d.id) }\n// marshalSyncPatch 编码字段替换补丁；id 为 0 时不带实体 ID（增量：接收端沿用基线的）。\nfunc(d *%[1]s) marshalSyncPatch(mask uint64,id int64) []byte { patch:=&Sync%[1]s{EntityID:id,SyncFormat:3}; changed:=false\n", dao.Name)
		fmt.Fprintf(&apply, `func(d *%[1]s) applySyncPB(raw []byte) error {
 patch,err:=UnmarshalSync%[1]s(raw);if err!=nil{return err}
 // 增量补丁不带实体 ID，沿用这份 DAO 的。
 if patch.EntityID==0{patch.EntityID=d.id}
 if d.id!=0 && d.id!=patch.EntityID{return fmt.Errorf("DAO identity changed")}
 previous,err:=UnmarshalSync%[1]s(d.marshalSyncPB(^uint64(0)));if err!=nil{return err}
 previous.EntityID=patch.EntityID
 merged,err:=ApplySync%[1]s(patch,previous);if err!=nil{return err}
`, dao.Name)
		for i, field := range dao.Fields {
			if !field.Tag.Sync || field.Tag.Skip {
				continue
			}
			shape, err := s.shape(field.TypeStr)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.%s: %w", dao.Name, field.Name, err)
			}
			if field.Name == "EntityID" || field.Name == "SyncFormat" {
				return nil, nil, fmt.Errorf("%s.%s: reserved DAO sync metadata field", dao.Name, field.Name)
			}
			wrapper := "Sync" + dao.Name + field.Name + "Field"
			s.trackedWrapper(wrapper, field, shape)
			shapes[field.Name] = shape
			message.Fields = append(message.Fields, syncField(field.Name, "*"+wrapper, i+2))
			if strings.HasPrefix(shape.protobuf, "map[") || (strings.HasPrefix(shape.protobuf, "[]") && shape.protobuf != "[]byte") {
				fmt.Fprintf(&values, "if patch.%[1]s!=nil && patch.%[1]s.Delta{return nil,fmt.Errorf(\"%[1]s: apply collection delta before Values\")}\n", field.Name)
			}
			if syncNestedName(defs, field) != "" && field.Kind != KindMap && !syncIsList(shape) {
				fmt.Fprintf(&values, "if patch.%[1]s!=nil&&patch.%[1]s.Patch!=nil{return nil,fmt.Errorf(\"apply nested patch before Values\")}\n", field.Name)
			}
			fmt.Fprintf(&values, "if patch.%s!=nil {out[%q]=patch.%s.Value}\n", field.Name, field.Key, field.Name)
			source := "d." + safeFieldVarName(field.Name)
			if field.Kind == KindMap {
				source = "d." + mapHelperName(dao.Name, field.Name) + "()"
			}
			encoded := fmt.Sprintf(shape.encode, toWire(defs, field, source))
			fmt.Fprintf(&marshal, "if mask&%s!=0 { patch.%s=&%s{Value:%s};changed=true }\n", fieldMaskName(dao.Name, field.Name), field.Name, wrapper, encoded)
			decoded := fromWire(defs, field, fmt.Sprintf(shape.decode, "merged."+field.Name+".Value"))
			fmt.Fprintf(&apply, "if patch.%s!=nil {\n", field.Name)
			if field.Kind == KindMap {
				fmt.Fprintf(&apply, "d.set%sRawMap(%s)\n", field.Name, decoded)
			} else {
				fmt.Fprintf(&apply, "d.%s=%s\n", safeFieldVarName(field.Name), decoded)
			}
			apply.WriteString("}\n")
		}
		marshal.WriteString("if !changed{return nil};raw,err:=MarshalSync" + dao.Name + "(patch);if err!=nil{panic(err)};return raw }\n")
		apply.WriteString("d.id=patch.EntityID;d.Init();return nil }\n")
		values.WriteString("return out,nil }\n")
		s.helpers.WriteString(values.String())
		s.helpers.WriteString(marshal.String())
		s.helpers.WriteString(apply.String())
		s.messages = append(s.messages, message)
		s.emitDAOTracked(dao, shapes)
	}
	if len(s.messages) == 0 {
		return nil, nil, nil
	}
	goSource, protoSource, err := protocol.DataSchema(pkg, "roost.dao.sync", s.messages)
	if err != nil {
		return nil, nil, err
	}
	goSource = bytes.Replace(goSource, []byte("Code generated by tool/protocol."), []byte("Code generated by tool/dao."), 1)
	goSource = append(goSource, []byte(s.helpers.String())...)
	goSource = bytes.Replace(goSource, []byte("import ("), []byte("import (\n\"github.com/tjbdwanghaibo/roost-core/framework/dataengine\""), 1)
	goSource = append(goSource, []byte(`
func syncSliceValues[A,B any](values []A, convert func(A)B) []B { if values==nil{return nil};out:=make([]B,len(values));for i,value:=range values{out[i]=convert(value)};return out }
func syncMapValues[K comparable,V any,L comparable,W any](values map[K]V,convert func(K,V)(L,W)) map[L]W {if values==nil{return nil};out:=make(map[L]W,len(values));for key,value:=range values{k,v:=convert(key,value);out[k]=v};return out}
`)...)
	if bytes.Contains(goSource, []byte("reflect.")) {
		goSource = bytes.Replace(goSource, []byte("import ("), []byte("import (\n\"reflect\""), 1)
	}
	protoSource = append([]byte(generatedHeader+"\n"), protoSource...)
	goSource, err = format.Source(goSource)
	return goSource, protoSource, err
}

// C# 的 PB 消息用于解码 patch，Apply 替换存在的字段，绝不使用 MergeFrom 追加 map/list。
func buildSyncCSharp(defs *Definitions) []byte { return buildTrackedSyncCSharp(defs) }

// 与 protoc C# 的字段属性命名一致：分隔符和数字开启下一个词，生成成员名加下划线。
func syncCSharpProperty(protoName, messageName string) string {
	var b strings.Builder
	upper := true
	for _, c := range protoName {
		switch {
		case c >= 'a' && c <= 'z':
			if upper {
				c -= 'a' - 'A'
			}
			b.WriteRune(c)
			upper = false
		case c >= 'A' && c <= 'Z':
			b.WriteRune(c)
			upper = false
		case c >= '0' && c <= '9':
			b.WriteRune(c)
			upper = true
		default:
			upper = true
		}
	}
	name := b.String()
	if name == messageName {
		return name + "_"
	}
	switch name {
	case "Parser", "Descriptor", "Clone", "Types", "CalculateSize", "MergeFrom", "Equals", "GetHashCode", "ToString", "WriteTo", "OnConstruction":
		return name + "_"
	}
	return name
}
