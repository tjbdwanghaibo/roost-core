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
		{Name: "Bonus", TypeStr: "map[string][]int64", Kind: KindMap, MapKey: "string", MapVal: "[]int64", Tag: DaoTag{Sync: true}},
	}}}}
	source, schema, err := buildSyncSchema(defs, "db")
	if err != nil {
		t.Fatal(err)
	}
	// 列表包装按元素类型命名并共用：两个 []int64 值的 map 只生成一条 SyncListInt64。
	if !strings.Contains(string(schema), "map<int64, SyncListInt64>") || !strings.Contains(string(schema), "map<string, SyncListInt64>") ||
		strings.Count(string(schema), "message SyncListInt64 ") != 1 || !strings.Contains(string(schema), "repeated int32 value") {
		t.Fatalf("unexpected proto schema:\n%s", schema)
	}
	if !strings.Contains(string(source), "syncMapValues") {
		t.Fatal("map list conversion missing")
	}
}

func TestSyncSchemaRejectsGeneratedNameCollision(t *testing.T) {
	// 业务嵌套类型 ListInt32 生成 SyncListInt32，与 []int32 的列表包装同名，必须报错而不是生成两个同名类型。
	defs := &Definitions{
		Daos: []DaoDef{{Name: "SampleDao", Fields: []FieldDef{
			{Name: "Bag", TypeStr: "map[string][]int32", Kind: KindMap, MapKey: "string", MapVal: "[]int32", Tag: DaoTag{Sync: true}},
			{Name: "Item", TypeStr: "ListInt32", Tag: DaoTag{Sync: true}},
		}}},
		Nested: []NestedDef{{Name: "ListInt32", Fields: []FieldDef{{Name: "Count", TypeStr: "int32"}}}},
	}
	if _, _, err := buildSyncSchema(defs, "db"); err == nil || !strings.Contains(err.Error(), "SyncListInt32 is generated twice") {
		t.Fatalf("expected generated name collision, got %v", err)
	}
}
