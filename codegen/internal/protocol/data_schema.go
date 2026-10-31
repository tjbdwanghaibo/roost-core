package protocol

import (
	"bytes"
	"fmt"
	"go/parser"
)

// DataSchema 为 DAO 等生成器复用正式 PB 编解码器，不建立业务路由或控制器。
// Fields 只需提供 Name、ProtoName、Number、TypeStr；类型与字段编号使用同一套验证。
func DataSchema(pkg, protoPackage string, messages []StructDef) (goSource, protoSource []byte, err error) {
	defs := &Definitions{ProtoPackage: protoPackage, GoPackage: "example.com/generated/" + pkg + ";" + pkg, Structs: messages}
	for i := range defs.Structs {
		for j := range defs.Structs[i].Fields {
			original := defs.Structs[i].Fields[j]
			expr, e := parser.ParseExpr(original.TypeStr)
			if e != nil {
				return nil, nil, e
			}
			field := classifyField(expr)
			field.Name, field.ProtoName, field.Number, field.TypeStr = original.Name, original.ProtoName, original.Number, original.TypeStr
			defs.Structs[i].Fields[j] = field
		}
	}
	if err = validateDefinitions(defs); err != nil {
		return nil, nil, fmt.Errorf("data schema: %w", err)
	}
	goSource, err = generatePBGo(defs)
	if err != nil {
		return nil, nil, err
	}
	goSource = bytes.Replace(goSource, []byte("package pb\n"), []byte("package "+pkg+"\n"), 1)
	protoSource, err = generateProto(defs)
	protoSource = bytes.Replace(protoSource, []byte(fmt.Sprintf("option go_package = %q;\n\n", defs.GoPackage)), nil, 1)
	protoSource = append(bytes.TrimRight(protoSource, "\n"), '\n')
	return
}
