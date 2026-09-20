package protocol

type Definitions struct {
	ModulePath   string
	ProtoPackage string
	GoPackage    string
	Enums        []EnumDef
	Structs      []StructDef
	Messages     []MsgDef
	Pushes       []PushDef
}

type EnumDef struct {
	Name       string
	Underlying string
	Values     []EnumValueDef
}

type EnumValueDef struct {
	Name  string
	Value int32
}

type StructDef struct {
	Name       string
	Fields     []FieldDef
	Group      string
	SourceFile string
	SourceKind string
}

type MsgDef struct {
	Name            string
	Req             string
	ReqID           uint32
	Resp            string
	RespID          uint32
	Tags            []string
	Handler         string
	Group           string
	SourceFile      string
	SourceInterface string
}

type PushDef struct {
	Name            string
	Msg             string
	MsgID           uint32
	Tags            []string
	Group           string
	SourceFile      string
	SourceInterface string
}

type FieldDef struct {
	Name      string
	ProtoName string
	Number    int
	Oneof     string
	TypeStr   string
	Kind      FieldKind
	Scalar    ScalarKind
	TypeName  string
	SliceElem *FieldDef
	MapKey    *FieldDef
	MapVal    *FieldDef
	IsPtr     bool
	ProtoType string
	WireType  string
	GoType    string
}

type FieldKind int

const (
	KindScalar FieldKind = iota
	KindMessage
	KindEnum
	KindBytes
	KindRepeated
	KindMap
)

type ScalarKind int

const (
	ScalarNone ScalarKind = iota
	ScalarString
	ScalarBool
	ScalarInt32
	ScalarInt64
	ScalarUint32
	ScalarUint64
	ScalarFloat32
	ScalarFloat64
)
