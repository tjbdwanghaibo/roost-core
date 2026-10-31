package dao

import (
	"strings"
	"testing"
)

func TestSyncCSharpUsesProtocPropertyNames(t *testing.T) {
	// 与本机 protoc 实际生成结果核对过；保留关键字与数字边界，防止生成合法 PB 却无法编译 Apply。
	for input, want := range map[string]string{"parser": "Parser_", "descriptor": "Descriptor_", "clone": "Clone_", "types": "Types_", "foo1bar": "Foo1Bar", "sync_hero_dao": "SyncHeroDao_", "result_json": "ResultJson"} {
		if got := syncCSharpProperty(input, "SyncHeroDao"); got != want {
			t.Errorf("%s => %s want %s", input, got, want)
		}
	}
}
func TestSyncPBTypeShapes(t *testing.T) {
	defs := &Definitions{Daos: []DaoDef{{Name: "SampleDao", Fields: []FieldDef{
		{Name: "Scores", TypeStr: "map[int][]int64", Kind: KindMap, MapKey: "int", MapVal: "[]int64", Tag: DaoTag{Sync: true}},
		{Name: "Runes", TypeStr: "[]rune", Kind: KindSlice, SliceElem: "rune", Tag: DaoTag{Sync: true}},
		{Name: "Flag", TypeStr: "bool", Tag: DaoTag{Sync: true}},
	}}}}
	source, schema, err := buildSyncSchema(defs, "db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), "map<int64, SyncList") || !strings.Contains(string(schema), "repeated int32 value") {
		t.Fatalf("unexpected proto schema:\n%s", schema)
	}
	if !strings.Contains(string(source), "syncMapValues") {
		t.Fatal("map list conversion missing")
	}
}
