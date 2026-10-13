package dao

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// 字段名首字母小写之后可能是 Go 关键字（`Type`、`Range`、`Map`、`Func` 都是 DAO 定义里自然的字段名）：生成物的私有名
// 必须避开关键字，包括嵌套结构体里的字段，否则整个包编译不过，错误还指向生成的临时文件。

func generateKeywordDao(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestAFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Type  int32  `+"`bson:\"type\"`"+`
	Range int64  `+"`bson:\"range\"`"+`
	Map   string `+"`bson:\"map\"`"+`
	Name  string `+"`bson:\"name\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_thing_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(generated)
	// The accessors keep the field's own name; only the private storage is
	// renamed, so a definition does not change meaning to its callers.
	for _, want := range []string{"func (d *ThingDao) GetType() int32", "func (d *ThingDao) SetType(", "func (d *ThingDao) GetRange() int64"} {
		if !strings.Contains(body, want) {
			t.Errorf("the generated DAO lost %q:\n%s", want, body)
		}
	}
}

// The same name inside a nested struct, which is where the demo hit it: the
// nested generator has its own template and its own copy of the problem.
func TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Node Node `+"`bson:\"node\"`"+`
}

type Node struct {
	Type   int32  `+"`bson:\"type\"`"+`
	Select string `+"`bson:\"select\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_node_nested.go"))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(generated); !strings.Contains(body, "func (s *Node) GetType() int32") {
		t.Errorf("the generated nested struct lost its accessor:\n%s", body)
	}
}

// 生成的嵌套 struct 能被 BSON 持久化，且 DirtyHook 不入文档：嵌套 struct 的字段全部未导出（变更只能走生成的 setter），
// 所以内嵌 DirtyHook 打 bson:"-" json:"-"，每个嵌套类型生成 MarshalBSON（值接收者）/ UnmarshalBSON（指针接收者），
// 按 snake_case 键写全部字段；否则落库的只是 {"pos": {"dirtyhook": {}}}。
func TestNestedStructsPersistTheirFieldsNotTheDirtyHook(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	var position *NestedDef
	for i := range defs.Nested {
		if defs.Nested[i].Name == "Position" {
			position = &defs.Nested[i]
		}
	}
	if position == nil {
		t.Fatal("fixture has no Position nested struct")
	}
	tmpl := template.Must(template.New("nested").Funcs(nestedFuncMap(defs)).Parse(nestedTemplate))
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"Package": "testdata", "Nested": *position}); err != nil {
		t.Fatal(err)
	}
	source := out.String()
	for _, want := range []string{
		"dataengine.DirtyHook `bson:\"-\" json:\"-\"`",
		"func (s Position) MarshalBSON() ([]byte, error)",
		"func (s *Position) UnmarshalBSON(raw []byte) error",
		"func (s Position) bsonDoc() positionBSONDoc",
		"func positionPtrBSONDoc(v *Position) *positionBSONDoc",
		"`bson:\"x\"`",
		"`bson:\"y\"`",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated nested struct lacks %q; its fields would not reach Mongo and DirtyHook would\n%s", want, source)
		}
	}
}

// The follow-up to the fix above: the parent DAO must not encode nested values
// through bson.Marshaler (one fresh []byte plus an interface box per value —
// about three times the reflection floor for a document with a few of them).
// It places the wire form into its own document instead, so reflection
// encodes the nested value inline.
func TestTheDaoDocumentCarriesNestedWireFormsNotMarshalers(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "gen_hero_dao.go")
	if _, err := generateDao(defs.Daos[0], defs, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	for _, want := range []string{
		`"pos":      d.pos.bsonDoc(),`,
		`"equips":   daoMapDocs(d.heroDaoEquipsRawMap(), equipInfoPtrBSONDoc),`,
		"Pos     positionBSONDoc             `bson:\"pos\"`",
		"Equips  map[int64]*equipInfoBSONDoc `bson:\"equips\"`",
		"d.pos = positionFromBSONDoc(dd.Pos)",
		"d.setEquipsRawMap(daoMapDocs(dd.Equips, equipInfoPtrFromBSONDoc))",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the DAO document does not use the nested wire form: missing %q", want)
		}
	}
	for _, stale := range []string{`"pos":      d.pos,`, "Pos     Position             `bson:\"pos\"`"} {
		if strings.Contains(source, stale) {
			t.Errorf("the DAO document still hands the nested value itself to the codec: %q", stale)
		}
	}
}

// A1（维护者 2026-10-05：回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：事务内会改、回滚时要恢复、
// 却不该落库也不该同步的状态放进 `dao:"nopersist,nosync"` 字段。承诺：这种字段和其他字段一样生成 mutator（undo 策略下登记逆操作），
// map 字段同样有 Get / Range / Len；它出现在回滚快照里，不出现在提交记录、存储读写与同步里。旧生成器只给 persist 或 sync 的字段
// 生成 mutator，nopersist,nosync 的标量只有 getter、map 连 getter 都没有，业务无从改它。
func TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	var variety *DaoDef
	for i := range defs.Daos {
		if defs.Daos[i].Name == "VarietyDao" {
			variety = &defs.Daos[i]
		}
	}
	if variety == nil {
		t.Fatal("fixture has no VarietyDao")
	}
	outFile := filepath.Join(t.TempDir(), "gen_variety_dao.go")
	if _, err := generateDao(*variety, defs, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, want := range []string{
		"func (d *VarietyDao) SetNeither(v int64)",
		"func (d *VarietyDao) SetPending(key int32, val int64)",
		"func (d *VarietyDao) DelPending(key int32)",
		"func (d *VarietyDao) GetPending(key int32) (int64, bool)",
		"func (d *VarietyDao) RangePending(",
		"func (d *VarietyDao) PendingLen() int",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the generated DAO has no %q: a nopersist,nosync field cannot be changed inside a transaction", want)
		}
	}
	bodies := generatedFunctions(source)
	if body := bodies["CaptureRollbackState"]; !strings.Contains(body, `bson:"pending"`) || !strings.Contains(body, `bson:"neither"`) {
		t.Error("the rollback snapshot does not cover the transient fields")
	}
	for _, name := range []string{"marshalCommitState", "marshalPersistData", "marshalPersistPatchBSON", "Unmarshal", "MarshalSync", "ApplySync", "markPendingKeyDirty", "markNeitherDirty"} {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("generated DAO has no %s", name)
		}
		if strings.Contains(body, "pending") || strings.Contains(body, "Pending") || strings.Contains(body, "neither") || strings.Contains(body, "Neither") {
			t.Errorf("%s touches a transient field; it must not reach storage, the commit record or sync:\n%s", name, body)
		}
	}
}

// generatedFunctions splits generated source into top-level function bodies,
// keyed by function name (methods by method name).
func generatedFunctions(source string) map[string]string {
	out := make(map[string]string)
	for _, chunk := range strings.Split(source, "\nfunc ")[1:] {
		header, _, _ := strings.Cut(chunk, "(")
		name := header
		if strings.HasPrefix(chunk, "(") {
			// method: "(d *X) Name(" — the name follows the receiver.
			_, rest, _ := strings.Cut(chunk, ") ")
			name, _, _ = strings.Cut(rest, "(")
		}
		body := chunk
		if end := strings.Index(chunk, "\n}\n"); end >= 0 {
			body = chunk[:end]
		}
		// The mark functions' own names contain the field name; check the body only.
		if _, after, ok := strings.Cut(body, "{"); ok {
			body = after
		}
		out[strings.TrimSpace(name)] = body
	}
	return out
}
