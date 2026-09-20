package attribute

type ProfileDef struct {
	Name       string
	Package    string
	IndexBegin int
	MaxCount   int
	Fields     []AttributeField
	Formulas   []FormulaDef
}

type AttributeField struct {
	Name      string
	Type      string
	AttrName  string
	ID        int
	Bit       int
	Derived   bool
	ConstName string
	MaskName  string
}

type FormulaDef struct {
	Method string
	Output int
	Inputs []int
}
