// 配置数据仓库：加载 / 热更 / DryRun / 回滚与快照哈希、指纹、键大小写、声明规则在加载层强制。

package configdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type testMonsterCfg struct {
	ID      int32  `json:"id"`
	Name    string `json:"name"`
	SceneID int32  `json:"scene_id"`
}

type testWorldCfg struct {
	Width int32 `json:"width"`
}

func TestStoreLoadReloadAndActiveSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	writeFile(t, filepath.Join(dir, "world.json"), `{"width":100}`)

	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
		Indexes: []IndexDef[testMonsterCfg]{
			{Name: "scene_id", Key: func(v testMonsterCfg) string { return "scene:" + string(rune(v.SceneID)) }},
		},
		Validate: func(ctx *BuildContext, v testMonsterCfg) error {
			// No t.Fatal here: it Goexits past the build's recover and would
			// leave the store half-published (documented callback contract).
			if _, ok := ObjectFrom[testWorldCfg](ctx.Snapshot, "world"); !ok {
				return errors.New("object should be visible during table validation")
			}
			return nil
		},
	})
	MustRegisterObject(reg, ObjectDef[testWorldCfg]{
		Name: "world",
		File: "world.json",
	})
	MustRegisterCustom(reg, CustomDef[int]{
		Name: "monster_count",
		Build: func(ctx *BuildContext) (int, error) {
			table := MustTableFrom[int32, testMonsterCfg](ctx.Snapshot, "monster")
			return table.Len(), nil
		},
	})

	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if snap1.Version != 1 {
		t.Fatalf("version = %d, want 1", snap1.Version)
	}
	table := MustTableFrom[int32, testMonsterCfg](snap1, "monster")
	row, ok := table.Get(1)
	if !ok || row.Name != "wolf" {
		t.Fatalf("monster row = %+v ok=%v", row, ok)
	}
	if count := MustCustomFrom[int](snap1, "monster_count"); count != 1 {
		t.Fatalf("custom count = %d", count)
	}

	_, release := fctx.NewContext()
	defer release()
	if got := ActiveSnapshot(); got != snap1 {
		t.Fatalf("request snapshot should be snap1")
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"bear","scene_id":7}]`)
	snap2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snap2 == snap1 || snap2.Version != 2 {
		t.Fatalf("unexpected snap2 version")
	}
	if got := ActiveSnapshot(); got != snap1 {
		t.Fatalf("active request should keep request snapshot")
	}

	release()
	if got := ActiveSnapshot(); got != snap2 {
		t.Fatalf("without request context should read latest snapshot")
	}
}

func TestReloadFailureKeepsOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `{broken`)
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("reload should fail")
	}
	if got := store.Current(); got != snap1 {
		t.Fatal("failed reload should keep current snapshot")
	}
}

func TestDryRunBuildsSnapshotWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":2,"name":"bear"}]`)
	candidate, err := store.DryRun(context.Background(), "gray")
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if candidate.Version != 2 {
		t.Fatalf("candidate version = %d, want 2", candidate.Version)
	}
	if got := store.Current(); got != snap1 {
		t.Fatal("dry-run should not publish candidate snapshot")
	}
}

func TestRollbackRestoresPreviousPublishedSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":2,"name":"bear"}]`)
	if _, err := store.ReloadWithReason(context.Background(), "publish"); err != nil {
		t.Fatalf("reload: %v", err)
	}

	rolled, err := store.Rollback(context.Background(), "manual rollback")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolled != snap1 || store.Current() != snap1 {
		t.Fatal("rollback should restore previous snapshot")
	}
}

func TestReloadListenerRollbackKeepsOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rollbackCalled := false
	wantErr := errors.New("after failed")
	store.AddReloadListener(ReloadHook{
		HookName: "fail_after",
		AfterApply: func(context.Context, ReloadEvent) error {
			return wantErr
		},
		Rollback: func(_ context.Context, _ ReloadEvent, err error) {
			rollbackCalled = errors.Is(err, wantErr)
		},
	})
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":2,"name":"bear"}]`)
	if _, err := store.ReloadWithReason(context.Background(), "test"); !errors.Is(err, wantErr) {
		t.Fatalf("reload err = %v, want %v", err, wantErr)
	}
	if !rollbackCalled {
		t.Fatal("rollback hook not called")
	}
	if got := store.Current(); got != snap1 {
		t.Fatal("after-apply failed reload should restore previous snapshot")
	}
}

