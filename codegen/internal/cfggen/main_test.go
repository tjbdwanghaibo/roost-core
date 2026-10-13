package cfggen

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const demoMeta = `package: cfg
beans:
  - name: DropItem
    fields:
      - { name: item_id, type: int32 }
      - { name: weight,  type: int32 }
tables:
  - name: drop
    key: id
    fields:
      - { name: id,    type: int32 }
      - { name: items, type: "[]DropItem" }
  - name: monster
    key: id
    comment: 怪物表
    fields:
      - { name: id,       type: int32 }
      - { name: name,     type: string }
      - { name: scene_id, type: int32, index: true }
      - { name: drop_id,  type: int32, ref: drop }
globals:
  - name: world
    fields:
      - { name: width, type: int32 }
`

func runCfggen(t *testing.T, meta string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cfg")
	var stdout bytes.Buffer
	if err := Run([]string{"-meta", metaPath, "-out", out}, &stdout); err != nil {
		return "", err
	}
	source, err := os.ReadFile(filepath.Join(out, generatedFileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(source), nil
}

func TestCfggenGeneratesStructsRegistrationAndAccessors(t *testing.T) {
	source, err := runCfggen(t, demoMeta)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"package cfg",
		"type DropItem struct {",
		"ItemID int32 `json:\"item_id\"`",
		"type MonsterCfg struct {",
		"ID      int32  `json:\"id\" cfg:\"key\"`",
		"SceneID int32  `json:\"scene_id\" cfg:\"index\"`",
		"DropID  int32  `json:\"drop_id\" cfg:\"ref=drop\"`",
		"Items []DropItem `json:\"items\"`",
		"type WorldCfg struct {",
		"func RegisterGeneratedConfigData(r *configdata.Registry) error {",
		"configdata.RegisterAutoTable[int32, MonsterCfg](r, configdata.WithAutoName(\"monster\"), configdata.WithAutoFile(\"monster.json\"))",
		"configdata.RegisterObject(r, configdata.ObjectDef[WorldCfg]{Name: \"world\", File: \"world.json\"})",
		"func MonsterTableFrom(snap *configdata.Snapshot) (*configdata.Table[int32, MonsterCfg], bool) {",
		"func MonsterBySceneID(snap *configdata.Snapshot, sceneID int32) []MonsterCfg {",
		"return table.GetByIndex(\"scene_id\", strconv.FormatInt(int64(sceneID), 10))",
		"func WorldFrom(snap *configdata.Snapshot) (WorldCfg, bool) {",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated code missing %q\n----\n%s", want, source)
		}
	}
}

// 全局单例配置只写在 globals 段；退役的 objects 别名是未知键，按 meta 拼写守卫拒绝。
func TestCfggenRejectsTheRetiredObjectsAlias(t *testing.T) {
	_, err := runCfggen(t, "objects:\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n")
	if err == nil || !strings.Contains(err.Error(), "objects") {
		t.Fatalf("objects alias = %v, want it refused as an unknown key", err)
	}
}

// 每个坏 meta 都按它点名的原因被拒绝：只要求“有错误”时，一条检查被删掉会被另一条检查顶替（缺 key 会被
// “key 未声明”拒绝，反之亦然），用例照样绿。悬空 ref、重复表名的原因由 TestCfggenRejectsEachFieldRuleForTheStatedReason 断言。
func TestCfggenRejectsBrokenMetaForTheStatedReason(t *testing.T) {
	for name, tc := range map[string]struct{ meta, want string }{
		"missing key":       {"tables:\n  - name: t\n    fields:\n      - { name: id, type: int32 }\n", "key is required"},
		"key not declared":  {"tables:\n  - name: t\n    key: nope\n    fields:\n      - { name: id, type: int32 }\n", "key field \"nope\" not declared"},
		"float key":         {"tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: float64 }\n", "not usable as key"},
		"unknown type":      {"tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: decimal }\n", `unknown type "decimal"`},
		"ref type mismatch": {"tables:\n  - name: a\n    key: id\n    fields:\n      - { name: id, type: int64 }\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, ref: a }\n", "ref type int32 does not match a key type int64"},
		"index on slice":    {"tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: \"[]int32\", index: true }\n", "index requires a string/integer/bool field"},
		"meta typo":         {"tabels:\n  - name: t\n", "tabels"},
		"global with key":   {"globals:\n  - name: o\n    key: id\n    fields:\n      - { name: id, type: int32 }\n", "singleton configs have no key"},
		"bean keyword":      {"beans:\n  - name: type\n    fields:\n      - { name: id, type: int32 }\n", "is a Go keyword"},
		"bean predeclared":  {"beans:\n  - name: string\n    fields:\n      - { name: id, type: int32 }\n", "shadows a predeclared Go identifier"},
	} {
		_, err := runCfggen(t, tc.meta)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}

func TestCfggenIndexFalseDisablesIndex(t *testing.T) {
	source, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: camp, type: int32, index: false }\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, "cfg:\"index\"") || strings.Contains(source, "TByCamp") {
		t.Fatalf("index: false generated an index anyway:\n%s", source)
	}
}

