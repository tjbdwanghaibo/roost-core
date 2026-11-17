package tablegen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseSchema 把一段 schema 源码写进临时工程并解析出 metas。
func parseSchema(t *testing.T, source string) []Meta {
	t.Helper()
	root := t.TempDir()
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), "module example.com/side\n")
	writeTablegenTestFile(t, filepath.Join(root, "schema", "schema.go"), "package schema\n"+source)
	metas, err := parseMetaRoot(filepath.Join(root, "schema"))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return metas
}

func fieldNames(fields []Field) []string {
	return fieldValues(fields, func(f Field) string { return f.Name })
}

// 每条按端规则各破坏一次：错误必须在服务端生成（validateMetas）时就报，不等到导出。
func TestSideRulesAreRejected(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"key excluded from client", "//roost:table key=ID\ntype Item struct { ID int32 `side:\"s\"`; Name string }\n",
			`key field ID is side:"s"`},
		{"key excluded from server", "//roost:table key=ID\ntype Item struct { ID int32 `side:\"c\"`; Name string }\n",
			`key field ID is side:"c"`},
		{"ref to a client-only table from a shared field", "//roost:table side=c\ntype Scene struct { ID int32 }\n//roost:table\ntype Item struct { ID int32; SceneID int32 `ref:\"scene\"` }\n",
			`ref target "scene" is not on the server side`},
		{"ref to a server-only table from a client field", "//roost:table side=s\ntype Scene struct { ID int32 }\n//roost:table\ntype Item struct { ID int32; SceneID int32 `ref:\"scene\" side:\"c\"` }\n",
			`ref target "scene" is not on the client side`},
		{"every object field server-only", "//roost:object\ntype Setting struct { A int32 `side:\"s\"`; B int32 `side:\"s\"` }\n",
			"every field is excluded from the client side; mark the object side=s instead"},
		{"every object field client-only", "//roost:object\ntype Setting struct { A int32 `side:\"c\"` }\n",
			"every field is excluded from the server side; mark the object side=c instead"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			err := validateMetas(parseSchema(t, testCase.source))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("validateMetas = %v, want an error containing %q", err, testCase.want)
			}
		})
	}
}

// side 的写法在解析时就核对：值只能是 c / s，字段的端与整表的端相反时字段哪里都不导出。
func TestSideDeclarationsAreParsedStrictly(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"field side typo", "//roost:table\ntype Item struct { ID int32; Name string `side:\"client\"` }\n", `side:"client" must be c (client only) or s (server only)`},
		{"table side typo", "//roost:table side=x\ntype Item struct { ID int32 }\n", `side="x" must be c`},
		{"field against table side", "//roost:table side=c\ntype Item struct { ID int32; Name string `side:\"s\"` }\n", "is exported nowhere"},
		{"layout on table", "//roost:table layout=kv\ntype Item struct { ID int32 }\n", `unknown marker option "layout"`},
		{"unknown layout", "//roost:object layout=grid\ntype Item struct { ID int32 }\n", `layout="grid" is unknown`},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			root := t.TempDir()
			writeTablegenTestFile(t, filepath.Join(root, "go.mod"), "module example.com/side\n")
			writeTablegenTestFile(t, filepath.Join(root, "schema", "schema.go"), "package schema\n"+testCase.source)
			_, err := parseMetaRoot(filepath.Join(root, "schema"))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("parse = %v, want an error containing %q", err, testCase.want)
			}
		})
	}
}