func TestAddReloadListenerUnregistersUncomparableHook(t *testing.T) {
	store := NewStore(NewRegistry(), t.TempDir())
	unregister := store.AddReloadListener(ReloadHook{
		HookName: "uncomparable",
		AfterApply: func(context.Context, ReloadEvent) error {
			return nil
		},
	})

	unregister()
	unregister()

	if got := len(store.reloadListeners()); got != 0 {
		t.Fatalf("listeners after unregister = %d, want 0", got)
	}
}

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSnapshotHashCoversTableRowContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster",
		File: "monster.json",
		Key:  func(v testMonsterCfg) int32 { return v.ID },
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Same table name, different row content: the hash must change, or the
	// hash is useless for config-consistency checks.
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"bear","scene_id":7}]`)
	snap2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snap1.Hash == snap2.Hash {
		t.Fatalf("hash unchanged across row edit: %s", snap1.Hash)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	snap3, err := store.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload back: %v", err)
	}
	if snap3.Hash != snap1.Hash {
		t.Fatalf("hash not deterministic: %s vs %s", snap3.Hash, snap1.Hash)
	}
}

func TestOpaqueCustomWithoutFingerprintFailsBuild(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterCustom(reg, CustomDef[*opaqueTables]{
		Name: "opaque",
		Build: func(*BuildContext) (*opaqueTables, error) {
			return &opaqueTables{rows: map[int32]string{1: "x"}}, nil
		},
	})
	if _, err := NewStore(reg, dir).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "serializes opaquely") {
		t.Fatalf("opaque custom silently hashed as {}: %v", err)
	}
}

func TestUnknownFingerprintNameFailsBuild(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterCustom(reg, CustomDef[int]{
		Name: "real",
		Build: func(ctx *BuildContext) (int, error) {
			ctx.Snapshot.SetFingerprint("tpyo", "abc") // misspelled member
			return 1, nil
		},
	})
	if _, err := NewStore(reg, dir).Load(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown member") {
		t.Fatalf("misspelled fingerprint silently ignored: %v", err)
	}
}

func TestSetFingerprintIsSealedAfterFinalize(t *testing.T) {
	store, _ := newCommitTestStore(t)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := snap.Hash
	snap.SetFingerprint("monster", "hijack") // must be a no-op on a published snapshot
	if snap.Hash != before {
		t.Fatal("published snapshot mutated")
	}
	if _, err := store.Reload(context.Background()); err != nil {
		t.Fatalf("reload after sealed SetFingerprint attempt: %v", err)
	}
}

func TestMustGetPanicsWithErrorsIsChain(t *testing.T) {
	table := &Table[int32, testMonsterCfg]{name: "monster", byKey: map[int32]int{}}
	defer func() {
		r := recover()
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrTableNotFound) {
			t.Fatalf("MustGet panic lost the error chain: %v", r)
		}
	}()
	table.MustGet(1)
}

func TestExternalFingerprintIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.json"), `{"a":1}`)
	writeFile(t, filepath.Join(dir, "b.json"), `{"b":2}`)
	loadHash := func(order []string, parallel bool) string {
		reg := NewRegistry()
		MustRegisterExternalTables(reg, "ext", func(read func(string) ([]byte, error)) (int, error) {
			if parallel {
				var wg sync.WaitGroup
				for _, f := range order {
					wg.Add(1)
					go func(name string) { defer wg.Done(); _, _ = read(name) }(f)
				}
				wg.Wait()
				return 1, nil
			}
			for _, f := range order {
				if _, err := read(f); err != nil {
					return 0, err
				}
			}
			return 1, nil
		})
		snap, err := NewStore(reg, dir).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return snap.Hash
	}
	forward := loadHash([]string{"a.json", "b.json"}, false)
	backward := loadHash([]string{"b.json", "a.json"}, false)
	concurrent := loadHash([]string{"a.json", "b.json"}, true)
	if forward != backward || forward != concurrent {
		t.Fatalf("fingerprint order-dependent: %s / %s / %s", forward, backward, concurrent)
	}
}

// configdata 键大小写敏感：数据文件里的键必须与声明字段的 json 名逐字一致。只差大小写的键、一行里同一字段的几种大小写拼写，在
// Load / Reload / DryRun 一律拒绝，错误是 *RuleError（Rule "case"），点名表、行、行主键、应有的拼写与原文的键；嵌套对象同样逐字核对。
// 未声明的键维持原行为（宽松忽略，严格模式拒绝）。

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

// 配置规则统一由运行时加载层强制：TableDef / ObjectDef 声明的 Rules 在每次加载与热更时对原始 JSON 检查，ref 在全部表加载后检查；
// 违反即整次拒绝、旧快照保持，错误点名表 / 行 / 字段 / 规则；声明本身错（字段不存在、min 用在字符串上……）在注册时就失败。
// 每次 Load / Reload / Rollback 恰好报告一次结果，含失败阶段与是否已发布后撤回。

type ruleSpawnCfg struct {
	ID       int32  `json:"id"`
	Template int32  `json:"template"`
	Count    int32  `json:"count"`
	Kind     string `json:"kind"`
	Drop     *int32 `json:"drop"`
}

type ruleWorldCfg struct {
	Width  int32 `json:"width"`
	Height int32 `json:"height"`
}

func ruleRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, autoDropCfg]{
		Name: "drop", File: "drop.json", Key: func(v autoDropCfg) int32 { return v.ID },
	})
	MustRegisterTable(reg, TableDef[int32, ruleSpawnCfg]{
		Name: "spawn", File: "spawn.json", Key: func(v ruleSpawnCfg) int32 { return v.ID },
		Rules: []FieldRule{
			{Field: "id", Required: true, Unique: true},
			{Field: "template", Required: true},
			{Field: "count", Required: true, Min: "1"},
			{Field: "kind", Enum: []string{"melee", "ranged"}},
			{Field: "drop", Ref: "drop"},
		},
	})
	MustRegisterObject(reg, ObjectDef[ruleWorldCfg]{
		Name: "world", File: "world.json",
		Rules: []FieldRule{{Field: "width", Required: true, Min: "1"}},
	})
	return reg
}

func TestDeclaredRulesRejectReloadAndNameTheViolation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100}]`)
	writeFile(t, filepath.Join(dir, "world.json"), `{"width":10}`)
	const good = `[{"id":1,"template":9001,"count":3,"kind":"melee","drop":100},{"id":2,"template":0,"count":1}]`
	writeFile(t, filepath.Join(dir, "spawn.json"), good)
	store := NewStore(ruleRegistry(t), dir)
	live, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("valid data rejected (template 0 present is a value, not a missing column): %v", err)
	}
	cases := []struct {
		file, body              string
		table, field, rule, key string
		row                     int
	}{
		{"spawn.json", `[{"id":1,"count":3}]`, "spawn", "template", "required", "1", 1},
		{"spawn.json", `[{"id":1,"template":5,"count":3},{"id":2,"template":null,"count":1}]`, "spawn", "template", "required", "2", 2},
		{"spawn.json", `[{"id":1,"template":5,"count":0}]`, "spawn", "count", "min", "1", 1},
		{"spawn.json", `[{"id":1,"template":5,"count":1,"kind":"magic"}]`, "spawn", "kind", "enum", "1", 1},
		{"spawn.json", `[{"id":1,"template":5,"count":1,"drop":7}]`, "spawn", "drop", "ref", "1", 1},
		{"world.json", `{"height":3}`, "world", "width", "required", "", 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.field+"/"+testCase.rule, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, testCase.file), testCase.body)
			defer writeFile(t, filepath.Join(dir, "spawn.json"), good)
			defer writeFile(t, filepath.Join(dir, "world.json"), `{"width":10}`)
			_, err := store.Reload(context.Background())
			var ruleErr *RuleError
			if !errors.As(err, &ruleErr) {
				t.Fatalf("reload err = %v, want a *RuleError", err)
			}
			if ruleErr.Table != testCase.table || ruleErr.Field != testCase.field || ruleErr.Rule != testCase.rule || ruleErr.Row != testCase.row || ruleErr.Key != testCase.key {
				t.Fatalf("got %+v, want table=%s row=%d key=%s field=%s rule=%s", *ruleErr, testCase.table, testCase.row, testCase.key, testCase.field, testCase.rule)
			}
			if store.Current() != live {
				t.Fatal("a rejected reload moved the live snapshot")
			}
		})
	}
}

