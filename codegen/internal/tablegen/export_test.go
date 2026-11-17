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
	// 横排表的运行时转换同样忽略 # 注释行（标题行前后、数据行之间都有）。
	rows, err = generated.ConvertMonsterCSV(strings.NewReader("# 怪物\nid,scene_id,level,drop_seed,model\n编号,场景,等级,掉落种子,模型\n# 场景 1\n10,1,3,777,slime.prefab\n# 场景 2\n11,2,5,888,bat.prefab\n"))
	check(err == nil && len(rows) == 2 && rows[0].ID == 10 && rows[1].ID == 11 && rows[1].DropSeed == 888, "ConvertMonsterCSV(# comments) = %+v, %v", rows, err)
	scenes, err := generated.ConvertSceneCSV(strings.NewReader("id,name\n# 注释\n1,forest\n"))
	check(err == nil && len(scenes) == 1 && scenes[0].Name == "forest", "ConvertSceneCSV(# comment) = %+v, %v", scenes, err)
	// 运行时横排转换的报错行号是文件里的行号（交接 C5 (c)）：前面两行注释，坏值在文件第 5 行。
	_, err = generated.ConvertSceneCSV(strings.NewReader("# 场景\n# 第二行注释\nid,name\n1,forest\nx,cave\n"))
	check(err != nil && strings.Contains(err.Error(), "row=5 field=ID"), "ConvertSceneCSV(bad id after comments) = %v, want row=5", err)
	// 带引号的多行单元格：一条记录占第 3、4 两行，坏值在第 4 行，报的是记录开始的第 3 行。
	_, err = generated.ConvertSceneCSV(strings.NewReader("name,id\n# 注释\n\"多行\n名字\",x\n"))
	check(err != nil && strings.Contains(err.Error(), "row=3 field=ID"), "ConvertSceneCSV(multi-line record) = %v, want row=3", err)
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

