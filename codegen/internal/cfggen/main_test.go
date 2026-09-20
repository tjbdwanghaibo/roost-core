package cfggen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCfggenAcceptsObjectsAliasForGlobals(t *testing.T) {
	source, err := runCfggen(t, "objects:\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "func WorldFrom(snap *configdata.Snapshot) (WorldCfg, bool) {") {
		t.Fatalf("alias objects not honored:\n%s", source)
	}
}

func TestCfggenRejectsBrokenMeta(t *testing.T) {
	for name, meta := range map[string]string{
		"missing key":       "tables:\n  - name: t\n    fields:\n      - { name: id, type: int32 }\n",
		"key not declared":  "tables:\n  - name: t\n    key: nope\n    fields:\n      - { name: id, type: int32 }\n",
		"float key":         "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: float64 }\n",
		"unknown type":      "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: decimal }\n",
		"dangling ref":      "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, ref: nope }\n",
		"ref type mismatch": "tables:\n  - name: a\n    key: id\n    fields:\n      - { name: id, type: int64 }\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, ref: a }\n",
		"duplicate table":   "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
		"index on slice":    "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: \"[]int32\", index: true }\n",
		"meta typo":         "tabels:\n  - name: t\n",
		"global with key":   "globals:\n  - name: o\n    key: id\n    fields:\n      - { name: id, type: int32 }\n",
	} {
		if _, err := runCfggen(t, meta); err == nil {
			t.Fatalf("%s: accepted", name)
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
	// Prerequisites enforced.
	if _, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, required: true }\n"); err == nil {
		t.Fatal("required without ref accepted")
	}
	if _, err := runCfggen(t, "tables:\n  - name: t\n    key: id\n    fields:\n      - { name: id, type: int32 }\n      - { name: x, type: int32, skipempty: true }\n"); err == nil {
		t.Fatal("skipempty without index accepted")
	}
}