func TestRuleDeclarationsAreCheckedAtRegistration(t *testing.T) {
	key := func(v ruleSpawnCfg) int32 { return v.ID }
	for label, declared := range map[string][]FieldRule{
		"unknown field":       {{Field: "tempalte", Required: true}},
		"min on a string":     {{Field: "kind", Min: "1"}},
		"min not a number":    {{Field: "count", Min: "x"}},
		"two rules one field": {{Field: "count", Required: true}, {Field: "count", Min: "1"}},
	} {
		if err := RegisterTable(NewRegistry(), TableDef[int32, ruleSpawnCfg]{Name: "spawn", File: "spawn.json", Key: key, Rules: declared}); err == nil {
			t.Errorf("%s: registered", label)
		}
	}
	if err := RegisterObject(NewRegistry(), ObjectDef[ruleWorldCfg]{Name: "world", File: "world.json", Rules: []FieldRule{{Field: "width", Unique: true}}}); err == nil {
		t.Error("unique on an object: registered")
	}
	type boolRef struct {
		ID   int32 `json:"id"`
		Flag bool  `json:"flag"`
	}
	if err := RegisterTable(NewRegistry(), TableDef[int32, boolRef]{Name: "b", File: "b.json", Key: func(v boolRef) int32 { return v.ID }, Rules: []FieldRule{{Field: "flag", Ref: "drop"}}}); err == nil {
		t.Error("ref on a bool: registered")
	}
}

