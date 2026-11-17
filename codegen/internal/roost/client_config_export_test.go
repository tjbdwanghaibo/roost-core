package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
)

// client_export.config（配置表导出给客户端，维护者 2026-11-17，docs/maintenance/FEATURE-2026-11-17-client-config-export.md）
// 的回归用例。夹具：两张普通表（monster 带只给服务端的 drop_seed、只给客户端的 model、scene_id 上的 index、数组与字典字段、
// parser 字段）、一张竖排键值表（balance）、一张 side=c 的表（tip）。C# 的编译与运行见 TestClientConfigCSharpRuntime
// （要本机 dotnet，ROOST_TEST_DOTNET=1 时跑）。

const configExportTestSchema = `package schema

//roost:table name=scene key=ID
type Scene struct {
	ID   int32  ` + "`csv:\"id\" json:\"id\" title:\"编号\" required:\"true\"`" + `
	Name string ` + "`csv:\"name\" json:\"name\" title:\"名字\" required:\"true\" unique:\"true\"`" + `
	Kind string ` + "`csv:\"kind\" json:\"kind\" title:\"类型\" enum:\"field|dungeon\"`" + `
}

//roost:table name=monster key=ID
type Monster struct {
	ID       int32            ` + "`csv:\"id\" json:\"id\" title:\"编号\" required:\"true\"`" + `
	SceneID  int32            ` + "`csv:\"scene_id\" json:\"scene_id\" title:\"场景\" ref:\"scene\" index:\"true\"`" + `
	Level    int32            ` + "`csv:\"level\" json:\"level\" title:\"等级\" min:\"1\"`" + `
	DropSeed int64            ` + "`csv:\"drop_seed\" json:\"drop_seed\" title:\"掉落种子\" side:\"s\"`" + `
	Model    string           ` + "`csv:\"model\" json:\"model\" title:\"模型\" side:\"c\" required:\"true\"`" + `
	Tags     string           ` + "`csv:\"tags\" json:\"tags\" title:\"标签\" parser:\"json\"`" + `
	Rewards  []int64          ` + "`csv:\"rewards\" json:\"rewards\" title:\"奖励\"`" + `
	Weights  map[string]int32 ` + "`csv:\"weights\" json:\"weights\" title:\"权重\"`" + `
}

//roost:object name=balance layout=kv
type Balance struct {
	MaxLevel   int32  ` + "`csv:\"max_level\" json:\"max_level\" title:\"最高等级\" required:\"true\" min:\"1\"`" + `
	StartGold  int64  ` + "`csv:\"start_gold\" json:\"start_gold\" title:\"初始金币\"`" + `
	ServerTick int32  ` + "`csv:\"server_tick\" json:\"server_tick\" title:\"服务端心跳\" side:\"s\"`" + `
	ShopBanner string ` + "`csv:\"shop_banner\" json:\"shop_banner\" title:\"商店横幅\" side:\"c\"`" + `
}

//roost:table name=tip side=c key=ID
type Tip struct {
	ID   int32  ` + "`csv:\"id\" json:\"id\" required:\"true\"`" + `
	Text string ` + "`csv:\"text\" json:\"text\"`" + `
}
`

var configExportTestTables = map[string]string{
	"scene.csv": "id,name,kind\n# 场景表\n1,forest,field\n2,cave,dungeon\n",
	"monster.csv": "id,scene_id,level,drop_seed,model,tags,rewards,weights\n编号,场景,等级,掉落种子,模型,标签,奖励,权重\n# 同一场景可以有多只\n" +
		"10,1,3,777,slime.prefab,\"[\"\"a\"\"]\",\"[1,2]\",\"{\"\"a\"\":1}\"\n" +
		"11,2,5,888,bat.prefab,\"[\"\"b\"\"]\",,\n" +
		"12,1,4,999,orc.prefab,\"[\"\"c\"\"]\",[3],\"{\"\"b\"\":2,\"\"a\"\":3}\"\n",
	"balance.csv": "key,value,note\nmax_level,60,玩家等级上限\nstart_gold,100,新角色的金币\nserver_tick,50,只给服务端\n# 只给客户端\nshop_banner,summer_sale,商店顶部横幅 <夏季>\n",
	"tip.csv":     "id,text\n1,hello\n2,\"多行\n提示\"\n",
}

// 只给服务端的东西：字段名、标题、note 在导出目录的任何文件里都不能出现；它们在 CSV 里的值不能出现在导出的数据里
// （C# 与清单里有十六进制哈希，数字串可能碰巧出现，所以值只查数据文件）。
var (
	configExportServerOnlyNames  = []string{"drop_seed", "server_tick", "掉落种子", "服务端心跳", "只给服务端"}
	configExportServerOnlyValues = []string{"777", "888", "999"}
)

const configOnlyExportYAML = "client_export:\n  config: {}\n"

func newConfigExportProject(t *testing.T, exportYAML string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"config"}})
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "configs/schema/fixture.go", configExportTestSchema)
	for name, body := range configExportTestTables {
		writeProjectFile(t, root, "configs/table/"+name, body)
	}
	writeProjectFile(t, root, ManifestName, readProjectFile(t, root, ManifestName)+exportYAML)
	return root
}

