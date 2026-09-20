package protocol

import (
	"go/ast"
)

func classifyField(expr ast.Expr) FieldDef {
	switch t := expr.(type) {
	case *ast.Ident:
		return classifyIdent(t.Name)
	case *ast.SelectorExpr:
		return FieldDef{Kind: KindMessage, TypeName: exprString(t), GoType: exprString(t), ProtoType: exprString(t), WireType: "bytes"}
	case *ast.StarExpr:
		fd := classifyField(t.X)
		fd.IsPtr = true
		if fd.Kind == KindScalar || fd.Kind == KindBytes {
			fd.GoType = "*" + fd.GoType
		} else if fd.GoType != "" && fd.GoType[0] != '*' {
			fd.GoType = "*" + fd.GoType
		}
		return fd
	case *ast.ArrayType:
		if t.Len != nil {
			return FieldDef{Kind: KindBytes, GoType: exprString(t), ProtoType: "bytes", WireType: "bytes"}
		}
		elem := classifyField(t.Elt)
		if elem.Kind == KindScalar && elem.Scalar == ScalarUint32 && exprString(t.Elt) == "byte" {
			return FieldDef{Kind: KindBytes, GoType: "[]byte", ProtoType: "bytes", WireType: "bytes"}
		}
		return FieldDef{
			Kind:      KindRepeated,
			SliceElem: &elem,
			GoType:    "[]" + elem.GoType,
			ProtoType: "repeated " + elem.ProtoType,
			WireType:  elem.WireType,
		}
	case *ast.MapType:
		key := classifyField(t.Key)
		val := classifyField(t.Value)
		return FieldDef{
			Kind:      KindMap,
			MapKey:    &key,
			MapVal:    &val,
			GoType:    "map[" + key.GoType + "]" + val.GoType,
			ProtoType: "map<" + key.ProtoType + ", " + val.ProtoType + ">",
			WireType:  "bytes",
		}
	default:
		return FieldDef{Kind: KindMessage, TypeName: exprString(expr), GoType: exprString(expr), ProtoType: exprString(expr), WireType: "bytes"}
	}
}

func classifyIdent(name string) FieldDef {
	switch name {
	case "string":
		return FieldDef{Kind: KindScalar, Scalar: ScalarString, GoType: "string", ProtoType: "string", WireType: "bytes"}
	case "bool":
		return FieldDef{Kind: KindScalar, Scalar: ScalarBool, GoType: "bool", ProtoType: "bool", WireType: "varint"}
	case "int32", "int":
		return FieldDef{Kind: KindScalar, Scalar: ScalarInt32, GoType: name, ProtoType: "int32", WireType: "varint"}
	case "int64":
		return FieldDef{Kind: KindScalar, Scalar: ScalarInt64, GoType: "int64", ProtoType: "int64", WireType: "varint"}
	case "uint32", "byte":
		return FieldDef{Kind: KindScalar, Scalar: ScalarUint32, GoType: name, ProtoType: "uint32", WireType: "varint"}
	case "uint64":
		return FieldDef{Kind: KindScalar, Scalar: ScalarUint64, GoType: "uint64", ProtoType: "uint64", WireType: "varint"}
	case "float32":
		return FieldDef{Kind: KindScalar, Scalar: ScalarFloat32, GoType: "float32", ProtoType: "float", WireType: "fixed32"}
	case "float64":
		return FieldDef{Kind: KindScalar, Scalar: ScalarFloat64, GoType: "float64", ProtoType: "double", WireType: "fixed64"}
	default:
		return FieldDef{Kind: KindMessage, TypeName: name, GoType: name, ProtoType: name, WireType: "bytes"}
	}
}
