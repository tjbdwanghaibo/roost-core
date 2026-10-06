package configdata

// configdata 大小写敏感（维护者 2026-10-06：“configdata 需要大小写敏感”，
// docs/feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md）。
// 承诺：数据文件里的键必须与声明字段的 json 名逐字一致。只差大小写的键（"Level"
// 对字段 level）、一行里同一字段的几种大小写拼写，在 Load / Reload / DryRun 一律拒绝，
// 错误是 *RuleError（Rule "case"），点名表、行、行主键、应有的拼写与原文的键；嵌套
// 对象同样逐字核对。未声明的键维持原行为（宽松忽略，严格模式拒绝）。
// 旧行为：encoding/json 大小写不敏感匹配，{"Level":1} 静默当作 level 加载，
// {"level":1,"Level":2} 取文档顺序最后一个；规则声明 Field "Level" 也能绑定到 level。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type caseRewardCfg struct {
	ItemID int32 `json:"item_id"`
}

type caseBaseCfg struct {
	Tier int32 `json:"tier"`
}

type caseMonsterCfg struct {
	caseBaseCfg
	ID      int32                    `json:"id"`
	Level   int32                    `json:"level"`
	Rewards []caseRewardCfg          `json:"rewards"`
	Extra   map[string]caseRewardCfg `json:"extra"`
	Raw     json.RawMessage          `json:"raw"`
}

type caseWorldCfg struct {
	Width int32 `json:"width"`
}

func caseRegistry() *Registry {
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, caseMonsterCfg]{
		Name: "monster", File: "monster.json", Key: func(v caseMonsterCfg) int32 { return v.ID },
	})
	MustRegisterObject(reg, ObjectDef[caseWorldCfg]{Name: "world", File: "world.json"})
	return reg
}

const (
	// map 的键由作者决定（"Gold"），json.RawMessage 里的内容不核对（"Any"）。
	caseGoodMonster = `[{"id":1,"tier":2,"level":3,"rewards":[{"item_id":5}],"extra":{"Gold":{"item_id":1}},"raw":{"Any":1}}]`
	caseGoodWorld   = `{"width":10}`
)

var caseRejected = []struct {
	label, file, body       string
	table, field, key, want string
	row                     int
}{
	{"variant only", "monster.json", `[{"id":1,"Level":1}]`, "monster", "level", "1", `"Level"`, 1},
	{"exact then variant", "monster.json", `[{"id":1,"level":1,"Level":2}]`, "monster", "level", "1", `"Level"`, 1},
	{"variant then exact", "monster.json", `[{"id":1,"Level":2,"level":1}]`, "monster", "level", "1", `"Level"`, 1},
	{"second row", "monster.json", `[{"id":1,"level":1},{"id":2,"LEVEL":1}]`, "monster", "level", "2", `"LEVEL"`, 2},
	{"nested", "monster.json", `[{"id":1,"level":1,"rewards":[{"item_id":1},{"Item_ID":5}]}]`, "monster", "rewards[1].item_id", "1", `"Item_ID"`, 1},
	{"promoted", "monster.json", `[{"id":1,"Tier":1}]`, "monster", "tier", "1", `"Tier"`, 1},
	{"map value", "monster.json", `[{"id":1,"extra":{"Gold":{"ITEM_ID":1}}}]`, "monster", `extra["Gold"].item_id`, "1", `"ITEM_ID"`, 1},
	{"object", "world.json", `{"Width":3}`, "world", "width", "", `"Width"`, 0},
	{"wrapped rows", "monster.json", `{"rows":[{"id":1,"Level":1}]}`, "monster", "level", "1", `"Level"`, 1},
}

func checkCaseError(t *testing.T, err error, table, field, key, want string, row int) {
	t.Helper()
	var ruleErr *RuleError
	if !errors.As(err, &ruleErr) {
		t.Fatalf("err = %v, want a *RuleError", err)
	}
	if ruleErr.Rule != "case" || ruleErr.Table != table || ruleErr.Field != field || ruleErr.Row != row || ruleErr.Key != key || !strings.Contains(ruleErr.Detail, want) {
		t.Fatalf("got %+v (%v), want rule=case table=%s row=%d key=%s field=%s naming %s", *ruleErr, err, table, row, key, field, want)
	}
}

func TestMisspelledKeysRejectLoad(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, testCase := range caseRejected {
			t.Run(testCase.label, func(t *testing.T) {
				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "monster.json"), caseGoodMonster)
				writeFile(t, filepath.Join(dir, "world.json"), caseGoodWorld)
				writeFile(t, filepath.Join(dir, testCase.file), testCase.body)
				store := NewStore(caseRegistry(), dir)
				store.SetStrictJSON(strict)
				_, err := store.Load(context.Background())
				checkCaseError(t, err, testCase.table, testCase.field, testCase.key, testCase.want, testCase.row)
				if store.Current() != nil {
					t.Fatal("a rejected load published a snapshot")
				}
			})
		}
	}
}

func TestMisspelledKeysRejectReloadAndDryRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), caseGoodMonster)
	writeFile(t, filepath.Join(dir, "world.json"), caseGoodWorld)
	store := NewStore(caseRegistry(), dir)
	live, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("exact keys rejected: %v", err)
	}
	for _, testCase := range caseRejected {
		t.Run(testCase.label, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, testCase.file), testCase.body)
			defer writeFile(t, filepath.Join(dir, "monster.json"), caseGoodMonster)
			defer writeFile(t, filepath.Join(dir, "world.json"), caseGoodWorld)
			_, err := store.Reload(context.Background())
			checkCaseError(t, err, testCase.table, testCase.field, testCase.key, testCase.want, testCase.row)
			if store.Current() != live {
				t.Fatal("a rejected reload moved the live snapshot")
			}
			_, err = store.DryRun(context.Background(), "case")
			checkCaseError(t, err, testCase.table, testCase.field, testCase.key, testCase.want, testCase.row)
		})
	}
	// 精确键照常热更。
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"level":4},{"id":2,"level":5,"rewards":[]}]`)
	next, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("exact keys rejected on reload: %v", err)
	}
	table, _ := TableFrom[int32, caseMonsterCfg](next, "monster")
	if row, ok := table.Get(2); !ok || row.Level != 5 {
		t.Fatalf("row 2 = %+v, %v", row, ok)
	}
}

// 未声明的键（与任何字段都不只差大小写）维持原行为：宽松模式忽略，严格模式拒绝。
func TestUndeclaredKeysKeepTheirBehaviour(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"level":3,"note":"x","Remark":1}]`)
	writeFile(t, filepath.Join(dir, "world.json"), caseGoodWorld)
	store := NewStore(caseRegistry(), dir)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("lenient load rejected an undeclared key: %v", err)
	}
	store.SetStrictJSON(true)
	_, err := store.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict reload err = %v, want encoding/json's unknown field", err)
	}
}

// 规则声明里的字段名同样逐字匹配：只差大小写的 Field 在注册时失败，并给出应有的拼写。
func TestRuleFieldMustMatchTheJSONNameExactly(t *testing.T) {
	err := RegisterTable(NewRegistry(), TableDef[int32, caseMonsterCfg]{
		Name: "monster", File: "monster.json", Key: func(v caseMonsterCfg) int32 { return v.ID },
		Rules: []FieldRule{{Field: "Level", Min: "1"}},
	})
	if err == nil || !strings.Contains(err.Error(), `"level"`) {
		t.Fatalf("register err = %v, want a refusal naming level", err)
	}
}
