package tablegen

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monster.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func monsterMeta() Meta {
	return Meta{Kind: KindTable, Name: "monster", File: "monster.csv", Key: "ID", TypeName: "Monster", Fields: []Field{
		{Name: "ID", Type: "int32", CSV: "id", JSON: "id", Title: "编号", Required: true},
		{Name: "Name", Type: "string", CSV: "name", JSON: "name", Title: "名字", Required: true},
		{Name: "Level", Type: "int32", CSV: "level", JSON: "level", Title: "等级", Min: "1"},
		{Name: "Code", Type: "string", CSV: "code", JSON: "code", Title: "代号", Unique: true},
	}}
}

// The rules a table declares in its tags are enforced when the CSV is turned
// into JSON — the moment the author can still fix the sheet. Each case breaks
// one rule; the error names the file, the field and the row.
func TestReadCSVRecordsEnforcesDeclaredRules(t *testing.T) {
	cases := []struct{ label, csv, want string }{
		{"required cell empty", "id,name,level,code\n1,,3,a\n", "monster.csv: table monster row 1 (key 1) field name: required: missing or null"},
		{"unparsable int", "id,name,level,code\n1,slime,abc,a\n", "row=2 col=level field=Level"},
		{"key repeated", "id,name,level,code\n1,slime,3,a\n1,orc,4,b\n", "row 2 (key 1) field id: unique: value 1 repeats row 1"},
		{"unique column repeated", "id,name,level,code\n1,slime,3,a\n2,orc,4,a\n", "field code: unique: value a repeats row 1"},
		{"below min", "id,name,level,code\n1,slime,0,a\n", "field level: min: value 0 is below min=1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			_, err := readCSVRecords(writeCSV(t, testCase.csv), monsterMeta())
			if err == nil {
				t.Fatalf("accepted a sheet that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
}

// The optional title / type / rule rows the generator itself emits into a
// sheet are recognised and skipped, so a round-tripped CSV does not parse its
// own header rows as data.
func TestReadCSVRecordsSkipsTitleTypeAndRuleRows(t *testing.T) {
	meta := monsterMeta()
	titles := "编号,名字,等级,代号\n"
	types := "int32,string,int32,string\n"
	rules := strings.Join(fieldValues(meta.Fields, fieldRule), ",") + "\n"
	rows, err := readCSVRecords(writeCSV(t, "id,name,level,code\n"+titles+types+rules+"1,slime,3,a\n2,orc,4,b\n"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["name"] != "slime" || rows[1]["level"] != int64(4) {
		t.Fatalf("rows = %#v", rows)
	}
}

// A generated file is never overwritten without -force: the check exists so a
// stale generator cannot clobber a hand-edited output by accident.
func TestWriteGeneratedRefusesToOverwriteWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.go")
	if err := writeGenerated(path, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerated(path, []byte("two"), false); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("second write without -force: err=%v", err)
	}
	if err := writeGenerated(path, []byte("two"), true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "two" {
		t.Fatalf("content after -force = %q", raw)
	}
}

// `tablegen -json <dir> -check` 对 JSON 执行与 CSV 转换相同的规则：required 列必须出现且不为 null，unique / key 不重复，
// min 不越界；生成器自己写出的 JSON 照常通过。只做 json.Unmarshal 时，直接改 configs/data 再 reload 的错误一条都查不出。

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

// 生成的 loader 检查不了的 ref= 是生成期的 schema 错误，不是被静默忽略的标签。
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

// configdata 大小写敏感（维护者 2026-10-06）：生成期检查与加载层用同一条拼写规则（configdata/rules.MisspelledKey）。
// `-check` 拒绝只差大小写的 JSON 键；CSV 转换拒绝只差大小写的表头，不能把这一列当未知列跳过、值静默丢成零值。

func TestCheckJSONRejectsMisspelledKeys(t *testing.T) {
	meta := monsterMeta()
	meta.JSON = "monster.json"
	for body, want := range map[string]string{
		`[{"id":1,"name":"slime","Level":3,"code":"a"}]`:           `table monster row 1 (key 1) field level: case: key "Level" must be spelled "level"`,
		`[{"id":1,"name":"slime","level":3,"Level":0,"code":"a"}]`: `field level: case: key "Level"`,
	} {
		err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", body, err, want)
		}
	}
	// 未声明的键维持原行为（-check 不管）。
	if err := checkJSONFiles([]Meta{meta}, writeMonsterJSON(t, `[{"id":1,"name":"slime","level":3,"code":"a","note":"x"}]`)); err != nil {
		t.Fatalf("undeclared key rejected: %v", err)
	}
}

func TestCSVHeaderIsCaseSensitive(t *testing.T) {
	_, err := readCSVRecords(writeCSV(t, "id,name,Level,code\n1,slime,3,a\n"), monsterMeta())
	if err == nil || !strings.Contains(err.Error(), `monster.csv: header "Level" must be spelled "level"`) {
		t.Fatalf("err = %v, want the misspelled header named", err)
	}
	// 未声明的列照旧跳过。
	if _, err := readCSVRecords(writeCSV(t, "id,name,level,code,note\n1,slime,3,a,x\n"), monsterMeta()); err != nil {
		t.Fatalf("undeclared column rejected: %v", err)
	}
}

// 规则统一由运行时加载层强制，生成期检查只作提前反馈，两处用同一份规则声明（维护者决定 B10）：一个 schema 标签同时驱动
// 生成期检查与生成的 loader——改一处标签，两处一起变。

func TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader(t *testing.T) {
	for _, minimum := range []string{"1", "5"} {
		meta := monsterMeta()
		meta.Alias, meta.ImportPath, meta.JSON = "meta0", "example.com/game/configs/schema", "monster.json"
		meta.Fields[2].Min = minimum // level

		// Generation time: a sheet whose level is below the tag's min fails.
		below := map[string]string{"1": "0", "5": "3"}[minimum]
		if _, err := readCSVRecords(writeCSV(t, "id,name,level,code\n1,slime,"+below+",a\n"), meta); err == nil || !strings.Contains(err.Error(), "min") {
			t.Fatalf("min=%s: CSV with level %s: err = %v, want a min violation", minimum, below, err)
		}

		// Run time: the generated loader carries the same rule for configdata
		// to enforce on every load and reload.
		out := t.TempDir()
		if err := generateGo([]Meta{meta}, out, "generated", true, io.Discard); err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join(out, "gen_table_config.go"))
		if err != nil {
			t.Fatal(err)
		}
		if want := `{Field: "level", Min: "` + minimum + `"}`; !strings.Contains(string(source), want) {
			t.Fatalf("min=%s: generated loader does not carry %s:\n%s", minimum, want, source)
		}
	}
}
