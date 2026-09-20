package dao

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// U-0224 · C2 · 用户复审提出：生成的嵌套 struct 要能被 BSON 持久化，且 DirtyHook 不入文档。
// 嵌套 struct 的字段全部未导出（变更只能走生成的 setter，dirty / undo 才不会被绕过），而 DAO 的
// marshalCommitState / CaptureRollbackState / MarshalSync 把嵌套值直接放进 bson.M 交给反射编码——
// 反射看不见未导出字段，只看见导出的内嵌 DirtyHook，于是落库的是 {"pos": {"dirtyhook": {}}}：
// 嵌套数据一个字节都没存，还多了一个空子文档。修法：内嵌打 bson:"-" json:"-"，并为每个嵌套类型
// 生成 MarshalBSON（值接收者，bson.M 里放的是值）/ UnmarshalBSON（指针接收者），按 snake_case 键写全部字段。
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