func readConfigManifest(t *testing.T, root, dir string) clientConfigManifest {
	t.Helper()
	manifest, err := readClientConfigManifest(filepath.Join(root, filepath.FromSlash(dir), exportManifest))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// snapshotDir 读一个目录下全部文件（相对路径 → 内容）。
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClientConfigExportSpecValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		spec ClientExportSpec
		want string
	}{
		{ClientExportSpec{Config: &ClientConfigExportSpec{Dir: "configs/client"}}, "inside configs/"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Dir: "game/config"}}, "inside game/"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Dir: "/abs/config"}}, "relative"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Dir: "../config"}}, "inside the project"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Dir: "shared/bin"}}, "generation skips"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Tables: []string{"monster", "monster"}}}, "repeats"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{Tables: []string{" "}}}, "empty name"},
		{ClientExportSpec{Config: &ClientConfigExportSpec{CSharp: &ClientConfigCSharp{Namespace: "Game..Config"}}}, "not a C# namespace"},
		// 协议与配置各占一个目录（两边都写 manifest.json）。
		{ClientExportSpec{Dir: "shared/protocol", Config: &ClientConfigExportSpec{Dir: "shared/protocol"}}, "overlaps"},
		{ClientExportSpec{Dir: "shared", Config: &ClientConfigExportSpec{}}, "overlaps"},
		{ClientExportSpec{Languages: []string{"csharp"}, Config: &ClientConfigExportSpec{Dir: "shared/protocol/config"}}, "overlaps"},
	} {
		err := test.spec.validate()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("validate(%+v) = %v, want %q", test.spec, err, test.want)
		}
	}
	for _, spec := range []ClientExportSpec{
		{Config: &ClientConfigExportSpec{}},
		{Languages: []string{"csharp"}, Config: &ClientConfigExportSpec{Dir: "client/config", Tables: []string{"monster"}, CSharp: &ClientConfigCSharp{Namespace: "Game.Config"}}},
		// 只开配置时协议不导出，协议目录的缺省值不与配置目录冲突。
		{Config: &ClientConfigExportSpec{Dir: "shared/protocol"}},
	} {
		if err := spec.validate(); err != nil {
			t.Errorf("validate(%+v) = %v", spec, err)
		}
	}

	// 只写 config：只导出配置；写了 dir / languages / csharp 或没有 config：照旧导出协议。
	for _, test := range []struct {
		spec     *ClientExportSpec
		protocol bool
	}{
		{nil, false},
		{&ClientExportSpec{}, true},
		{&ClientExportSpec{Config: &ClientConfigExportSpec{}}, false},
		{&ClientExportSpec{Dir: "shared/protocol", Config: &ClientConfigExportSpec{}}, true},
		{&ClientExportSpec{Languages: []string{"csharp"}, Config: &ClientConfigExportSpec{}}, true},
	} {
		if got := test.spec.protocol(); got != test.protocol {
			t.Errorf("protocol(%+v) = %v, want %v", test.spec, got, test.protocol)
		}
	}

	spec := &ClientExportSpec{Config: &ClientConfigExportSpec{}}
	for rel, want := range map[string]bool{
		"shared/config/manifest.json":          true,
		"shared/config/data/monster.json":      true,
		"shared/config/csharp/ConfigTables.cs": true,
		"shared/config/README.md":              false,
		"shared/config/data/notes.txt":         false,
		"shared/config/data/sub/x.json":        false,
		"shared/config/csharp/Mine/Extra.cs":   false,
		"shared/configx/manifest.json":         false,
		"shared/protocol/manifest.json":        false, // 协议没开
		"shared/protocol/csharp/MessageIds.cs": false,
		"configs/data/monster.json":            false,
	} {
		if got := spec.owns(rel); got != want {
			t.Errorf("owns(%s) = %v, want %v", rel, got, want)
		}
	}

	// client_export.config 需要 config feature。
	manifest := DefaultManifest("b7", "example.com/b7", nil, nil, nil)
	manifest.Features = []string{"dao"}
	manifest.ClientExport = &ClientExportSpec{Config: &ClientConfigExportSpec{}}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "need the config feature") {
		t.Fatalf("config export without the config feature: %v", err)
	}
}

// 没声明 config 的工程：roost.yaml 写回时没有 config 键，生成器列表里没有 client-config-export；只声明 config 的工程不导出协议。
func TestUndeclaredClientConfigExportChangesNothing(t *testing.T) {
	t.Parallel()
	manifest := DefaultManifest("b7", "example.com/b7", []string{"config"}, nil, nil)
	manifest.ClientExport = &ClientExportSpec{Languages: []string{"csharp"}}
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, section, _ := strings.Cut(string(raw), "client_export:"); strings.Contains(section, "config") {
		t.Fatalf("undeclared client_export.config written to roost.yaml:\n%s", section)
	}
	names := func(m Manifest) string {
		var out []string
		for _, gen := range generatorsFor(m, false) {
			if strings.HasPrefix(gen.Name, "client-") {
				out = append(out, gen.Name)
			}
		}
		return strings.Join(out, ",")
	}
	if got := names(manifest); got != "client-export" {
		t.Fatalf("protocol-only export runs %q", got)
	}
	manifest.ClientExport = &ClientExportSpec{Config: &ClientConfigExportSpec{}}
	if got := names(manifest); got != "client-config-export" {
		t.Fatalf("config-only export runs %q (protoc must not be needed)", got)
	}
	manifest.ClientExport = nil
	if got := names(manifest); got != "" {
		t.Fatalf("no client_export runs %q", got)
	}
}