func TestCfggenKeywordFieldNamesGenerateCompilableParams(t *testing.T) {
	source, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: type, type: int32, index: true }\n      - { name: table, type: string, index: true }\n      - { name: snap, type: int32, index: true }\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"func TByType(snap *configdata.Snapshot, typeArg int32) []TCfg",
		"func TByTable(snap *configdata.Snapshot, tableArg string) []TCfg",
		"func TBySnap(snap *configdata.Snapshot, snapArg int32) []TCfg",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("missing %q in:\n%s", want, source)
		}
	}
}

func TestCfggenSanitizesMultilineComments(t *testing.T) {
	source, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    comment: \"line1\\nfunc init() { panic(1) }\"\n    fields:\n      - { name: id, type: int32, comment: \"a\\nHack int64\" }\n")
	if err != nil {
		t.Fatal(err)
	}
	// The injected lines must be folded into single-line comments, never
	// land at column 0 / as a struct field line.
	if strings.Contains(source, "\nfunc init()") || strings.Contains(source, "\n\tHack") {
		t.Fatalf("comment injection survived:\n%s", source)
	}
	if !strings.Contains(source, "// TCfg is line1 func init() { panic(1) }.") {
		t.Fatalf("table comment not folded onto one line:\n%s", source)
	}
}

func TestCfggenRejectsIdentifierCollisionsAndBadNames(t *testing.T) {
	for name, meta := range map[string]string{
		"pascal collision tables": "tables:\n  - name: my_table\n    key: id\n    fields:\n      - { name: id, type: int32 }\n  - name: myTable\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
		"field pascal collision":  "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: a_b, type: int32 }\n      - { name: aB, type: string }\n",
		"table vs global derived": "tables:\n  - name: item\n    key: id\n    fields:\n      - { name: id, type: int32 }\nglobals:\n  - name: item_table\n    fields:\n      - { name: n, type: int32 }\n",
		"bean vs table type":      "beans:\n  - name: MonsterCfg\n    fields:\n      - { name: x, type: int32 }\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
		"bean vs fixed function":  "beans:\n  - name: RegisterGeneratedConfigData\n    fields:\n      - { name: x, type: int32 }\n",
		"bean shadows builtin":    "beans:\n  - name: string\n    fields:\n      - { name: x, type: int32 }\n",
		"dash in name":            "tables:\n  - name: my-table\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
		"non-ascii field":         "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: 名字, type: string }\n",
		"bean dup field":          "beans:\n  - name: B\n    fields:\n      - { name: x, type: int32 }\n      - { name: x, type: string }\n",
		"bean recursion":          "beans:\n  - name: Node\n    fields:\n      - { name: child, type: Node }\n",
		"bean mutual recursion":   "beans:\n  - name: A\n    fields:\n      - { name: b, type: B }\n  - name: B\n    fields:\n      - { name: a, type: A }\n",
		"global with index":       "globals:\n  - name: g\n    fields:\n      - { name: x, type: int32, index: true }\n",
		"global with ref":         "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\nglobals:\n  - name: g\n    fields:\n      - { name: x, type: int32, ref: t }\n",
		"index weird type":        "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, index: 123 }\n",
		"index bad charset":       "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, index: \"=camp\" }\n",
		"file escapes dir":        "tables:\n  - name: t\n    key: id\n    file: ../../x.json\n    fields:\n      - { name: id, type: int32 }\n",
	} {
		if _, err := runCfggen(t, meta); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestCfggenKeywordTableNameIsFine(t *testing.T) {
	// Table/field names are always PascalCased into identifiers, so a
	// keyword table name generates RangeCfg/RangeTableFrom and compiles.
	source, err := runCfggen(t, "tables:\n  - name: range\n    key: id\n    fields:\n      - { name: id, type: int32 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "type RangeCfg struct") {
		t.Fatalf("keyword table name mishandled:\n%s", source)
	}
}

func TestCfggenBeanSliceRecursionIsAllowed(t *testing.T) {
	source, err := runCfggen(t, "beans:\n  - name: Node\n    fields:\n      - { name: children, type: \"[]Node\" }\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "Children []Node") {
		t.Fatalf("slice recursion mishandled:\n%s", source)
	}
}

func TestCfggenRequiredAndSkipEmptyOptions(t *testing.T) {
	source, err := runCfggen(t, "tables:\n  - name: drop\n    key: id\n    fields:\n      - { name: id, type: int32 }\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: drop_id, type: int32, ref: drop, required: true }\n      - { name: scene, type: int32, index: true, skipempty: true }\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cfg:\"ref=drop,required\"",
		"cfg:\"index,skipempty\"",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("missing %q in:\n%s", want, source)
		}
	}
	// B10: required stands on its own (presence), and unique / min / enum
	// become cfg directives for configdata's shared rule check.
	rules, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, required: true, unique: true, min: 1 }\n      - { name: kind, type: string, enum: [melee, ranged] }\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`cfg:"required,unique,min=1"`, `cfg:"enum=melee|ranged"`} {
		if !strings.Contains(rules, want) {
			t.Fatalf("missing %q in:\n%s", want, rules)
		}
	}
	for label, field := range map[string]string{
		"min on a string":   "{ name: x, type: string, min: 1 }",
		"min not a number":  "{ name: x, type: int32, min: one }",
		"enum on a float":   "{ name: x, type: float64, enum: [a] }",
		"enum with a comma": `{ name: x, type: string, enum: ["a,b"] }`,
		"unique on a slice": `{ name: x, type: "[]int32", unique: true }`,
	} {
		if _, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - "+field+"\n"); err == nil {
			t.Errorf("%s accepted", label)
		}
	}
	// Prerequisites enforced.
	if _, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, skipempty: true }\n"); err == nil {
		t.Fatal("skipempty without index accepted")
	}
}

