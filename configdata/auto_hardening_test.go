package configdata

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := RegisterAutoTable[int32, orphanRequired](reg); err == nil {
		t.Fatal("required without ref accepted")
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
	if _, err := store.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "required reference") {
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