// DataVersion 把表名算进去：数据一字不改、只改表名（标记 name=），版本号也要变。新名字 tips 与 tip
// 排序位置相同（仍在最后），所以变化只能来自表名本身，不是来自拼接顺序。
func TestClientExportDataVersionCoversTableNames(t *testing.T) {
	root := stageClientExportFixture(t)
	base := clientExport(t, root)
	path := filepath.Join(root, "schema", "schema.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(raw), "//roost:table name=tip side=c key=ID", "//roost:table name=tips side=c key=ID", 1)
	if renamed == string(raw) {
		t.Fatal("fixture has no tip marker")
	}
	writeTablegenTestFile(t, path, renamed)
	got := clientExport(t, root)
	if len(got.Tables) != len(base.Tables) {
		t.Fatalf("renaming changed the table count: %d -> %d", len(base.Tables), len(got.Tables))
	}
	for i := range got.Tables {
		want := base.Tables[i].Name
		if want == "tip" {
			want = "tips"
		}
		if got.Tables[i].Name != want || got.Tables[i].DataHash != base.Tables[i].DataHash {
			t.Fatalf("table %d = %s %s, want %s %s (same data, same order)", i, got.Tables[i].Name, got.Tables[i].DataHash, want, base.Tables[i].DataHash)
		}
	}
	if got.DataVersion == base.DataVersion {
		t.Fatalf("renaming tip to tips left the data version at %s", base.DataVersion)
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

// 具名类型（交接 C5 (a)，维护者 2026-11-17 选项 1）：schema 包里底层是内建标量的具名类型按底层类型处理——
// CSV 转 JSON（Element 底层是 string，"42" 写成字符串而不是数字）、index 键、导出模型的类型都看底层类型；
// 生成的服务端 Go 照常使用具名类型本身（写成 <别名>.Rank），编译运行后主键、index、ref、横排转换都成立。
// 类型声明可以在同包的另一个文件里，也可以是别名（Power）或具名类型的具名类型（Tier）。
func TestNamedScalarTypesFollowTheirUnderlyingType(t *testing.T) {
	root := t.TempDir()
	core, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	writeTablegenTestFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf("module example.com/named\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => %s\n", core))
	writeTablegenTestFile(t, filepath.Join(root, "schema", "types.go"), `package schema

type HeroID int32
type Rank int32
type Tier Rank
type Element string
type Rate float64
type Ready bool
type Power = int64
`)
	writeTablegenTestFile(t, filepath.Join(root, "schema", "hero.go"), "package schema\n\n"+
		"//roost:table name=hero key=ID\ntype Hero struct {\n"+
		"\tID      HeroID   `csv:\"id\" json:\"id\" required:\"true\"`\n"+
		"\tRank    Rank     `csv:\"rank\" json:\"rank\" index:\"true\" min:\"1\"`\n"+
		"\tTier    Tier     `csv:\"tier\" json:\"tier\"`\n"+
		"\tElement Element  `csv:\"element\" json:\"element\" index:\"true\"`\n"+
		"\tRate    Rate     `csv:\"rate\" json:\"rate\"`\n"+
		"\tReady   Ready    `csv:\"ready\" json:\"ready\"`\n"+
		"\tPower   Power    `csv:\"power\" json:\"power\"`\n"+
		"\tFriends []HeroID `csv:\"friends\" json:\"friends\"`\n}\n\n"+
		"//roost:table name=duel key=ID\ntype Duel struct {\n"+
		"\tID     int32  `csv:\"id\" json:\"id\"`\n"+
		"\tHeroID HeroID `csv:\"hero_id\" json:\"hero_id\" ref:\"hero\"`\n}\n")
	writeTablegenTestFile(t, filepath.Join(root, "table", "hero.csv"), "id,rank,tier,element,rate,ready,power,friends\n1,2,3,fire,1.5,true,9000000000,[2]\n2,1,1,42,0.25,false,1,[]\n3,2,2,42,2,true,7,\"[1,2]\"\n")
	writeTablegenTestFile(t, filepath.Join(root, "table", "duel.csv"), "id,hero_id\n1,3\n")
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-csv", filepath.Join(root, "table"), "-json", filepath.Join(root, "data"), "-force"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-meta", filepath.Join(root, "schema"), "-out", filepath.Join(root, "generated"), "-pkg", "generated", "-force"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	heroJSON, err := os.ReadFile(filepath.Join(root, "data", "hero.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"element": "42"`, `"rank": 2`, `"ready": true`} {
		if !strings.Contains(string(heroJSON), want) {
			t.Fatalf("hero.json lacks %s (the CSV is converted by the underlying type):\n%s", want, heroJSON)
		}
	}
	source, err := os.ReadFile(filepath.Join(root, "generated", "gen_table_config.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func HeroByRank(v meta1.Rank) []meta1.Hero", "func HeroByID(id meta1.HeroID)", "configdata.TableDef[meta1.HeroID, meta1.Hero]"} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("generated Go lacks %q:\n%s", want, source)
		}
	}
	writeTablegenTestFile(t, filepath.Join(root, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"example.com/named/generated"
	"example.com/named/schema"
	"github.com/tjbdwanghaibo/roost-core/framework/configdata"
)

func check(ok bool, format string, args ...any) {
	if !ok {
		fmt.Printf(format+"\n", args...)
		os.Exit(1)
	}
}

func ids(rows []generated.HeroCfg) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		parts[i] = fmt.Sprint(row.ID)
	}
	return strings.Join(parts, ",")
}

func main() {
	r := configdata.NewRegistry()
	check(generated.RegisterGeneratedConfigData(r) == nil, "register")
	store := configdata.NewStore(r, "data")
	configdata.SetDefaultStore(store)
	_, err := store.Load(context.Background())
	check(err == nil, "load: %v", err)

	hero, ok := generated.HeroByID(1)
	check(ok && hero.Rank == 2 && hero.Tier == 3 && hero.Element == "fire" && hero.Rate == 1.5 && bool(hero.Ready) && hero.Power == 9000000000 && reflect.DeepEqual(hero.Friends, []schema.HeroID{2}), "hero 1 = %+v", hero)
	check(ids(generated.HeroByRank(2)) == "1,3", "HeroByRank(2) = %s", ids(generated.HeroByRank(2)))
	check(ids(generated.HeroByElement("42")) == "2,3", "HeroByElement(42) = %s", ids(generated.HeroByElement("42")))
	check(len(generated.HeroByRank(9)) == 0, "HeroByRank(9) not empty")

	raw, err := os.ReadFile("table/hero.csv")
	check(err == nil, "read hero.csv: %v", err)
	rows, err := generated.ConvertHeroCSV(strings.NewReader(string(raw)))
	check(err == nil && len(rows) == 3 && rows[1].Element == "42" && rows[0].Rank == 2 && rows[0].Power == 9000000000 && !bool(rows[1].Ready) && rows[2].Rate == 2 && reflect.DeepEqual(rows[2].Friends, []schema.HeroID{1, 2}), "ConvertHeroCSV = %+v, %v", rows, err)
	_, err = generated.ConvertHeroCSV(strings.NewReader("id,rank\n1,x\n"))
	check(err != nil && strings.Contains(err.Error(), "field=Rank"), "ConvertHeroCSV(bad rank) = %v", err)
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated server binding over named types: %v\n%s", err, output)
	}

	// 导出模型：字段与 index 的类型是底层类型（客户端按它生成 C#）。
	model, err := ClientExport(filepath.Join(root, "schema"), filepath.Join(root, "table"))
	if err != nil {
		t.Fatal(err)
	}
	var hero ExportTable
	for _, table := range model.Tables {
		if table.Name == "hero" {
			hero = table
		}
	}
	var types []string
	for _, field := range hero.Fields {
		types = append(types, field.Name+":"+field.Type)
	}
	if got := strings.Join(types, ","); got != "ID:int32,Rank:int32,Tier:int32,Element:string,Rate:float64,Ready:bool,Power:int64,Friends:[]int32" {
		t.Fatalf("export hero field types = %s", got)
	}
	if !reflect.DeepEqual(hero.Indexes, []ExportIndex{{Name: "Rank", Field: "Rank", JSON: "rank", Type: "int32"}, {Name: "Element", Field: "Element", JSON: "element", Type: "string"}}) {
		t.Fatalf("export hero indexes = %+v", hero.Indexes)
	}
}