// validateMeta 的逐条字段规则（bean 与 table / global 字段）都按点名的原因拒绝；夹具满足其他所有规则，
// 被测规则是唯一的拒绝者。
func TestCfggenRejectsEachFieldRuleForTheStatedReason(t *testing.T) {
	cases := map[string][2]string{
		"bean duplicate field": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: item, type: int32 }\n      - { name: item, type: int32 }\n",
			"bean Drop: duplicate field item",
		},
		"bean fields collide on Go field": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: item_id, type: int32 }\n      - { name: itemID, type: int32 }\n",
			"bean Drop: fields item_id and itemID map to the same Go field ItemID",
		},
		"bean field with ref": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: monster, type: int32, ref: monster }\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
			"bean Drop field monster: ref/index only apply to table fields",
		},
		"bean field with index": {
			"beans:\n  - name: Drop\n    fields:\n      - { name: weight, type: int32, index: true }\n",
			"bean Drop field weight: ref/index only apply to table fields",
		},
		"ref target not a table": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: drop_id, type: int32, ref: drop }\n",
			`table monster field drop_id: ref target "drop" is not a declared table`,
		},
		"duplicate table name": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
			"duplicate table name monster",
		},
		"duplicate global name": {
			"globals:\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n",
			"duplicate global name world",
		},
		"table without fields": {
			"tables:\n  - name: monster\n    key: id\n",
			"table monster: no fields",
		},
		"global without fields": {
			"globals:\n  - name: world\n",
			"global world: no fields",
		},
		"table duplicate field": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: id, type: int32 }\n",
			"table monster: duplicate field id",
		},
		"table fields collide on Go field": {
			"tables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: scene_id, type: int32 }\n      - { name: sceneID, type: int32 }\n",
			"table monster: fields scene_id and sceneID map to the same Go field SceneID",
		},
		"table file escapes data dir": {
			"tables:\n  - name: monster\n    key: id\n    file: ../monster.json\n    fields:\n      - { name: id, type: int32 }\n",
			`table monster: file "../monster.json" escapes the data directory`,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runCfggen(t, "package: cfg\n"+testCase[0])
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), testCase[1]) {
				t.Fatalf("error %q does not contain %q", err, testCase[1])
			}
		})
	}
}