// The cfg tag directives become the same rules (auto tables, cfggen output).
func TestAutoTableTagRulesUseTheSharedCheck(t *testing.T) {
	type tagged struct {
		ID    int32  `json:"id" cfg:"key"`
		Code  string `json:"code" cfg:"required,unique"`
		Level int32  `json:"level" cfg:"min=1"`
		Kind  string `json:"kind" cfg:"enum=melee|ranged"`
	}
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterAutoTable[int32, tagged](reg, WithAutoName("tagged"))
	store := NewStore(reg, dir)
	for body, want := range map[string]string{
		`[{"id":1,"level":1}]`: "field code: required",
		`[{"id":1,"code":"a","level":1},{"id":2,"code":"a","level":1}]`: "field code: unique",
		`[{"id":1,"code":"a","level":0}]`:                               "field level: min",
		`[{"id":1,"code":"a","level":1,"kind":"x"}]`:                    "field kind: enum",
	} {
		writeFile(t, filepath.Join(dir, "tagged.json"), body)
		if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", body, err, want)
		}
	}
	writeFile(t, filepath.Join(dir, "tagged.json"), `[{"id":1,"code":"a","level":2,"kind":"ranged"}]`)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEveryReloadReportsOneOutcome(t *testing.T) {
	store, dir := newCommitTestStore(t)
	var outcomes []ReloadOutcome
	store.OnReloadOutcome(func(outcome ReloadOutcome) { outcomes = append(outcomes, outcome) })
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `null`)
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("null document accepted")
	}
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":2,"name":"orc","scene_id":7}]`)
	remove := store.AddReloadListener(ReloadHook{HookName: "late", AfterApply: func(context.Context, ReloadEvent) error { return errFail }})
	if _, err := store.Reload(context.Background()); err == nil {
		t.Fatal("failing AfterApply accepted")
	}
	remove()
	if _, err := store.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rollback(context.Background(), "operator"); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 5 {
		t.Fatalf("outcomes = %d, want one per attempt (5): %+v", len(outcomes), outcomes)
	}
	load, built, reverted, ok, rolled := outcomes[0], outcomes[1], outcomes[2], outcomes[3], outcomes[4]
	if load.Err != nil || load.Live != load.Version {
		t.Errorf("load = %+v", load)
	}
	if built.Stage != ReloadStageBuild || built.Reverted() || built.Live != load.Version {
		t.Errorf("build failure = %+v", built)
	}
	if reverted.Stage != ReloadStageApply || !reverted.Reverted() || reverted.Live != load.Version {
		t.Errorf("reverted = %+v", reverted)
	}
	if ok.Err != nil || ok.Live != ok.Version || ok.Version <= reverted.Version {
		t.Errorf("ok = %+v (versions are burned by failures, never reused)", ok)
	}
	if !rolled.Rollback || rolled.Err != nil || rolled.Live != load.Version || rolled.Version != load.Version {
		t.Errorf("rollback = %+v", rolled)
	}
}