func TestGenerateClientConfigExport(t *testing.T) {
	t.Parallel()
	root := newConfigExportProject(t, configOnlyExportYAML)
	var out bytes.Buffer
	if err := Generate(root, GenerateOptions{Stdout: &out}); err != nil {
		t.Fatalf("generate: %v\n%s", err, out.String())
	}
	exportDir := filepath.Join(root, "shared", "config")
	files := snapshotDir(t, exportDir)
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "csharp/ConfigJson.cs,csharp/ConfigTables.Balance.cs,csharp/ConfigTables.Monster.cs,csharp/ConfigTables.Scene.cs,csharp/ConfigTables.Tip.cs,csharp/ConfigTables.cs,data/balance.json,data/monster.json,data/scene.json,data/tip.json,manifest.json" {
		t.Fatalf("export files = %s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "shared", "protocol")); !os.IsNotExist(err) {
		t.Fatalf("config-only export wrote the protocol export: %v", err)
	}

	// 只给服务端的字段与值绝不出现在导出目录里；只给客户端的都在。
	for name, body := range files {
		serverOnly := configExportServerOnlyNames
		if strings.HasPrefix(name, "data/") {
			serverOnly = append(append([]string{}, serverOnly...), configExportServerOnlyValues...)
		}
		for _, text := range serverOnly {
			if strings.Contains(body, text) || strings.Contains(name, text) {
				t.Errorf("export %s carries server-only %q", name, text)
			}
		}
	}
	for _, clientOnly := range []string{"slime.prefab", "summer_sale", "多行\\n提示"} {
		found := false
		for name, body := range files {
			if strings.HasPrefix(name, "data/") && strings.Contains(body, clientOnly) {
				found = true
			}
		}
		if !found {
			t.Errorf("export data lacks client value %q", clientOnly)
		}
	}
	if files["data/balance.json"] != "{\n  \"max_level\": 60,\n  \"shop_banner\": \"summer_sale\",\n  \"start_gold\": 100\n}\n" {
		t.Fatalf("data/balance.json = %s", files["data/balance.json"])
	}
	if !strings.HasPrefix(files["data/monster.json"], "{\n  \"rows\": [\n") {
		t.Fatalf("data/monster.json is not {\"rows\":[...]}: %s", files["data/monster.json"])
	}
	// 服务端 configs/data 里没有只给客户端的东西（tablegen 的投影，这里只抽查导出目录之外没混进来）。
	if raw := readProjectFile(t, root, "configs/data/monster.json"); strings.Contains(raw, "slime.prefab") || !strings.Contains(raw, "777") {
		t.Fatalf("server configs/data/monster.json = %s", raw)
	}

	// manifest：种类、版本号、每张表的哈希与行数、每个文件的哈希，与 tablegen 的模型一致。
	manifest := readConfigManifest(t, root, "shared/config")
	model, err := tablegen.ClientExport(filepath.Join(root, "configs", "schema"), filepath.Join(root, "configs", "table"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != "roost-config-export" || manifest.Schema != 1 || manifest.DataVersion != model.DataVersion ||
		manifest.DataVersionShort != model.ShortDataVersion() || manifest.Namespace != "Roost.Config" ||
		manifest.Omitted != (clientConfigOmitted{Tables: 0, Fields: 2}) || manifest.Generator == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Tables) != len(model.Tables) {
		t.Fatalf("manifest tables = %+v", manifest.Tables)
	}
	for i, table := range model.Tables {
		got := manifest.Tables[i]
		if got.Name != table.Name || got.DataHash != table.DataHash || got.SchemaHash != table.SchemaHash || got.Rows != table.Rows || got.File != "data/"+table.Name+".json" {
			t.Errorf("manifest table %d = %+v, model %s rows %d", i, got, table.Name, table.Rows)
		}
		if files[got.File] != string(table.Data) {
			t.Errorf("%s differs from the model data", got.File)
		}
	}
	if monster := manifest.Tables[1]; monster.Key != "id" || monster.Kind != "table" || monster.Rows != 3 || len(monster.Fields) != 7 || monster.Fields[4].Parser != "json" {
		t.Fatalf("manifest monster = %+v", monster)
	}
	if len(manifest.Files) != len(files)-1 {
		t.Fatalf("manifest lists %d files, export has %d besides manifest.json", len(manifest.Files), len(files)-1)
	}
	for name, hash := range manifest.Files {
		if contentHash([]byte(files[name])) != hash {
			t.Errorf("manifest hash of %s is wrong", name)
		}
	}
	csharp := files["csharp/ConfigTables.cs"]
	if !strings.Contains(csharp, `public const string DataVersion = "`+model.DataVersion+`";`) || !strings.Contains(csharp, "namespace Roost.Config") {
		t.Fatalf("ConfigTables.cs does not carry the data version:\n%s", csharp)
	}
	for _, want := range []string{
		"public sealed class Monster", "public int scene_id;", "public long[] rewards;", "Dictionary<string, int> weights;",
		"public sealed class MonsterTable", "BySceneID(int value)", "/// <summary>商店顶部横幅 &lt;夏季&gt;</summary>",
		"/// <summary>标签（parser json：CSV 原文字符串，由游戏自己解析）</summary>",
	} {
		if !strings.Contains(strings.Join([]string{files["csharp/ConfigTables.Monster.cs"], files["csharp/ConfigTables.Balance.cs"]}, "\n"), want) {
			t.Errorf("generated C# lacks %q", want)
		}
	}
	for _, unwanted := range []string{"UnityEngine", "JsonUtility", "System.Reflection", "static ConfigRule[] rules"} {
		for name, body := range files {
			if strings.HasSuffix(name, ".cs") && strings.Contains(body, unwanted) {
				t.Errorf("%s uses %q", name, unwanted)
			}
		}
	}
	for _, want := range []string{
		"client config export: 4 tables, omitted 0 tables / 2 fields",
		"client config data version: " + model.ShortDataVersion() + " (" + model.DataVersion + ")",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("generate output lacks %q:\n%s", want, out.String())
		}
	}

	// 同样的输入，导出逐字节相同；--check 与 doctor 都认为是最新的。
	if err := Generate(root, GenerateOptions{Stdout: io.Discard, Force: true}); err != nil {
		t.Fatal(err)
	}
	if again := snapshotDir(t, exportDir); fmt.Sprint(again) != fmt.Sprint(files) {
		t.Fatal("a second generate changed the export")
	}
	m := mustLoadManifest(t, root)
	if err := checkGenerated(context.Background(), root, m, io.Discard); err != nil {
		t.Fatalf("generate --check right after generate: %v", err)
	}
	if item := checkClientConfigExport(root, m); item.Status != StatusOK || !strings.Contains(item.Detail, "data version "+model.ShortDataVersion()) {
		t.Fatalf("config-export:fresh = %+v", item)
	}

	// 改 CSV 不重新生成：--check 与 doctor 都报过期，点名文件与表。
	writeProjectFile(t, root, "configs/table/monster.csv", strings.Replace(configExportTestTables["monster.csv"], "bat.prefab", "bat2.prefab", 1))
	err = checkGenerated(context.Background(), root, m, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "shared/config/data/monster.json") || !strings.Contains(err.Error(), "shared/config/manifest.json") {
		t.Fatalf("generate --check after a CSV edit = %v", err)
	}
	item := checkClientConfigExport(root, m)
	if item.Status != StatusFail || !strings.Contains(item.Detail, "table monster: data (configs/table/monster.csv) changed") || !strings.Contains(item.Detail, "data version") {
		t.Fatalf("config-export:fresh after a CSV edit = %+v", item)
	}
	// 只改只给服务端的列：导出不变，doctor 照样通过。
	writeProjectFile(t, root, "configs/table/monster.csv", strings.Replace(configExportTestTables["monster.csv"], "777", "778", 1))
	if item := checkClientConfigExport(root, m); item.Status != StatusOK {
		t.Fatalf("a server-only edit made config-export:fresh fail: %+v", item)
	}
	// 改 schema（加一个客户端字段）：doctor 报 schema 变化。
	writeProjectFile(t, root, "configs/schema/fixture.go", strings.Replace(configExportTestSchema, "\tText string", "\tIcon string `csv:\"icon\" json:\"icon\"`\n\tText string", 1))
	if item := checkClientConfigExport(root, m); item.Status != StatusFail || !strings.Contains(item.Detail, "table tip: schema (configs/schema) changed") {
		t.Fatalf("config-export:fresh after a schema edit = %+v", item)
	}
	// 导出目录被手改：doctor 报出该文件。
	writeProjectFile(t, root, "configs/schema/fixture.go", configExportTestSchema)
	writeProjectFile(t, root, "shared/config/data/tip.json", files["data/tip.json"]+" ")
	if item := checkClientConfigExport(root, m); item.Status != StatusFail || !strings.Contains(item.Detail, "data/tip.json does not match manifest.json") {
		t.Fatalf("config-export:fresh after a hand edit = %+v", item)
	}
}

func TestClientConfigExportTables(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		yaml, want string
		tables     []string
	}{
		{yaml: "client_export:\n  config:\n    tables: [scene, tip]\n", tables: []string{"scene", "tip"}},
		{yaml: "client_export:\n  config:\n    tables: [nope]\n", want: `lists "nope", which is not a table with client fields`},
		{yaml: "client_export:\n  config:\n    tables: [monster]\n", want: "refs table scene, which is not exported"},
	} {
		root := newConfigExportProject(t, test.yaml)
		var out bytes.Buffer
		err := Generate(root, GenerateOptions{Stdout: &out})
		if test.want != "" {
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("%s: generate = %v, want %q", test.yaml, err, test.want)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", test.yaml, err)
		}
		manifest := readConfigManifest(t, root, "shared/config")
		var names []string
		for _, table := range manifest.Tables {
			names = append(names, table.Name)
		}
		if strings.Join(names, ",") != strings.Join(test.tables, ",") || manifest.Omitted.Tables != 2 {
			t.Errorf("%s: exported %v, omitted %+v", test.yaml, names, manifest.Omitted)
		}
		if !strings.Contains(out.String(), "omitted 2 tables") {
			t.Errorf("omitted tables not printed:\n%s", out.String())
		}
		if _, err := os.Stat(filepath.Join(root, "shared", "config", "data", "monster.json")); !os.IsNotExist(err) {
			t.Errorf("unlisted table exported: %v", err)
		}
	}
	// side=s 的表不能列进 tables。
	root := newConfigExportProject(t, "client_export:\n  config:\n    tables: [secret]\n")
	writeProjectFile(t, root, "configs/schema/secret.go", "package schema\n\n//roost:table name=secret side=s key=ID\ntype Secret struct {\n\tID int32 `csv:\"id\" json:\"id\"`\n}\n")
	writeProjectFile(t, root, "configs/table/secret.csv", "id\n1\n")
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err == nil || !strings.Contains(err.Error(), `lists "secret"`) {
		t.Fatalf("a side=s table listed in tables: %v", err)
	}
}