// 生成函数 / import 与 bean 共用 Go 命名空间：冲突必须在写输出前报错，不能把 gofmt 的成功当编译成功。
func TestCfggenRejectsReservedBeanNamesBeforeWriting(t *testing.T) {
	for _, name := range []string{"RegisterConfigData", "RegisterGeneratedConfigData", "MustRegisterGeneratedConfigData", "configdata", "strconv"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			meta := "package: cfg\nbeans:\n  - name: " + name + "\n    fields:\n      - { name: value, type: int32 }\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: camp, type: int32, index: true }\n"
			metaPath := filepath.Join(root, "schema.yaml")
			if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(root, "cfg")
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(out, generatedFileName)
			before := []byte("existing output must survive validation failure\n")
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := Run([]string{"-meta", metaPath, "-out", out}, &stdout)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "collides") {
				t.Fatalf("reserved bean %s was not rejected with a collision diagnostic: %v", name, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("invalid schema replaced output: %v\n%s", err, after)
			}
		})
	}
}

func TestCfggenNonCollidingBeanNamesCompile(t *testing.T) {
	for _, name := range []string{"Configdata", "RegisterConfigDataBean", "strconv"} {
		t.Run(name, func(t *testing.T) {
			source, err := runCfggen(t, "package: cfg\nbeans:\n  - name: "+name+"\n    fields:\n      - { name: value, type: int32 }\n")
			if err != nil {
				t.Fatal(err)
			}
			assertGeneratedConsumerCompiles(t, source)
		})
	}
}

// 显式关闭索引也必须生成可编译的消费包（不留未使用的 strconv import）。
func TestCfggenDisabledIndexesCompile(t *testing.T) {
	var disabled strings.Builder
	for i, typ := range []string{"int32", "int64", "uint32", "uint64", "float32", "float64", "bool", "string"} {
		fmt.Fprintf(&disabled, "      - { name: value_%d, type: %s, index: false }\n", i, typ)
	}
	for name, fields := range map[string]string{
		"all disabled":                    disabled.String(),
		"no indexes":                      "      - { name: camp, type: int32 }\n",
		"string enabled numeric disabled": "      - { name: camp, type: int32, index: false }\n      - { name: name, type: string, index: true }\n",
		"mixed enabled disabled":          "      - { name: camp, type: int32, index: false }\n      - { name: flag, type: bool, index: true }\n      - { name: owner, type: uint64, index: owner_id }\n",
	} {
		t.Run(name, func(t *testing.T) {
			source, err := runCfggen(t, "package: cfg\ntables:\n  - name: monster\n    key: id\n    fields:\n      - { name: id, type: int32 }\n"+fields)
			if err != nil {
				t.Fatal(err)
			}
			assertGeneratedConsumerCompiles(t, source)
		})
	}
}

// Compile against the real configdata package, not a generated-text assertion
// or a fake type importer. Module/cache/proxy configuration is inherited.
func assertGeneratedConsumerCompiles(t *testing.T, source string) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mod := "module example.com/cfggenconsumer\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.18.0\n\nreplace github.com/tjbdwanghaibo/roost-core => " + strconv.Quote(filepath.ToSlash(repo)) + "\n"
	for name, body := range map[string]string{"go.mod": mod, "cfg_gen.go": source} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "test", "-mod=mod", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated consumer does not compile: %v\n%s", err, out)
	}
}
