package tablegen

// RR-20261005-NC-75：`tablegen -json <dir> -check` 的帮助是 “check generated JSON files against
// current table metadata”，旧实现只做 json.Unmarshal——语法对就过。运维按 game-demo 的说明直接
// 改 configs/data/*.json 再 reload 时，schema 里的 required / unique / min 一条都不查（required 在
// 运行时也不查，见 N07 第二批 H2e：删掉 spawn 的 template 后 reload 被接受，刷出 template 0
// 的怪）。承诺：-check 对 JSON 执行与 CSV 转换相同的规则——required 列必须出现且不为
// null，unique / key 不重复，min 不越界；生成器自己写出的 JSON 照常通过。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMonsterJSON(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "monster.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckJSONEnforcesTheDeclaredRules(t *testing.T) {
	meta := monsterMeta()
	meta.JSON = "monster.json"
	cases := []struct{ label, body, want string }{
		{"required key missing", `[{"id":1,"level":3,"code":"a"}]`, "table monster row 1 (key 1) field name: required: missing or null"},
		{"required key null", `[{"id":1,"name":null,"level":3,"code":"a"}]`, "field name: required: missing or null"},
		{"key repeated", `[{"id":1,"name":"slime","level":3,"code":"a"},{"id":1,"name":"orc","level":4,"code":"b"}]`, "row 2 (key 1) field id: unique: value 1 repeats row 1"},
		{"unique column repeated", `[{"id":1,"name":"slime","level":3,"code":"a"},{"id":2,"name":"orc","level":4,"code":"a"}]`, "field code: unique: value a repeats row 1"},
		{"below min", `[{"id":1,"name":"slime","level":0,"code":"a"}]`, "field level: min: value 0 is below min=1"},
		{"not a list of rows", `{"id":1}`, "monster.json"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, testCase.body))
			if err == nil {
				t.Fatalf("-check accepted JSON that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
	// What the generator writes from a valid sheet passes, including an
	// optional column left at its zero value.
	if err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, `{"rows":[{"id":1,"name":"slime","level":3,"code":"a"},{"id":2,"name":"orc","level":1,"code":""}]}`)); err != nil {
		t.Fatalf("-check rejected valid rows: %v", err)
	}
}

// A ref= the generated loader cannot check is a schema error at generation
// time, not a silently ignored tag (RR-20261005-NC-75).
func TestRefDeclarationsAreResolvedAtGeneration(t *testing.T) {
	scene := Meta{Kind: KindTable, Name: "scene", Key: "ID", TypeName: "Scene", Alias: "meta0", Fields: []Field{{Name: "ID", Type: "int32", JSON: "id"}}}
	monster := func(ref, typ string) Meta {
		return Meta{Kind: KindTable, Name: "monster", Key: "ID", TypeName: "Monster", Alias: "meta1", Fields: []Field{
			{Name: "ID", Type: "int32", JSON: "id"},
			{Name: "SceneID", Type: typ, JSON: "scene_id", Ref: ref},
		}}
	}
	if err := resolveRefs([]Meta{scene, monster("scene", "int32")}); err != nil {
		t.Fatalf("valid ref: %v", err)
	}
	if err := resolveRefs([]Meta{scene, monster("scene", "*int32")}); err != nil {
		t.Fatalf("pointer ref: %v", err)
	}
	for label, metas := range map[string][]Meta{
		"unknown target": {scene, monster("zone", "int32")},
		"type mismatch":  {scene, monster("scene", "int64")},
	} {
		if err := resolveRefs(metas); err == nil {
			t.Errorf("%s: resolveRefs accepted it", label)
		}
	}
}
