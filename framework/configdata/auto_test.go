package configdata

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type autoDropCfg struct {
	ID    int32 `json:"id" cfg:"key"`
	Bonus int32 `json:"bonus"`
}

type autoMonsterCfg struct {
	ID      int32  `json:"id"       cfg:"key"`
	Name    string `json:"name"`
	SceneID int32  `json:"scene_id" cfg:"index"`
	DropID  int32  `json:"drop_id"  cfg:"ref=drop"`
}

func TestRegisterAutoTableDerivesMappingFromTags(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "auto_monster.json"),
		`[{"id":1,"name":"wolf","scene_id":7,"drop_id":100},{"id":2,"name":"bear","scene_id":7,"drop_id":0}]`)

	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop")) // ref=drop 的目标名
	MustRegisterAutoTable[int32, autoMonsterCfg](reg)                    // 名字与文件全推导

	store := NewStore(reg, dir)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Name inference: autoMonsterCfg -> auto_monster (test types carry the
	// auto prefix; MonsterCfg would infer "monster").
	table, ok := TableFrom[int32, autoMonsterCfg](snap, "auto_monster")
	if !ok {
		t.Fatal("inferred table name auto_monster missing")
	}
	row, ok := table.Get(1)
	if !ok || row.Name != "wolf" {
		t.Fatalf("row = %+v ok=%v", row, ok)
	}
	if rows := table.GetByIndex("scene_id", "7"); len(rows) != 2 {
		t.Fatalf("index rows = %+v", rows)
	}
}

func TestRegisterAutoTableRefValidation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7,"drop_id":999}]`)

	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, autoMonsterCfg](reg, WithAutoName("monster"), WithAutoFile("monster.json"))

	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "missing drop key 999") {
		t.Fatalf("dangling reference accepted: %v", err)
	}
	// Zero value means "no reference" and passes.
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7,"drop_id":0}]`)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("zero ref rejected: %v", err)
	}
}

func TestRegisterAutoTableUserValidateRunsAfterRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":-5}]`)
	reg := NewRegistry()
	if err := RegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"),
		WithAutoValidate(func(_ *BuildContext, v autoDropCfg) error {
			if v.Bonus < 0 {
				return errNegativeBonus
			}
			return nil
		})); err != nil {
		t.Fatal(err)
	}
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "negative bonus") {
		t.Fatalf("user validate skipped: %v", err)
	}
}

var errNegativeBonus = &validationError{"negative bonus"}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func TestRegisterAutoTableTagMistakesFailAtRegistration(t *testing.T) {
	type noKey struct {
		ID int32 `json:"id"`
	}
	type wrongKeyType struct {
		ID int64 `json:"id" cfg:"key"`
	}
	type twoKeys struct {
		A int32 `json:"a" cfg:"key"`
		B int32 `json:"b" cfg:"key"`
	}
	type badDirective struct {
		ID int32 `json:"id" cfg:"key,frobnicate"`
	}
	type sliceIndex struct {
		ID   int32   `json:"id" cfg:"key"`
		Tags []int32 `json:"tags" cfg:"index"`
	}
	reg := NewRegistry()
	if err := RegisterAutoTable[int32, noKey](reg); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := RegisterAutoTable[int32, wrongKeyType](reg); err == nil {
		t.Fatal("key type mismatch accepted")
	}
	if err := RegisterAutoTable[int32, twoKeys](reg); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if err := RegisterAutoTable[int32, badDirective](reg); err == nil {
		t.Fatal("unknown directive accepted")
	}
	if err := RegisterAutoTable[int32, sliceIndex](reg); err == nil {
		t.Fatal("slice index accepted")
	}
}

func TestInferTableName(t *testing.T) {
	for typeName, want := range map[string]string{
		"MonsterCfg":   "monster",
		"WorldConfig":  "world",
		"DropGroupCfg": "drop_group",
		"NPC":          "npc",
	} {
		if got := inferTableName(typeName); got != want {
			t.Fatalf("inferTableName(%s) = %s, want %s", typeName, got, want)
		}
	}
}

func TestRegisterExternalTablesRebuildsOnReload(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ext.json"), `{"v":1}`)
	type extTables struct{ V int }
	reg := NewRegistry()
	MustRegisterExternalTables(reg, "ext", func(read func(string) ([]byte, error)) (extTables, error) {
		raw, err := read("ext.json")
		if err != nil {
			return extTables{}, err
		}
		var doc struct {
			V int `json:"v"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return extTables{}, err
		}
		return extTables{V: doc.V}, nil
	})
	store := NewStore(reg, dir)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ExternalTablesFrom[extTables](snap, "ext")
	if !ok || got.V != 1 {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
	writeFile(t, filepath.Join(dir, "ext.json"), `{"v":2}`)
	snap2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ExternalTablesFrom[extTables](snap2, "ext"); got.V != 2 {
		t.Fatalf("reload aggregate = %+v", got)
	}
	// Old snapshot is untouched: in-flight requests keep their view.
	if got, _ := ExternalTablesFrom[extTables](snap, "ext"); got.V != 1 {
		t.Fatalf("old snapshot mutated: %+v", got)
	}
}

func TestRegisterExternalTablesRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterExternalTables(reg, "ext", func(read func(string) ([]byte, error)) (int, error) {
		if _, err := read("../secret.json"); err == nil {
			return 0, nil
		} else {
			return 0, err
		}
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "escapes the data directory") {
		t.Fatalf("path escape accepted: %v", err)
	}
}
