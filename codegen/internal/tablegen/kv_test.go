package tablegen

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const kvSchema = "//roost:object name=balance layout=kv\ntype Balance struct {\n" +
	"MaxLevel int32 `csv:\"max_level\" json:\"max_level\" title:\"最高等级\" required:\"true\" min:\"1\"`\n" +
	"StartGold int64 `csv:\"start_gold\" json:\"start_gold\"`\n" +
	"Tick int32 `csv:\"tick\" json:\"tick\" side:\"s\"`\n" +
	"Banner string `csv:\"banner\" json:\"banner\" side:\"c\"`\n}\n"

func kvSide(t *testing.T, side string) Meta {
	t.Helper()
	metas, _, err := projectSide(parseSchema(t, kvSchema), side)
	if err != nil {
		t.Fatal(err)
	}
	return metas[0]
}

func writeKV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "balance.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 竖排键值表的 JSON 与横排 object 相同（单个对象，键是 JSON 名）；# 注释行与空行忽略；note 列
// 读出来（服务端写进生成代码的注释）；只给客户端的 key 是合法的 key，但不进服务端 JSON。
func TestKVSheetBecomesTheSameObjectAsAWideSheet(t *testing.T) {
	sheet := "# 数值\nkey,value,note\nmax_level,60,玩家等级上限\n\n# 金币\nstart_gold,100,\ntick,50,只给服务端\nbanner,summer,横幅\n"
	server, err := readKVObject(writeKV(t, sheet), kvSide(t, sideServer))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"max_level": int64(60), "start_gold": int64(100), "tick": int64(50)}
	if !reflect.DeepEqual(server.values, want) {
		t.Fatalf("server kv values = %#v, want %#v", server.values, want)
	}
	if server.notes["max_level"] != "玩家等级上限" || server.notes["banner"] != "横幅" {
		t.Fatalf("notes = %#v", server.notes)
	}
	// 同一份数据写成横排 object，得到的 JSON 逐字节相同。
	wide := kvSide(t, sideServer)
	wide.Layout = ""
	rows, err := readCSVRecords(writeCSV(t, "max_level,start_gold,tick,banner\n60,100,50,summer\n"), wide)
	if err != nil {
		t.Fatal(err)
	}
	kvJSON, _ := json.MarshalIndent(server.values, "", "  ")
	wideJSON, _ := json.MarshalIndent(rows[0], "", "  ")
	if string(kvJSON) != string(wideJSON) {
		t.Fatalf("kv JSON %s differs from the wide object %s", kvJSON, wideJSON)
	}
	client, err := readKVObject(writeKV(t, sheet), kvSide(t, sideClient))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.values["tick"]; ok || client.values["banner"] != "summer" {
		t.Fatalf("client kv values = %#v, want banner and no tick", client.values)
	}
}

// 每条 kv 规则各破坏一次：错误写明文件、行与 key。
func TestKVSheetRejectsBadKeys(t *testing.T) {
	cases := []struct{ label, sheet, want string }{
		{"duplicate key", "key,value,note\nmax_level,60,\n# x\nmax_level,70,\n", `balance.csv:4: duplicate key "max_level" (first at line 2)`},
		{"unknown key", "key,value,note\nmax_level,60,\nmaxlevel2,1,\n", `balance.csv:3: unknown key "maxlevel2"`},
		{"key case", "key,value,note\nMax_Level,60,\n", `balance.csv:2: key "Max_Level" must be spelled "max_level"`},
		{"required missing", "key,value,note\nstart_gold,1,\n", `balance.csv: required key "max_level" is missing`},
		{"required empty", "key,value,note\nmax_level,,\n", "field max_level: required: missing or null"},
		{"below min", "key,value,note\nmax_level,0,\n", "field max_level: min: value 0 is below min=1"},
		{"bad value", "key,value,note\nmax_level,abc,\n", `balance.csv:2: key "max_level" field=MaxLevel value="abc"`},
		{"no header", "max_level,60,\n", "balance.csv:1: a layout=kv sheet starts with the header key,value,note"},
		{"extra header column", "key,value,note,extra\nmax_level,60,\n", "balance.csv:1: a layout=kv sheet starts with the header"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			_, err := readKVObject(writeKV(t, testCase.sheet), kvSide(t, sideServer))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("readKVObject = %v, want an error containing %q", err, testCase.want)
			}
		})
	}
	// 只给客户端的必填字段缺行，服务端转换也报：一张表两端是同一份检查。
	required := strings.Replace(kvSchema, "`csv:\"banner\" json:\"banner\" side:\"c\"`", "`csv:\"banner\" json:\"banner\" side:\"c\" required:\"true\"`", 1)
	metas, _, err := projectSide(parseSchema(t, required), sideServer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readKVObject(writeKV(t, "key,value,note\nmax_level,60,\n"), metas[0]); err == nil || !strings.Contains(err.Error(), `required key "banner" is missing`) {
		t.Fatalf("server conversion of a sheet missing a client-only required key = %v", err)
	}
}

// kv 的 CSV 模板是竖排的：表头加每个字段一行（含两端的字段）。
func TestKVTemplateIsVertical(t *testing.T) {
	dir := t.TempDir()
	if err := writeCSVTemplates(parseSchema(t, kvSchema), dir, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "balance.csv"))
	if err != nil {
		t.Fatal(err)
	}
	want := "key,value,note\nmax_level,,最高等级\nstart_gold,,\ntick,,\nbanner,,\n"
	if string(raw) != want {
		t.Fatalf("kv template = %q, want %q", raw, want)
	}
}
