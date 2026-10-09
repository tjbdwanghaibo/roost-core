package configdata

// B10（维护者决定，2026-10-06）：配置规则统一由运行时加载层强制。旧实现里 configdata
// 只拿到类型化的行，分不清缺列与零值，required 在 Load / Reload 时无从检查
// （RR-20261005-NC-75 留下的待决定项；N07 H2e：删掉 spawn 的 template 后 reload 被接受）。
// 承诺：TableDef / ObjectDef 声明的 Rules 在每次加载与热更时对原始 JSON 检查，ref 在
// 全部表加载后检查；违反即整次拒绝、旧快照保持，错误点名表 / 行 / 字段 / 规则；
// 声明本身错（字段不存在、min 用在字符串上……）在注册时就失败。
// C2：每次 Load / Reload / Rollback 恰好报告一次结果，含失败阶段与是否已发布后撤回。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

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
