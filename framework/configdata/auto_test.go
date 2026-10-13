// 自动表：由标签推导映射、ref / index / required 校验、标签错误在注册时按原因拒绝、外部表与内容哈希。

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

type refBase struct {
	DropID int32 `json:"drop_id" cfg:"ref=drop"`
}

type embeddedMonsterCfg struct {
	refBase
	ID   int32  `json:"id" cfg:"key"`
	Name string `json:"name"`
}

func TestAutoTablePromotesEmbeddedFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","drop_id":999}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, embeddedMonsterCfg](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	store := NewStore(reg, dir)
	// The ref lives in the embedded base: it must still be enforced.
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "missing drop key 999") {
		t.Fatalf("embedded ref ignored: %v", err)
	}
}

func TestAutoTableRefTargetCheckedEvenWhenColumnIsZero(t *testing.T) {
	type typoRef struct {
		ID     int32 `json:"id" cfg:"key"`
		DropID int32 `json:"drop_id" cfg:"ref=drpo"` // misspelled target
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"drop_id":0}]`) // all zero
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, typoRef](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown table drpo") {
		t.Fatalf("misspelled ref target hidden by zero column: %v", err)
	}
}

func TestAutoTableRefTypeMismatchIsSchemaError(t *testing.T) {
	type wideRef struct {
		ID     int32 `json:"id" cfg:"key"`
		DropID int64 `json:"drop_id" cfg:"ref=drop"` // int64 vs int32 key: silent truncation risk
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"drop_id":100}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, wideRef](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "not compatible") {
		t.Fatalf("cross-kind ref accepted: %v", err)
	}
}

func TestAutoTableNamedAliasRefIsCompatible(t *testing.T) {
	type dropID int32
	type aliasRef struct {
		ID     int32  `json:"id" cfg:"key"`
		DropID dropID `json:"drop_id" cfg:"ref=drop"` // named alias of the key type: fine
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"drop_id":100}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, aliasRef](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("named alias ref rejected: %v", err)
	}
}

func TestAutoTableRegistrationRejectsBadRefAndIndexShapes(t *testing.T) {
	type sliceRef struct {
		ID  int32   `json:"id" cfg:"key"`
		IDs []int32 `json:"ids" cfg:"ref=drop"`
	}
	type boolRef struct {
		ID int32 `json:"id" cfg:"key"`
		B  bool  `json:"b" cfg:"ref=drop"`
	}
	type dupIndex struct {
		ID int32 `json:"id" cfg:"key"`
		A  int32 `json:"a" cfg:"index=x"`
		B  int32 `json:"b" cfg:"index=x"`
	}
	type emptyIndexName struct {
		ID int32 `json:"id" cfg:"key"`
		A  int32 `json:"a" cfg:"index="`
	}
	type orphanRequired struct {
		ID int32 `json:"id" cfg:"key"`
		A  int32 `json:"a" cfg:"required"`
	}
	reg := NewRegistry()
	if err := RegisterAutoTable[int32, sliceRef](reg); err == nil {
		t.Fatal("slice ref accepted")
	}
	if err := RegisterAutoTable[int32, boolRef](reg); err == nil {
		t.Fatal("bool ref accepted")
	}
	if err := RegisterAutoTable[int32, dupIndex](reg); err == nil {
		t.Fatal("duplicate index name accepted")
	}
	if err := RegisterAutoTable[int32, emptyIndexName](reg); err == nil {
		t.Fatal("empty index name accepted")
	}
	// B10: required stands on its own now (the JSON key must be present and
	// not null); it used to be accepted only next to ref.
	if err := RegisterAutoTable[int32, orphanRequired](reg); err != nil {
		t.Fatalf("required without ref rejected: %v", err)
	}
	if err := RegisterAutoTable[string, autoDropCfg](reg); err == nil {
		t.Fatal("key type mismatch accepted") // K=string vs ID int32
	}
}

func TestAutoTableRequiredRefRejectsZero(t *testing.T) {
	type strictRef struct {
		ID     int32 `json:"id" cfg:"key"`
		DropID int32 `json:"drop_id" cfg:"ref=drop,required"`
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	// The silent-rename scenario: the column decodes to zero.
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"dropId":100}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, strictRef](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	store := NewStore(reg, dir)
	// B10: the renamed column is now seen as missing on the raw row.
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "field drop_id: required: missing or null") {
		t.Fatalf("renamed required ref column accepted: %v", err)
	}
	// Present but zero: a required reference must still point at a key.
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"drop_id":0}]`)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "references missing drop key 0") {
		t.Fatalf("zeroed required ref accepted: %v", err)
	}
}