// 重载要说出哪些表变了：内容摘要与 Snapshot.Hash 同源，按成员给出新增 / 删除 / 变化与表的行数；内容完全
// 没变时 Changes.None()，但仍发布新版本号（监听者依赖每次重载都跑，见 ReloadWithChanges）。
func TestReloadReportsWhichTablesChanged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	writeFile(t, filepath.Join(dir, "world.json"), `{"width":100}`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{Name: "monster", File: "monster.json", Key: func(v testMonsterCfg) int32 { return v.ID }})
	MustRegisterObject(reg, ObjectDef[testWorldCfg]{Name: "world", File: "world.json"})
	store := NewStore(reg, dir)
	var outcomes []ReloadOutcome
	store.OnReloadOutcome(func(outcome ReloadOutcome) { outcomes = append(outcomes, outcome) })
	ctx := context.Background()

	first, err := store.ReloadWithChanges(ctx, "load")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changes.Initial || first.Changes.None() || len(first.Changes.Added) != 2 || first.Previous != 0 {
		t.Fatalf("first load changes = %+v previous=%d, want both members added and no previous generation", first.Changes, first.Previous)
	}

	same, err := store.ReloadWithChanges(ctx, "nothing edited")
	if err != nil {
		t.Fatal(err)
	}
	if !same.Changes.None() || same.Changes.Unchanged != 2 {
		t.Fatalf("a reload of unchanged files reported %+v, want no change", same.Changes)
	}
	if same.Snapshot.Version != 2 || same.Previous != 1 || same.Snapshot.Hash != first.Snapshot.Hash || store.Current() != same.Snapshot {
		t.Fatalf("an unchanged reload must still publish a new version with the same hash: version=%d previous=%d hash equal=%v", same.Snapshot.Version, same.Previous, same.Snapshot.Hash == first.Snapshot.Hash)
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7},{"id":2,"name":"bear","scene_id":7}]`)
	grown, err := store.ReloadWithChanges(ctx, "one row added")
	if err != nil {
		t.Fatal(err)
	}
	want := []MemberChange{{Kind: "table", Name: "monster", RowsBefore: 1, RowsAfter: 2}}
	if fmt.Sprint(grown.Changes.Changed) != fmt.Sprint(want) || grown.Changes.Unchanged != 1 || grown.Changes.None() {
		t.Fatalf("adding a monster row reported %+v, want only monster changed 1 -> 2 rows", grown.Changes)
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7},{"id":2,"name":"boar","scene_id":7}]`)
	edited, err := store.ReloadWithChanges(ctx, "one field edited")
	if err != nil {
		t.Fatal(err)
	}
	if want := []MemberChange{{Kind: "table", Name: "monster", RowsBefore: 2, RowsAfter: 2}}; fmt.Sprint(edited.Changes.Changed) != fmt.Sprint(want) {
		t.Fatalf("editing a field reported %+v, want monster changed with the row count unchanged", edited.Changes)
	}

	back, err := store.RollbackWithChanges(ctx, "undo")
	if err != nil {
		t.Fatal(err)
	}
	if back.Snapshot != grown.Snapshot || len(back.Changes.Changed) != 1 || back.Changes.Changed[0].Name != "monster" {
		t.Fatalf("rollback = version %d changes %+v, want the previous generation with monster changed back", back.Snapshot.Version, back.Changes)
	}

	if len(outcomes) != 5 || !outcomes[1].Changes.None() || outcomes[2].Changes.None() {
		t.Fatalf("outcome observers saw %d outcomes (unchanged flags wrong?): %+v", len(outcomes), outcomes)
	}
}