// 服务端投影：side=c 的表不在，只给客户端的字段不在 Fields 里，但仍在 AllFields（CSV 表头可以有
// 这一列）；客户端投影反过来。没有 side 声明的表投影前后相同。
func TestProjectSideKeepsOnlyThatSide(t *testing.T) {
	metas := parseSchema(t, "//roost:table\ntype Item struct { ID int32; Seed int64 `side:\"s\"`; Icon string `side:\"c\"`; Name string }\n"+
		"//roost:table side=c\ntype Tip struct { ID int32 }\n//roost:table side=s\ntype Drop struct { ID int32 }\n//roost:table\ntype Plain struct { ID int32; Name string }\n")
	server, omitted, err := projectSide(metas, sideServer)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != (omittedBySide{tables: 1, fields: 1}) {
		t.Fatalf("server omitted = %+v, want 1 table / 1 field", omitted)
	}
	byName := map[string]Meta{}
	for _, meta := range server {
		byName[meta.Name] = meta
	}
	if _, ok := byName["tip"]; ok {
		t.Fatal("side=c table tip is on the server side")
	}
	if got := strings.Join(fieldNames(byName["item"].Fields), ","); got != "ID,Seed,Name" {
		t.Fatalf("server item fields = %s", got)
	}
	if got := strings.Join(fieldNames(byName["item"].AllFields), ","); got != "ID,Seed,Icon,Name" {
		t.Fatalf("server item sheet fields = %s", got)
	}
	if got := strings.Join(byName["item"].Omitted, ","); got != "Icon" {
		t.Fatalf("server item omitted = %s", got)
	}
	if plain := byName["plain"]; len(plain.Fields) != 2 || plain.Omitted != nil {
		t.Fatalf("a table without side changed: %+v", plain)
	}
	client, omitted, err := projectSide(metas, sideClient)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != (omittedBySide{tables: 1, fields: 1}) {
		t.Fatalf("client omitted = %+v, want 1 table / 1 field", omitted)
	}
	for _, meta := range client {
		if meta.Name == "drop" {
			t.Fatal("side=s table drop is on the client side")
		}
		if meta.Name == "item" {
			if got := strings.Join(fieldNames(meta.Fields), ","); got != "ID,Icon,Name" {
				t.Fatalf("client item fields = %s", got)
			}
		}
	}
}

// 服务端产物只含服务端：configs/data 与 _manifest.json 没有 side=c 的表，行里没有只给客户端的
// 字段（CSV 里有那一列也不报错），生成时打印 omitted；生成的 Go 不登记 side=c 的表。
func TestServerArtifactsCarryNoClientOnlyData(t *testing.T) {
	root := t.TempDir()
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), "module example.com/side\n")
	writeTablegenTestFile(t, filepath.Join(root, "schema", "schema.go"), "package schema\n"+
		"//roost:table\ntype Item struct { ID int32 `csv:\"id\" json:\"id\"`; Icon string `csv:\"icon\" json:\"icon\" side:\"c\" required:\"true\"`; Seed int64 `csv:\"seed\" json:\"seed\" side:\"s\"` }\n"+
		"//roost:table side=c\ntype Tip struct { ID int32 `csv:\"id\" json:\"id\"` }\n")
	writeTablegenTestFile(t, filepath.Join(root, "csv", "item.csv"), "id,icon,seed\n1,sword.png,7\n")
	writeTablegenTestFile(t, filepath.Join(root, "csv", "tip.csv"), "id\n1\n")
	var out bytes.Buffer
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "csv"), "-json", filepath.Join(root, "data"), "-out", filepath.Join(root, "generated"), "-pkg", "generated", "-force"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tablegen: server side omitted 1 tables / 1 fields (client only)") {
		t.Fatalf("omitted counts not printed:\n%s", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(root, "data", "item.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if _, ok := rows[0]["icon"]; ok || rows[0]["seed"] != float64(7) {
		t.Fatalf("server item.json = %s, want seed and no icon", raw)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "tip.json")); !os.IsNotExist(err) {
		t.Fatalf("client-only table written to configs/data: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "data", "_manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "tip.json") {
		t.Fatalf("_manifest.json lists the client-only table: %s", manifest)
	}
	source, err := os.ReadFile(filepath.Join(root, "generated", "gen_table_config.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "Tip") {
		t.Fatal("generated Go registers the client-only table")
	}
	if !strings.Contains(string(source), "Client-only fields (side:\"c\") stay zero on the server") || !strings.Contains(string(source), "They are Icon.") {
		t.Fatalf("generated Go does not say Icon stays zero:\n%s", source)
	}
	// -check 用服务端投影：服务端 JSON 通过，手工把客户端字段写回去不算拼错（它是 schema 的字段）。
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-json", filepath.Join(root, "data"), "-check"}, &out); err != nil {
		t.Fatalf("-check rejects the server projection: %v", err)
	}
}

// CSV 表头的大小写核对覆盖另一端的列：只给客户端的列拼错大小写，服务端转换也要报。
func TestHeaderCaseCheckCoversTheOtherSidesColumns(t *testing.T) {
	metas := parseSchema(t, "//roost:table\ntype Item struct { ID int32 `csv:\"id\" json:\"id\"`; Icon string `csv:\"icon\" json:\"icon\" side:\"c\"` }\n")
	server, _, err := projectSide(metas, sideServer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readCSVRecords(writeCSV(t, "id,Icon\n1,x\n"), server[0]); err == nil || !strings.Contains(err.Error(), `header "Icon" must be spelled "icon"`) {
		t.Fatalf("server conversion err = %v, want the misspelt client column named", err)
	}
}