func TestAutoTableSkipEmptyIndex(t *testing.T) {
	type banded struct {
		ID    int32 `json:"id" cfg:"key"`
		Scene int32 `json:"scene" cfg:"index,skipempty"`
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "banded.json"), `[{"id":1,"scene":7},{"id":2,"scene":0},{"id":3,"scene":0}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, banded](reg, WithAutoName("banded"))
	store := NewStore(reg, dir)
	snap, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	table, _ := TableFrom[int32, banded](snap, "banded")
	if rows := table.GetByIndex("scene", "0"); len(rows) != 0 {
		t.Fatalf("zero values indexed despite skipempty: %+v", rows)
	}
	if rows := table.GetByIndex("scene", "7"); len(rows) != 1 {
		t.Fatalf("non-zero rows missing: %+v", rows)
	}
}

// opaqueTables mimics an externally generated aggregate whose state is
// entirely unexported — exactly the shape json.Marshal sees as "{}".
type opaqueTables struct {
	rows map[int32]string
}

func TestExternalTablesContentDriftIsVisibleInHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tbitem.json"), `[{"id":1,"name":"sword"}]`)
	reg := NewRegistry()
	MustRegisterExternalTables(reg, "luban", func(read func(string) ([]byte, error)) (*opaqueTables, error) {
		raw, err := read("tbitem.json")
		if err != nil {
			return nil, err
		}
		return &opaqueTables{rows: map[int32]string{1: string(raw)}}, nil
	})
	store := NewStore(reg, dir)
	snap1, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "tbitem.json"), `[{"id":1,"name":"SWORD OF DOOM"}]`)
	snap2, err := store.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap1.Hash == snap2.Hash {
		t.Fatalf("external content drift invisible in hash: %s", snap1.Hash)
	}
}

func TestExternalTablesBuildPanicBecomesError(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	MustRegisterExternalTables(reg, "luban", func(read func(string) ([]byte, error)) (int, error) {
		panic("generated loader exploded")
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "panic: generated loader exploded") {
		t.Fatalf("external build panic escaped: %v", err)
	}
}

func TestExternalTablesReadInvalidAfterBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.json"), `{}`)
	var leaked func(string) ([]byte, error)
	reg := NewRegistry()
	MustRegisterExternalTables(reg, "luban", func(read func(string) ([]byte, error)) (int, error) {
		if _, err := read("a.json"); err != nil { // one legit read: fingerprint satisfied
			return 0, err
		}
		leaked = read
		return 1, nil
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := leaked("a.json"); err == nil || !strings.Contains(err.Error(), "after build returned") {
		t.Fatalf("lazy read allowed after build: %v", err)
	}
}

func TestFinalizeMarshalErrorFailsBuild(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	type cyclic struct {
		Self *cyclic `json:"self"`
	}
	MustRegisterCustom(reg, CustomDef[*cyclic]{
		Name: "cycle",
		Build: func(*BuildContext) (*cyclic, error) {
			c := &cyclic{}
			c.Self = c
			return c, nil
		},
	})
	store := NewStore(reg, dir)
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "hash custom cycle") {
		t.Fatalf("marshal failure swallowed into constant hash: %v", err)
	}
}

// 两个同深度嵌入字段在 json 名上打平且带 cfg 标签时拒绝（否则 encoding/json 会悄悄丢掉它）；skipempty 必须与 index 同用。
func TestAutoTableRefusesTaggedTiesAndSkipEmptyWithoutIndex(t *testing.T) {
	type left struct {
		ID int32 `cfg:"key"`
	}
	type right struct {
		ID int32
	}
	type tied struct {
		left
		right
		Key int32 `json:"key" cfg:"key"`
	}
	if err := RegisterAutoTable[int32, tied](NewRegistry()); err == nil || !strings.Contains(err.Error(), "tie on json name") || !strings.Contains(err.Error(), "carries a cfg tag") {
		t.Fatalf("tied embedded fields with a cfg tag = %v", err)
	}
	// 镜像顺序：带标签的嵌入字段后到，走的是"后来者带标签"那条检查。
	type tiedLater struct {
		right
		left
		Key int32 `json:"key" cfg:"key"`
	}
	if err := RegisterAutoTable[int32, tiedLater](NewRegistry()); err == nil || !strings.Contains(err.Error(), "tie on json name") || !strings.Contains(err.Error(), "carries a cfg tag") {
		t.Fatalf("tied embedded fields with the tagged one second = %v", err)
	}
	type skipEmptyOnly struct {
		ID   int32  `json:"id" cfg:"key"`
		Name string `json:"name" cfg:"skipempty"`
	}
	if err := RegisterAutoTable[int32, skipEmptyOnly](NewRegistry()); err == nil || !strings.Contains(err.Error(), "skipempty requires index") {
		t.Fatalf("skipempty without index = %v", err)
	}
	type indexed struct {
		ID   int32  `json:"id" cfg:"key"`
		Name string `json:"name" cfg:"index=name,skipempty"`
	}
	if err := RegisterAutoTable[int32, indexed](NewRegistry()); err != nil {
		t.Fatalf("skipempty with index refused: %v", err)
	}
}

type promiseRow struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// Two rows with the same key used to be the T-35 class of defect elsewhere:
// the later row wins silently. Here the table constructor refuses, and the
// load must surface the table name and the key.
func TestTableLoadRefusesDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rows.json"), `[{"id":1,"name":"a"},{"id":1,"name":"b"}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, promiseRow]{Name: "rows", File: "rows.json", Key: func(v promiseRow) int32 { return v.ID }})
	_, err := NewStore(reg, dir).Load(context.Background())
	expectErr(t, err, "table rows duplicate key 1")
}