// 暂存生成（roost generate 的正常路径）：导出随批次提交；去掉一张表后它的数据与 C# 文件被删；导出目录里别的文件不动。
func TestClientConfigExportTransactionalRetirement(t *testing.T) {
	t.Parallel()
	root := newConfigExportProject(t, "client_export:\n  config:\n    dir: client/config\n    csharp:\n      namespace: Game.Config\n")
	writeProjectFile(t, root, "client/config/README.md", "client notes\n")
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard, deferDependencies: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if manifest := readConfigManifest(t, root, "client/config"); manifest.Namespace != "Game.Config" || len(manifest.Tables) != 4 {
		t.Fatalf("committed manifest = %+v", manifest)
	}
	if !strings.Contains(readProjectFile(t, root, "client/config/csharp/ConfigTables.cs"), "namespace Game.Config") {
		t.Fatal("namespace not applied")
	}
	writeProjectFile(t, root, "configs/schema/fixture.go", configExportTestSchema[:strings.Index(configExportTestSchema, "//roost:table name=tip")])
	if err := os.Remove(filepath.Join(root, "configs", "table", "tip.csv")); err != nil {
		t.Fatal(err)
	}
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard, deferDependencies: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"client/config/data/tip.json", "client/config/csharp/ConfigTables.Tip.cs"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s survived dropping the table: %v", rel, err)
		}
	}
	if manifest := readConfigManifest(t, root, "client/config"); len(manifest.Tables) != 3 {
		t.Fatalf("manifest after dropping tip = %+v", manifest.Tables)
	}
	if readProjectFile(t, root, "client/config/README.md") != "client notes\n" {
		t.Fatal("a file the generator does not own was changed")
	}
	m := mustLoadManifest(t, root)
	if err := checkGenerated(context.Background(), root, m, io.Discard); err != nil {
		t.Fatalf("generate --check after the transactional generate: %v", err)
	}
}

