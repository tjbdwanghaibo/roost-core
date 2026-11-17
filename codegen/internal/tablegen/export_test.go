package tablegen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stageClientExportFixture 把 testdata/clientexport 复制成一个可编译的工程（go.mod 指向本仓库）。
func stageClientExportFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	core, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf("module example.com/clientexport\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => %s\n", core))
	for _, dir := range []string{"schema", "table"} {
		entries, err := os.ReadDir(filepath.Join("testdata", "clientexport", dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join("testdata", "clientexport", dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			writeTablegenTestFile(t, filepath.Join(root, dir, entry.Name()), string(raw))
		}
	}
	return root
}

// clientExport 读夹具工程的导出模型。
func clientExport(t *testing.T, root string) *ExportModel {
	t.Helper()
	model, err := ClientExport(filepath.Join(root, "schema"), filepath.Join(root, "table"))
	if err != nil {
		t.Fatalf("ClientExport: %v", err)
	}
	return model
}

// 端到端：同一份 schema 与 CSV，服务端产物（configs/data、_manifest.json、Go 绑定）里没有任何只给
// 客户端的东西，生成的 Go 编译运行后 index 查询、kv 转换、只给客户端字段为零值都成立；导出模型
// 里没有任何只给服务端的东西。
func TestClientExportFixtureEndToEnd(t *testing.T) {
	root := stageClientExportFixture(t)
	var out bytes.Buffer
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "table"), "-json", filepath.Join(root, "data"), "-force"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "table"), "-out", filepath.Join(root, "generated"), "-pkg", "generated", "-force"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tablegen: server side omitted 1 tables / 2 fields (client only)") {
		t.Fatalf("omitted counts not printed:\n%s", out.String())
	}

	// 服务端产物：只给客户端的表、字段、key 都不在。
	for _, clientOnly := range []string{"tip", "model", "shop_banner", "summer_sale", "slime.prefab"} {
		entries, err := os.ReadDir(filepath.Join(root, "data"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(root, "data", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(entry.Name(), clientOnly) || strings.Contains(string(raw), clientOnly) {
				t.Fatalf("server configs/data/%s carries client-only %q:\n%s", entry.Name(), clientOnly, raw)
			}
		}
	}
	balance, err := os.ReadFile(filepath.Join(root, "data", "balance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(balance) != "{\n  \"max_level\": 60,\n  \"server_tick\": 50,\n  \"start_gold\": 100\n}\n" {
		t.Fatalf("server balance.json = %s", balance)
	}
	source, err := os.ReadFile(filepath.Join(root, "generated", "gen_table_config.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func MonsterBySceneID(v int32) []meta1.Monster", "//   - max_level: 玩家等级上限", "They are Model.", "They are ShopBanner."} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated Go lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(string(source), "Tip") || strings.Contains(string(source), "商店顶部横幅") {
		t.Fatalf("generated Go carries client-only tip table or a client-only note:\n%s", source)
	}

	writeTablegenTestFile(t, filepath.Join(root, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"example.com/clientexport/generated"
	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
)

func check(ok bool, format string, args ...any) {
	if !ok {
		fmt.Printf(format+"\n", args...)
		os.Exit(1)
	}
}

func main() {
	r := configdata.NewRegistry()
	check(generated.RegisterGeneratedConfigData(r) == nil, "register")
	store := configdata.NewStore(r, "data")
	configdata.SetDefaultStore(store)
	snap, err := store.Load(context.Background())
	check(err == nil, "load: %v", err)

	// 非唯一索引：场景 1 有两只怪，按文件顺序返回；没有的值返回空。
	inForest := generated.MonsterBySceneID(1)
	check(len(inForest) == 2 && inForest[0].ID == 10 && inForest[1].ID == 12, "MonsterBySceneID(1) = %+v", inForest)
	check(len(generated.MonsterBySceneID(3)) == 0, "MonsterBySceneID(3) not empty")
	// 只给客户端的字段在服务端是零值；只给服务端的字段有值。
	m, ok := generated.MonsterByID(10)
	check(ok && m.Model == "" && m.DropSeed == 777 && m.Level == 3, "monster 10 = %+v", m)
	b, ok := generated.BalanceConfigFrom(snap)
	check(ok && b.MaxLevel == 60 && b.ServerTick == 50 && b.ShopBanner == "", "balance = %+v", b)

	// 运行时的 kv 转换：# 注释忽略，只给客户端的 key 接受但留零值。
	raw, err := os.ReadFile("table/balance.csv")
	check(err == nil, "read balance.csv: %v", err)
	kv, err := generated.ConvertBalanceCSV(strings.NewReader(string(raw)))
	check(err == nil && kv.MaxLevel == 60 && kv.StartGold == 100 && kv.ServerTick == 50 && kv.ShopBanner == "", "ConvertBalanceCSV = %+v, %v", kv, err)
	_, err = generated.ConvertBalanceCSV(strings.NewReader("key,value,note\nmax_level,1,\n# c\nmax_level,2,\n"))
	check(err != nil && strings.Contains(err.Error(), "line=4") && strings.Contains(err.Error(), "duplicate key"), "duplicate kv key: %v", err)
	_, err = generated.ConvertBalanceCSV(strings.NewReader("key,value,note\nmax_level,1,\nnope,2,\n"))
	check(err != nil && strings.Contains(err.Error(), "line=3 key=\"nope\": unknown key"), "unknown kv key: %v", err)
	_, err = generated.ConvertBalanceCSV(strings.NewReader("key,value,note\nstart_gold,1,\n"))
	check(err != nil && strings.Contains(err.Error(), "\"max_level\" is required"), "missing required kv key: %v", err)
	// 运行时的横排转换容忍只给客户端的列。
	rows, err := generated.ConvertMonsterCSV(strings.NewReader("id,scene_id,level,drop_seed,model,tags\n10,1,3,777,slime.prefab,\n"))
	check(err == nil && len(rows) == 1 && rows[0].Model == "" && rows[0].DropSeed == 777, "ConvertMonsterCSV = %+v, %v", rows, err)
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated server binding: %v\n%s", err, output)
	}

	// 导出模型：四张客户端表，没有只给服务端的字段与值。
	model := clientExport(t, root)
	var names []string
	for _, table := range model.Tables {
		names = append(names, table.Name+":"+string(table.Kind))
		for _, field := range table.Fields {
			if field.Name == "DropSeed" || field.Name == "ServerTick" {
				t.Fatalf("export model carries server-only field %s.%s", table.Name, field.Name)
			}
		}
		for _, serverOnly := range []string{"drop_seed", "server_tick", "777", "888"} {
			if strings.Contains(string(table.Data), serverOnly) {
				t.Fatalf("export %s data carries server-only %q:\n%s", table.Name, serverOnly, table.Data)
			}
		}
		if table.DataHash != DataHash(table.Data) || len(table.SchemaHash) != 64 {
			t.Fatalf("%s hashes = %s / %s", table.Name, table.DataHash, table.SchemaHash)
		}
	}
	if got := strings.Join(names, ","); got != "balance:kv,monster:table,scene:table,tip:table" {
		t.Fatalf("export tables = %s", got)
	}
	if model.OmittedTables != 0 || model.OmittedFields != 2 {
		t.Fatalf("export omitted = %d tables / %d fields, want 0 / 2", model.OmittedTables, model.OmittedFields)
	}
	balanceTable, monster := model.Tables[0], model.Tables[1]
	if string(balanceTable.Data) != "{\n  \"max_level\": 60,\n  \"shop_banner\": \"summer_sale\",\n  \"start_gold\": 100\n}\n" || balanceTable.Rows != 1 {
		t.Fatalf("export balance = %s (rows %d)", balanceTable.Data, balanceTable.Rows)
	}
	if balanceTable.Fields[2].Name != "ShopBanner" || balanceTable.Fields[2].Note != "商店顶部横幅" || balanceTable.Fields[0].Note != "玩家等级上限" {
		t.Fatalf("export balance fields = %+v", balanceTable.Fields)
	}
	var monsterData struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(monster.Data, &monsterData); err != nil || len(monsterData.Rows) != 3 || monster.Rows != 3 {
		t.Fatalf("export monster data = %s (%v)", monster.Data, err)
	}
	if monsterData.Rows[0]["model"] != "slime.prefab" || monsterData.Rows[0]["tags"] != `["a"]` {
		t.Fatalf("export monster row 0 = %v", monsterData.Rows[0])
	}
	if !reflect.DeepEqual(monster.Indexes, []ExportIndex{{Name: "SceneID", Field: "SceneID", JSON: "scene_id", Type: "int32"}}) {
		t.Fatalf("export monster indexes = %+v", monster.Indexes)
	}
	if monster.Key != "ID" || !monster.Fields[0].Key || monster.Fields[4].Parser != "json" || monster.Fields[1].Title != "场景" {
		t.Fatalf("export monster fields = %+v", monster.Fields)
	}
	var ruleFields []string
	for _, rule := range monster.Rules {
		ruleFields = append(ruleFields, rule.Field)
	}
	if strings.Join(ruleFields, ",") != "id,scene_id,level,model" {
		t.Fatalf("export monster rules = %+v", monster.Rules)
	}
	if len(model.DataVersion) != 64 || model.ShortDataVersion() != model.DataVersion[:12] {
		t.Fatalf("data version = %q / %q", model.DataVersion, model.ShortDataVersion())
	}

	// 同样的输入，模型逐字节相同。
	if again := clientExport(t, root); !reflect.DeepEqual(again, model) {
		t.Fatal("ClientExport is not deterministic")
	}
}

// DataVersion：客户端数据变了就变；只改只给服务端的字段不变；CSV 换成 CRLF 不变。
func TestClientExportDataVersionFollowsClientDataOnly(t *testing.T) {
	root := stageClientExportFixture(t)
	base := clientExport(t, root)
	edit := func(file, old, replacement string) {
		t.Helper()
		path := filepath.Join(root, "table", file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), old) {
			t.Fatalf("%s has no %q", file, old)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, replacement, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 只给服务端的字段：普通表的 drop_seed 与 kv 表的 server_tick。
	edit("monster.csv", "777", "778")
	edit("balance.csv", "server_tick,50", "server_tick,51")
	if got := clientExport(t, root); got.DataVersion != base.DataVersion {
		t.Fatalf("server-only edits changed the data version: %s -> %s", base.DataVersion, got.DataVersion)
	}
	// CRLF 的 CSV 得到同一份数据。
	path := filepath.Join(root, "table", "scene.csv")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := clientExport(t, root); got.DataVersion != base.DataVersion {
		t.Fatal("CRLF line endings changed the data version")
	}
	if DataHash([]byte("a\r\nb\r\n")) != DataHash([]byte("a\nb\n")) {
		t.Fatal("DataHash does not normalise CRLF")
	}
	// 每张表的客户端字段各改一处，版本号都要变。
	for _, change := range []struct{ file, old, replacement string }{
		{"monster.csv", "slime.prefab", "slime2.prefab"}, // 只给客户端的字段
		{"scene.csv", "forest", "woods"},                 // 两端共用的表
		{"balance.csv", "summer_sale", "winter_sale"},    // kv 表里只给客户端的字段
		{"tip.csv", "hello", "hi"},                       // side=c 的表
	} {
		before := clientExport(t, root).DataVersion
		edit(change.file, change.old, change.replacement)
		if after := clientExport(t, root).DataVersion; after == before {
			t.Fatalf("editing %s (%s -> %s) left the data version at %s", change.file, change.old, change.replacement, before)
		}
	}
}

// 导出也要求两端的声明都成立：服务端规则错误在导出时同样报。
func TestClientExportRejectsBadSchema(t *testing.T) {
	root := stageClientExportFixture(t)
	writeTablegenTestFile(t, filepath.Join(root, "schema", "bad.go"), "package schema\n//roost:table side=s\ntype Secret struct { ID int32 }\n//roost:table\ntype Ref struct { ID int32; SecretID int32 `ref:\"secret\" side:\"c\"` }\n")
	writeTablegenTestFile(t, filepath.Join(root, "table", "secret.csv"), "id\n1\n")
	writeTablegenTestFile(t, filepath.Join(root, "table", "ref.csv"), "id,secret_i_d\n1,1\n")
	if _, err := ClientExport(filepath.Join(root, "schema"), filepath.Join(root, "table")); err == nil || !strings.Contains(err.Error(), `ref target "secret" is not on the client side`) {
		t.Fatalf("ClientExport = %v, want the cross-side ref named", err)
	}
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "table"), "-json", filepath.Join(root, "data"), "-force"}, io.Discard); err == nil {
		t.Fatal("server generation accepted a cross-side ref")
	}
}