// A definition missing its identity or its key function must fail at load
// with the definition named; these used to be reachable only by hand-built
// defs and had no test.
func TestDefinitionsRefuseIncompleteShapes(t *testing.T) {
	ctx := &BuildContext{Dir: t.TempDir()}
	_, err := TableDef[int32, promiseRow]{File: "x.json", Key: func(v promiseRow) int32 { return v.ID }}.load(ctx)
	expectErr(t, err, "table name is empty")
	_, err = TableDef[int32, promiseRow]{Name: "rows", Key: func(v promiseRow) int32 { return v.ID }}.load(ctx)
	expectErr(t, err, "table rows file is empty")
	_, err = TableDef[int32, promiseRow]{Name: "rows", File: "x.json"}.load(ctx)
	expectErr(t, err, "table rows key func is nil")
	_, err = ObjectDef[promiseRow]{File: "x.json"}.load(ctx)
	expectErr(t, err, "object name is empty")
	_, err = ObjectDef[promiseRow]{Name: "world"}.load(ctx)
	expectErr(t, err, "object world file is empty")
	_, err = CustomDef[int]{Build: func(*BuildContext) (int, error) { return 0, nil }}.build(ctx)
	expectErr(t, err, "custom name is empty")
	_, err = CustomDef[int]{Name: "count"}.build(ctx)
	expectErr(t, err, "custom count build func is nil")

	// validate() is handed whatever load() produced; a wrong type is a
	// programming error that must not be swallowed as "nothing to validate".
	tableDef := TableDef[int32, promiseRow]{Name: "rows", Validate: func(*BuildContext, promiseRow) error { return nil }}
	expectErr(t, tableDef.validate(ctx, "not a table"), "table rows type mismatch")
	objectDef := ObjectDef[promiseRow]{Name: "world", Validate: func(*BuildContext, promiseRow) error { return nil }}
	expectErr(t, objectDef.validate(ctx, 42), "object world type mismatch")
	customDef := CustomDef[int]{Name: "count", Validate: func(*BuildContext, int) error { return nil }}
	expectErr(t, customDef.validate(ctx, "str"), "custom count type mismatch")

	reg := NewRegistry()
	expectErr(t, reg.RegisterTable(nil), "nil table def")
	expectErr(t, reg.RegisterObject(nil), "nil object def")
	expectErr(t, reg.RegisterCustom(nil), "nil custom def")
	expectErr(t, RegisterTable(reg, TableDef[int32, promiseRow]{File: "x.json"}), "table name is empty")
}