func TestClientConfigSync(t *testing.T) {
	t.Parallel()
	root := newConfigExportProject(t, configOnlyExportYAML)
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	client := t.TempDir()
	code, data := filepath.Join(client, "Assets", "Scripts", "Config"), filepath.Join(client, "Assets", "StreamingAssets", "Config")
	first := readConfigManifest(t, root, "shared/config")

	// 首次同步：C# 进代码目录，数据进数据目录，服务端清单另存为同步记录。
	var out bytes.Buffer
	if err := ClientConfigSync(root, code, data, false, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{"files: 10 added, 0 updated, 0 removed, 0 unchanged", "added data/monster.json", "added csharp/ConfigTables.cs", "first config sync", "config data version: none -> " + first.DataVersionShort} {
		if !strings.Contains(report, want) {
			t.Errorf("first sync report lacks %q:\n%s", want, report)
		}
	}
	for name, body := range snapshotDir(t, filepath.Join(root, "shared", "config")) {
		target := filepath.Join(code, filepath.Base(name))
		switch {
		case strings.HasPrefix(name, "data/"):
			target = filepath.Join(data, filepath.Base(name))
		case name == exportManifest:
			target = filepath.Join(code, clientConfigSyncRecord)
		}
		if got := readProjectFile(t, filepath.Dir(target), filepath.Base(target)); got != body {
			t.Errorf("client %s differs from the export %s", target, name)
		}
	}
	if record, err := readClientConfigManifest(filepath.Join(code, clientConfigSyncRecord)); err != nil || record.DataVersion != first.DataVersion {
		t.Fatalf("sync record data version = %s, want %s", record.DataVersion, first.DataVersion)
	}
	for _, dir := range []string{code, data} {
		if entries, _ := filepath.Glob(filepath.Join(dir, "*.meta")); len(entries) != 0 {
			t.Fatalf("sync wrote Unity .meta files: %v", entries)
		}
	}

	// 增量：monster 加一行、去掉 weights、level 改成 int64、加 icon；去掉 tip 表；balance 不变。
	schema := strings.Replace(configExportTestSchema, "\tWeights  map[string]int32 `csv:\"weights\" json:\"weights\" title:\"权重\"`\n", "\tIcon     string           `csv:\"icon\" json:\"icon\" title:\"图标\"`\n", 1)
	schema = strings.Replace(schema, "\tLevel    int32 ", "\tLevel    int64 ", 1)
	schema = schema[:strings.Index(schema, "//roost:table name=tip")]
	writeProjectFile(t, root, "configs/schema/fixture.go", schema)
	writeProjectFile(t, root, "configs/table/monster.csv", "id,scene_id,level,drop_seed,model,tags,rewards,icon\n"+
		"10,1,3,777,slime.prefab,\"[\"\"a\"\"]\",\"[1,2]\",s.png\n11,2,5,888,bat.prefab,,,\n12,1,4,999,orc.prefab,,[3],\n13,2,6,1,ghost.prefab,,,\n")
	if err := os.Remove(filepath.Join(root, "configs", "table", "tip.csv")); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	second := readConfigManifest(t, root, "shared/config")
	out.Reset()
	if err := ClientConfigSync(root, code, data, true, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "tip.json")); err != nil || !strings.Contains(out.String(), "would sync") {
		t.Fatalf("dry run wrote or removed files: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := ClientConfigSync(root, code, data, false, &out); err != nil {
		t.Fatal(err)
	}
	report = out.String()
	for _, want := range []string{
		"config data version: " + first.DataVersionShort + " -> " + second.DataVersionShort,
		"removed data/tip.json", "removed csharp/ConfigTables.Tip.cs", "updated data/monster.json", "updated csharp/ConfigTables.Monster.cs",
		"config tables removed (1):\n  tip (table, 2 row(s))",
		"config fields added (1):\n  monster.icon string",
		"config fields removed (1):\n  monster.weights map[string]int32",
		"config fields changed type (1):\n  monster.level: int32 -> int64",
		"config rows changed (1):\n  monster: 3 -> 4",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("incremental sync report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "balance") {
		t.Errorf("unchanged table reported:\n%s", report)
	}
	for _, gone := range []string{filepath.Join(data, "tip.json"), filepath.Join(code, "ConfigTables.Tip.cs")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still in the client after the server dropped it: %v", gone, err)
		}
	}
	if record, err := readClientConfigManifest(filepath.Join(code, clientConfigSyncRecord)); err != nil || record.DataVersion != second.DataVersion {
		t.Fatalf("sync record data version = %s, want %s", record.DataVersion, second.DataVersion)
	}
	// 再同步一次：没有变化。
	out.Reset()
	if err := ClientConfigSync(root, code, data, false, &out); err != nil || !strings.Contains(out.String(), "config data version: "+second.DataVersionShort+" (unchanged)") {
		t.Fatalf("no-op sync: %v\n%s", err, out.String())
	}

	// 客户端目录里不是上次同步写的同名文件（手写的）：拒绝覆盖，一个文件都不写。
	foreignCode, foreignData := filepath.Join(t.TempDir(), "Code"), filepath.Join(t.TempDir(), "Data")
	writeProjectFile(t, foreignData, "monster.json", "{\"mine\":true}\n")
	err := ClientConfigSync(root, foreignCode, foreignData, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refuse to overwrite "+filepath.Join(foreignData, "monster.json")) {
		t.Fatalf("overwrote a hand-written client file: %v", err)
	}
	if readProjectFile(t, foreignData, "monster.json") != "{\"mine\":true}\n" {
		t.Fatal("hand-written client file changed")
	}
	if _, err := os.Stat(foreignCode); !os.IsNotExist(err) {
		t.Fatalf("a refused sync wrote other files: %v", err)
	}
	// 服务端导出目录被手改：拒绝同步。
	writeProjectFile(t, root, "shared/config/data/scene.json", readProjectFile(t, root, "shared/config/data/scene.json")+" ")
	if err := ClientConfigSync(root, code, data, false, io.Discard); err == nil || !strings.Contains(err.Error(), "data/scene.json does not match") {
		t.Fatalf("synced a hand-edited export: %v", err)
	}
	// 命令行：两个配置目录必须一起给；只有配置导出的工程不能用 --to。
	if err := runClient([]string{"sync", "--from", root, "--config-code-to", code}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "go together") {
		t.Fatalf("--config-code-to alone: %v", err)
	}
	if err := runClient([]string{"sync", "--from", root, "--to", code}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "does not export the protocol") {
		t.Fatalf("--to on a config-only export: %v", err)
	}
}

func TestConfigCSharpTypes(t *testing.T) {
	t.Parallel()
	for goType, want := range map[string]string{
		"string": "string", "bool": "bool", "int8": "sbyte", "int16": "short", "int32": "int", "int": "long", "int64": "long",
		"uint8": "byte", "uint16": "ushort", "uint32": "uint", "uint": "ulong", "uint64": "ulong", "float32": "float", "float64": "double",
		"*int32": "int?", "*string": "string", "[]string": "string[]", "[][]int32": "int[][]", "[]*int64": "long?[]",
		"map[string]int32":    "global::System.Collections.Generic.Dictionary<string, int>",
		"map[string][]string": "global::System.Collections.Generic.Dictionary<string, string[]>",
	} {
		got, err := csharpType(goType, false)
		if err != nil || got.decl != want {
			t.Errorf("csharpType(%s) = %q, %v; want %q", goType, got.decl, err, want)
		}
	}
	if got, err := csharpType("[]Reward", true); err != nil || got.decl != "string" {
		t.Errorf("a parser field = %q, %v; want string", got.decl, err)
	}
	nested, _ := csharpType("map[string][]int32", false)
	if got := nested.read("node", "site", 1); got != "ConfigJson.ReadMap<int[]>(node, site, (n1, s1) => ConfigJson.ReadArray<int>(n1, s1, (n2, s2) => ConfigJson.ReadInt32(n2, s2)))" {
		t.Errorf("nested reader = %s", got)
	}
	for goType, want := range map[string]string{
		"time.Duration": "not supported", "Reward": "not supported", "[]byte": "base64", "map[int32]string": "only map[string]T", "*[]int32": "only pointers",
	} {
		if _, err := csharpType(goType, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("csharpType(%s) = %v, want %q", goType, err, want)
		}
	}
	if csString("a\"b\\c\n\u2028") != `"a\"b\\c\u000a\u2028"` {
		t.Errorf("csString = %s", csString("a\"b\\c\n\u2028"))
	}

	table := func(name, typeName string, fields ...tablegen.ExportField) tablegen.ExportTable {
		return tablegen.ExportTable{Name: name, TypeName: typeName, Kind: tablegen.KindObject, Fields: fields}
	}
	field := func(json, typ string) tablegen.ExportField {
		return tablegen.ExportField{Name: json, JSON: json, CSV: json, Type: typ}
	}
	for _, test := range []struct {
		tables []tablegen.ExportTable
		want   string
	}{
		{[]tablegen.ExportTable{table("a", "ConfigJson", field("x", "int32"))}, "collides with a generated runtime type"},
		{[]tablegen.ExportTable{table("a", "Load", field("x", "int32"))}, "member of ConfigTables"},
		{[]tablegen.ExportTable{table("a", "Item", field("x", "int32")), table("b", "ItemReader", field("x", "int32"))}, "collides with table a"},
		{[]tablegen.ExportTable{table("a", "Item", field("Item", "int32"))}, "equals the type name"},
		{[]tablegen.ExportTable{table("a", "Item", field("max-level", "int32"))}, "not a C# identifier"},
		{[]tablegen.ExportTable{table("a", "Item", field("x", "time.Duration"))}, "table a field x: type time.Duration is not supported"},
	} {
		if _, err := renderConfigCSharp(test.tables, "Game.Config", "v"); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("renderConfigCSharp(%+v) = %v, want %q", test.tables, err, test.want)
		}
	}
	// C# 关键字作字段名：写成 @名字。
	files, err := renderConfigCSharp([]tablegen.ExportTable{table("a", "Item", field("class", "string"))}, "Game.Config", "v")
	if err != nil || !strings.Contains(string(files["ConfigTables.Item.cs"]), "public string @class;") {
		t.Fatalf("keyword field: %v\n%s", err, files["ConfigTables.Item.cs"])
	}
}

// configCSharpModel 构造一张直接写好数据的导出表（不经 CSV），用于让 C# 读到服务端生成时不会放过的数据。
func configCSharpModel(table tablegen.ExportTable, data string) tablegen.ExportTable {
	table.Data = []byte(data)
	table.DataHash = tablegen.DataHash(table.Data)
	return table
}

// TestClientConfigCSharpRuntime 在 netstandard2.1 工程里编译导出的 C#（LangVersion 8、Nullable、警告即错误、XML 文档），
// 再用一个 net 控制台程序跑：Load 读回全部数据、两次 Load 互不相干、CRLF 照常读、改任何一份 JSON 报出表名、
// 违反 required / ref / unique / min / enum 报出表、行与字段、类型不对报出位置。要本机 dotnet（不下载依赖），
// ROOST_TEST_DOTNET=1 时跑。
func TestClientConfigCSharpRuntime(t *testing.T) {
	if os.Getenv("ROOST_TEST_DOTNET") != "1" {
		t.Skip("set ROOST_TEST_DOTNET=1 to compile and run the exported C# with the local dotnet SDK")
	}
	dotnet, err := exec.LookPath("dotnet")
	if err != nil {
		t.Fatalf("ROOST_TEST_DOTNET=1 but dotnet is not on PATH: %v", err)
	}
	root := newConfigExportProject(t, "client_export:\n  config:\n    csharp:\n      namespace: Good.Config\n")
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	exportDir := filepath.Join(root, "shared", "config")
	copyTree := func(from, to string) {
		for name, body := range snapshotDir(t, from) {
			writeProjectFile(t, to, name, body)
		}
	}
	copyTree(filepath.Join(exportDir, "csharp"), filepath.Join(work, "gen", "good"))
	copyTree(filepath.Join(exportDir, "data"), filepath.Join(work, "data", "good"))

	// 坏数据：从夹具的模型出发，直接改一张表的数据（哈希随之更新），各放一个命名空间。
	model, err := tablegen.ClientExport(filepath.Join(root, "configs", "schema"), filepath.Join(root, "configs", "table"))
	if err != nil {
		t.Fatal(err)
	}
	tableByName := map[string]tablegen.ExportTable{}
	for _, table := range model.Tables {
		tableByName[table.Name] = table
	}
	monsterRows := func(second string) string {
		return `{"rows":[{"id":10,"scene_id":1,"level":3,"model":"slime.prefab","tags":"[]","rewards":null,"weights":null},` + second + `]}`
	}
	bad := []struct {
		name  string
		table tablegen.ExportTable
	}{
		{"required", configCSharpModel(tableByName["monster"], monsterRows(`{"id":11,"scene_id":2,"level":5}`))},
		{"ref", configCSharpModel(tableByName["monster"], monsterRows(`{"id":11,"scene_id":9,"level":5,"model":"bat.prefab"}`))},
		{"unique", configCSharpModel(tableByName["scene"], `{"rows":[{"id":1,"name":"forest","kind":"field"},{"id":2,"name":"forest","kind":"field"}]}`)},
		{"min", configCSharpModel(tableByName["monster"], monsterRows(`{"id":11,"scene_id":2,"level":0,"model":"bat.prefab"}`))},
		{"enum", configCSharpModel(tableByName["scene"], `{"rows":[{"id":1,"name":"forest","kind":"field"},{"id":2,"name":"cave","kind":"sky"}]}`)},
		{"kvrequired", configCSharpModel(tableByName["balance"], `{"start_gold":1}`)},
		{"type", configCSharpModel(tableByName["monster"], monsterRows(`{"id":11,"scene_id":2,"level":5,"model":"bat.prefab","rewards":[1,"x"]}`))},
		{"overflow", configCSharpModel(tableByName["monster"], monsterRows(`{"id":11,"scene_id":2,"level":3000000000,"model":"bat.prefab"}`))},
	}
	for _, item := range bad {
		var tables []tablegen.ExportTable
		for _, table := range model.Tables {
			if table.Name == item.table.Name {
				table = item.table
			}
			tables = append(tables, table)
			writeProjectFile(t, filepath.Join(work, "data", item.name), table.Name+".json", string(table.Data))
		}
		sources, err := renderConfigCSharp(tables, "Bad."+strings.ToUpper(item.name[:1])+item.name[1:], tablegen.DataVersion(tables))
		if err != nil {
			t.Fatal(err)
		}
		for name, raw := range sources {
			writeProjectFile(t, filepath.Join(work, "gen", item.name), name, string(raw))
		}
	}

	// 类型覆盖：可空、嵌套数组、字典里的数组、浮点、无符号、bool、关键字字段，读回的值逐个核对。
	types := tablegen.ExportTable{Name: "extra", TypeName: "Extra", Kind: tablegen.KindTable, Key: "Code", Fields: []tablegen.ExportField{
		{Name: "Code", JSON: "code", Type: "string", Key: true},
		{Name: "Bonus", JSON: "bonus", Type: "*int32"},
		{Name: "Alias", JSON: "alias", Type: "*string"},
		{Name: "Grid", JSON: "grid", Type: "[][]int32"},
		{Name: "Groups", JSON: "groups", Type: "map[string][]string"},
		{Name: "Rate", JSON: "rate", Type: "float32"},
		{Name: "Big", JSON: "big", Type: "uint64"},
		{Name: "On", JSON: "on", Type: "bool"},
		{Name: "Class", JSON: "class", Type: "uint8"},
	}}
	types = configCSharpModel(types, "{\"rows\":[\r\n{\"code\":\"a\\u00e9\\n\\\"q\\\"\",\"bonus\":7,\"alias\":null,\"grid\":[[1,2],[]],\"groups\":{\"x\":[\"p\",\"q\"]},\"rate\":1.5,\"big\":18446744073709551615,\"on\":true,\"class\":255},\r\n"+
		"{\"code\":\"b\",\"bonus\":null,\"alias\":\"bb\",\"grid\":null,\"groups\":null,\"rate\":-2e-1,\"big\":0,\"on\":false}]}\r\n")
	sources, err := renderConfigCSharp([]tablegen.ExportTable{types}, "Types.Config", tablegen.DataVersion([]tablegen.ExportTable{types}))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range sources {
		writeProjectFile(t, filepath.Join(work, "gen", "types"), name, string(raw))
	}
	writeProjectFile(t, filepath.Join(work, "data", "types"), "extra.json", string(types.Data))

	writeProjectFile(t, work, "lib/Lib.csproj", `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>netstandard2.1</TargetFramework>
    <LangVersion>8</LangVersion>
    <Nullable>enable</Nullable>
    <TreatWarningsAsErrors>true</TreatWarningsAsErrors>
    <GenerateDocumentationFile>true</GenerateDocumentationFile>
    <EnableDefaultCompileItems>false</EnableDefaultCompileItems>
  </PropertyGroup>
  <ItemGroup>
    <Compile Include="../gen/**/*.cs" />
  </ItemGroup>
</Project>
`)
	writeProjectFile(t, work, "app/App.csproj", `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <OutputType>Exe</OutputType>
    <TargetFramework>net`+dotnetMajor(t, dotnet)+`.0</TargetFramework>
    <Nullable>disable</Nullable>
  </PropertyGroup>
  <ItemGroup>
    <ProjectReference Include="../lib/Lib.csproj" />
  </ItemGroup>
</Project>
`)
	writeProjectFile(t, work, "app/Program.cs", configCSharpProgram)
	env := append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer func() {
		shutdown := exec.Command(dotnet, "build-server", "shutdown")
		shutdown.Env = env
		_ = shutdown.Run()
	}()
	build := exec.CommandContext(ctx, dotnet, "build", "--disable-build-servers", "-nologo", "-v", "q", "-o", filepath.Join(work, "out"), filepath.Join(work, "app", "App.csproj"))
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("dotnet build: %v\n%s", err, output)
	}
	run := exec.CommandContext(ctx, dotnet, filepath.Join(work, "out", "App.dll"), filepath.Join(work, "data"))
	run.Env = env
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("exported C# harness: %v\n%s", err, output)
	}
	got := string(output)
	t.Logf("harness output:\n%s", got)

	// 读回全部数据：每张表每行每个字段，与导出的 JSON 逐个比。
	for _, table := range model.Tables {
		for _, line := range expectedDump(t, table) {
			if !strings.Contains(got, line+"\n") {
				t.Errorf("Load did not read back %q", line)
			}
		}
	}
	want := []string{
		"VERSION " + model.DataVersion,
		"GET monster 10 slime.prefab tags=[\"a\"]",
		"INDEX scene_id=1 -> 10,12; scene_id=3 -> (none)",
		"MISSING monster 99 KeyNotFoundException",
		"BALANCE max_level=60 start_gold=100 shop_banner=summer_sale",
		"INDEPENDENT ok",
		"CRLF ok",
		"NULL table balance: read: readText returned null for balance.json",
		"TYPES a\u00e9\n\"q\" bonus=7 alias=(null) grid=[1,2];[] groups=x:p,q rate=1.5 big=18446744073709551615 on=True class=255",
		"TYPES b bonus=(null) alias=bb grid= groups= rate=-0.2 big=0 on=False class=0",
		"BAD required: table monster row 2 (key 11) field model: required: missing or null",
		"BAD ref: table monster row 2 (key 11) field scene_id: ref: references missing scene key 9",
		"BAD unique: table scene row 2 (key 2) field name: unique: value forest repeats row 1",
		"BAD min: table monster row 2 (key 11) field level: min: value 0 is below min=1",
		"BAD enum: table scene row 2 (key 2) field kind: enum: value sky is not one of [field dungeon]",
		"BAD kvrequired: table balance field max_level: required: missing or null",
		"BAD type: table monster row 2 (key 11) field rewards[1]: type: want an int64, got a string",
		"BAD overflow: table monster row 2 (key 11) field level: type: want an int32, got the number 3000000000",
	}
	for _, table := range model.Tables {
		want = append(want, fmt.Sprintf("TAMPER %s.json -> table=%s rule=hash", table.Name, table.Name))
	}
	for _, line := range want {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("harness output lacks %q", line)
		}
	}
	if strings.Contains(got, "UNEXPECTED") {
		t.Errorf("harness reported an unexpected result")
	}
}