// Every auto-table tag rule, pinned by its message. The earlier test only
// checked that *some* error came back, so a rule could vanish while a
// neighbour kept the test green.
func TestRegisterAutoTableRefusesEachTagMistakeByMessage(t *testing.T) {
	type promoted struct {
		ID int32 `json:"id" cfg:"key"`
	}
	type taggedEmbed struct {
		promoted `cfg:"key"`
	}
	type namedEmbed struct {
		promoted `json:"inner"`
	}
	type pointerEmbed struct {
		*promoted
	}
	// No json tags here on purpose: vet's structtag check rejects the
	// duplicate-name shapes we need, and encoding/json resolves untagged
	// fields by their Go name just the same.
	type promotedByName struct {
		ID int32 `cfg:"key"`
	}
	type shadowedTag struct {
		promotedByName
		ID int32
	}
	type base1 struct {
		ID int32 `cfg:"key"`
	}
	type base2 struct {
		ID int32
	}
	type tieDropped struct {
		base1
		base2
		Key int32 `json:"key" cfg:"key"`
	}
	type unexportedTag struct {
		ID     int32 `json:"id" cfg:"key"`
		hidden int32 `cfg:"index"`
	}
	type emptyRef struct {
		ID  int32 `json:"id" cfg:"key"`
		Ref int32 `json:"ref" cfg:"ref="`
	}
	type emptyIndex struct {
		ID int32 `json:"id" cfg:"key,index="`
	}
	type doubleRef struct {
		ID  int32 `json:"id" cfg:"key"`
		Ref int32 `json:"ref" cfg:"ref=a,ref=b"`
	}
	type sliceRef struct {
		ID  int32   `json:"id" cfg:"key"`
		Ref []int32 `json:"ref" cfg:"ref=a"`
	}
	type stringKey struct {
		ID string `json:"id" cfg:"key"`
	}
	cases := []struct {
		name string
		reg  func(*Registry) error
		want string
	}{
		{"cfg tag on embedded field", func(r *Registry) error { return RegisterAutoTable[int32, taggedEmbed](r) }, "cfg tag on embedded field promoted is not supported"},
		{"json-named embed hides inner cfg tags", func(r *Registry) error { return RegisterAutoTable[int32, namedEmbed](r) }, "not promoted by encoding/json"},
		{"pointer embed with cfg tags", func(r *Registry) error { return RegisterAutoTable[int32, pointerEmbed](r) }, "cfg tags inside pointer embed promoted are not supported"},
		{"shadowed cfg field", func(r *Registry) error { return RegisterAutoTable[int32, shadowedTag](r) }, "is shadowed by"},
		{"tie-dropped cfg field", func(r *Registry) error { return RegisterAutoTable[int32, tieDropped](r) }, "tie on json name"},
		{"value type is not a struct", func(r *Registry) error { return RegisterAutoTable[int32, int](r) }, "must be a struct"},
		{"key type is not scalar", func(r *Registry) error { return RegisterAutoTable[[2]int32, promoted](r) }, "must be an integer or string"},
		{"cfg tag on unexported field", func(r *Registry) error { return RegisterAutoTable[int32, unexportedTag](r) }, "cfg tag on unexported field hidden"},
		{"empty ref target", func(r *Registry) error { return RegisterAutoTable[int32, emptyRef](r) }, "empty ref target"},
		{"empty index name", func(r *Registry) error { return RegisterAutoTable[int32, emptyIndex](r) }, "empty index name"},
		{"two ref directives", func(r *Registry) error { return RegisterAutoTable[int32, doubleRef](r) }, "multiple ref directives"},
		{"ref on a slice", func(r *Registry) error { return RegisterAutoTable[int32, sliceRef](r) }, "must be an integer or string"},
		{"validate callback of the wrong type", func(r *Registry) error {
			return RegisterAutoTable[int32, promoted](r, WithAutoValidate(func(*BuildContext, stringKey) error { return nil }))
		}, "WithAutoValidate callback must be func(*BuildContext, configdata.promoted) error"},
		{"validate-table callback of the wrong type", func(r *Registry) error {
			return RegisterAutoTable[int32, promoted](r, WithAutoValidateTable(func(*BuildContext, *Table[string, stringKey]) error { return nil }))
		}, "WithAutoValidateTable callback signature mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectErr(t, tc.reg(NewRegistry()), tc.want)
		})
	}
	if err := RegisterAutoTable[int32, promoted](NewRegistry()); err != nil {
		t.Fatalf("the plain shape must register: %v", err)
	}
}

func TestEmbeddedRefPositiveAndShadowingRejected(t *testing.T) {
	// Positive: promoted ref passes with a valid value.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "drop.json"), `[{"id":100,"bonus":1}]`)
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","drop_id":100}]`)
	reg := NewRegistry()
	MustRegisterAutoTable[int32, autoDropCfg](reg, WithAutoName("drop"))
	MustRegisterAutoTable[int32, embeddedMonsterCfg](reg, WithAutoName("monster"), WithAutoFile("monster.json"))
	if _, err := NewStore(reg, dir).Load(context.Background()); err != nil {
		t.Fatalf("valid embedded ref rejected: %v", err)
	}

	// Outer field shadows an embedded cfg-tagged field: must be a
	// registration error, never a silently zero key.
	type keyBase struct {
		ID int32 `json:"id" cfg:"key"`
	}
	type shadowed struct {
		keyBase
		ID int32 `json:"id"`
	}
	if err := RegisterAutoTable[int32, shadowed](NewRegistry()); err == nil || !strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("shadowed cfg key accepted: %v", err)
	}

	// Embedded field with a json name is NOT promoted by encoding/json —
	// cfg tags inside it must be rejected.
	type namedEmbed struct {
		keyBase `json:"base"`
		Name    string `json:"name"`
	}
	if err := RegisterAutoTable[int32, namedEmbed](NewRegistry()); err == nil || !strings.Contains(err.Error(), "json name") {
		t.Fatalf("json-named embed with cfg tags accepted: %v", err)
	}
}

func TestIndexNameContainingRequiredIsAccepted(t *testing.T) {
	// Regression for the substring parsing bug: "required" inside an index
	// name must not be treated as the required directive.
	type levels struct {
		ID    int32 `json:"id" cfg:"key"`
		Level int32 `json:"level" cfg:"index=required_level"`
	}
	if err := RegisterAutoTable[int32, levels](NewRegistry()); err != nil {
		t.Fatalf("index=required_level rejected: %v", err)
	}
	type twoRefs struct {
		ID int32 `json:"id" cfg:"key"`
		A  int32 `json:"a" cfg:"ref=x,ref=y"`
	}
	if err := RegisterAutoTable[int32, twoRefs](NewRegistry()); err == nil {
		t.Fatal("multiple ref directives accepted")
	}
}