func dotnetMajor(t *testing.T, dotnet string) string {
	t.Helper()
	output, err := exec.Command(dotnet, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	major, _, _ := strings.Cut(strings.TrimSpace(string(output)), ".")
	return major
}

// expectedDump 把一张表的导出 JSON 写成控制台程序 DUMP 的格式：DUMP <表> <行号> <字段>=<值>...（字段按 schema 顺序；
// 字符串原样，数组 [a,b]，字典按键排序 {k:v}，null 的数组 / 字典为空）。
func expectedDump(t *testing.T, table tablegen.ExportTable) []string {
	t.Helper()
	var rows []map[string]any
	if table.Kind == tablegen.KindTable {
		var doc struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(table.Data, &doc); err != nil {
			t.Fatal(err)
		}
		rows = doc.Rows
	} else {
		var row map[string]any
		if err := json.Unmarshal(table.Data, &row); err != nil {
			t.Fatal(err)
		}
		rows = []map[string]any{row}
	}
	var format func(value any, typ string) string
	format = func(value any, typ string) string {
		switch {
		case strings.HasPrefix(typ, "[]"):
			items, _ := value.([]any)
			parts := make([]string, len(items))
			for i, item := range items {
				parts[i] = format(item, strings.TrimPrefix(typ, "[]"))
			}
			return "[" + strings.Join(parts, ",") + "]"
		case strings.HasPrefix(typ, "map[string]"):
			entries, _ := value.(map[string]any)
			keys := make([]string, 0, len(entries))
			for key := range entries {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			parts := make([]string, len(keys))
			for i, key := range keys {
				parts[i] = key + ":" + format(entries[key], strings.TrimPrefix(typ, "map[string]"))
			}
			return "{" + strings.Join(parts, ",") + "}"
		}
		if value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
	var lines []string
	for i, row := range rows {
		parts := []string{"DUMP", table.Name, fmt.Sprint(i + 1)}
		for _, field := range table.Fields {
			typ := field.Type
			if field.Parser != "" {
				typ = "string"
			}
			parts = append(parts, field.JSON+"="+format(row[field.JSON], typ))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return lines
}

// configCSharpProgram 是驱动导出 C# 的控制台程序（只在测试里用；它用反射把行打印出来对比，生成的代码本身不用反射）。
const configCSharpProgram = `using System;
using System.Collections;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Text;

static class Program
{
    static string dataRoot;

    static Func<string, string> Reader(string set, Func<string, string, string> edit = null)
    {
        return file =>
        {
            string text = File.ReadAllText(Path.Combine(dataRoot, set, file));
            return edit == null ? text : edit(file, text);
        };
    }

    static string Show(object value)
    {
        if (value == null) return "";
        if (value is string s) return s;
        if (value is IDictionary map)
        {
            var keys = map.Keys.Cast<string>().OrderBy(k => k, StringComparer.Ordinal);
            return "{" + string.Join(",", keys.Select(k => k + ":" + Show(map[k]))) + "}";
        }
        if (value is IEnumerable items)
        {
            return "[" + string.Join(",", items.Cast<object>().Select(Show)) + "]";
        }
        if (value is bool b) return b ? "true" : "false";
        if (value is IFormattable f) return f.ToString(null, System.Globalization.CultureInfo.InvariantCulture);
        return value.ToString();
    }

    static void Dump(string table, int row, object item)
    {
        var fields = item.GetType().GetFields(BindingFlags.Public | BindingFlags.Instance).OrderBy(x => x.MetadataToken);
        Console.Write("DUMP " + table + " " + row);
        foreach (var field in fields)
        {
            Console.Write(" " + field.Name + "=" + Show(field.GetValue(item)));
        }
        Console.Write("\n");
    }

    static void Line(string text)
    {
        Console.Write(text + "\n");
    }

    static void Expect(string label, Action load)
    {
        try
        {
            load();
            Line("UNEXPECTED " + label + " loaded");
        }
        catch (Exception e)
        {
            Line("BAD " + label + ": " + e.Message);
        }
    }

    static int Main(string[] args)
    {
        dataRoot = args[0];
        var tables = Good.Config.ConfigTables.Load(Reader("good"));
        Line("VERSION " + Good.Config.ConfigTables.DataVersion);
        for (int i = 0; i < tables.Monster.Count; i++) Dump("monster", i + 1, tables.Monster.All[i]);
        for (int i = 0; i < tables.Scene.Count; i++) Dump("scene", i + 1, tables.Scene.All[i]);
        for (int i = 0; i < tables.Tip.Count; i++) Dump("tip", i + 1, tables.Tip.All[i]);
        Dump("balance", 1, tables.Balance);
        var slime = tables.Monster.Get(10);
        Line("GET monster 10 " + slime.model + " tags=" + slime.tags);
        Line("INDEX scene_id=1 -> " + string.Join(",", tables.Monster.BySceneID(1).Select(m => m.id)) +
             "; scene_id=3 -> " + (tables.Monster.BySceneID(3).Count == 0 ? "(none)" : "?"));
        Good.Config.Monster missing;
        if (tables.Monster.TryGet(99, out missing)) Line("UNEXPECTED TryGet 99");
        try { tables.Monster.Get(99); Line("UNEXPECTED Get 99"); }
        catch (KeyNotFoundException) { Line("MISSING monster 99 KeyNotFoundException"); }
        Line("BALANCE max_level=" + tables.Balance.max_level + " start_gold=" + tables.Balance.start_gold + " shop_banner=" + tables.Balance.shop_banner);

        var again = Good.Config.ConfigTables.Load(Reader("good"));
        again.Monster.Get(10).level = 99;
        again.Balance.max_level = 1;
        bool independent = !ReferenceEquals(tables, again) && !ReferenceEquals(tables.Monster, again.Monster)
            && tables.Monster.Get(10).level == 3 && tables.Balance.max_level == 60 && again.Monster.Get(10).level == 99;
        Line(independent ? "INDEPENDENT ok" : "UNEXPECTED shared state");

        Good.Config.ConfigTables.Load(Reader("good", (file, text) => text.Replace("\r\n", "\n").Replace("\n", "\r\n")));
        Line("CRLF ok");

        foreach (string file in Good.Config.ConfigTables.DataFiles())
        {
            try
            {
                Good.Config.ConfigTables.Load(Reader("good", (name, text) => name == file ? text.Replace("\n}", " \n}") : text));
                Line("UNEXPECTED tampered " + file + " loaded");
            }
            catch (Good.Config.ConfigLoadException e)
            {
                Line("TAMPER " + file + " -> table=" + e.Table + " rule=" + e.Rule + (e.Message.StartsWith("table " + e.Table + ": hash: ") ? "" : " UNEXPECTED message " + e.Message));
            }
        }
        try
        {
            Good.Config.ConfigTables.Load(file => null);
            Line("UNEXPECTED null loaded");
        }
        catch (Good.Config.ConfigLoadException e)
        {
            Line("NULL " + e.Message);
        }

        var extra = Types.Config.ConfigTables.Load(Reader("types")).Extra;
        foreach (var row in extra.All)
        {
            Line("TYPES " + row.code + " bonus=" + (row.bonus.HasValue ? row.bonus.Value.ToString() : "(null)") + " alias=" + (row.alias ?? "(null)") +
                 " grid=" + string.Join(";", row.grid.Select(r => "[" + string.Join(",", r) + "]")) +
                 " groups=" + string.Join(";", row.groups.Select(g => g.Key + ":" + string.Join(",", g.Value))) +
                 " rate=" + row.rate.ToString(System.Globalization.CultureInfo.InvariantCulture) + " big=" + row.big + " on=" + row.on + " class=" + row.@class);
        }

        Expect("required", () => Bad.Required.ConfigTables.Load(Reader("required")));
        Expect("ref", () => Bad.Ref.ConfigTables.Load(Reader("ref")));
        Expect("unique", () => Bad.Unique.ConfigTables.Load(Reader("unique")));
        Expect("min", () => Bad.Min.ConfigTables.Load(Reader("min")));
        Expect("enum", () => Bad.Enum.ConfigTables.Load(Reader("enum")));
        Expect("kvrequired", () => Bad.Kvrequired.ConfigTables.Load(Reader("kvrequired")));
        Expect("type", () => Bad.Type.ConfigTables.Load(Reader("type")));
        Expect("overflow", () => Bad.Overflow.ConfigTables.Load(Reader("overflow")));
        return 0;
    }
}
`
